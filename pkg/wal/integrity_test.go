package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The log had no checksums and no bound on its length prefix. Two consequences,
// both silent:
//
//   - A record altered on disk but still parseable as JSON — a flipped digit inside
//     a price — was replayed as truth. The recovered book was wrong and nothing
//     anywhere said so.
//   - The 4-byte length prefix was read off disk and handed straight to make(),
//     so one flipped bit in it asked for an allocation of up to 4 GiB during
//     recovery.
//
// Both are fixed; these tests are the proof, and they are written to fail loudly if
// either regresses.

func writeLog(t *testing.T, path string, n int) {
	t.Helper()
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !w.Checksummed() {
		t.Fatal("a fresh log is not checksummed")
	}
	for i := 0; i < n; i++ {
		o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, int64(100+i), 10, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		if _, err := w.AppendSubmit(o); err != nil {
			t.Fatalf("AppendSubmit: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestNewLogCarriesHeaderAndChecksums — the format marker has to be on disk, or a
// reader cannot tell a checksummed log from a v1 one.
//
// The marker a fresh log carries is now the full segment header rather than the bare
// six-byte Magic, and the assertion got stronger rather than weaker: the header
// declares the base sequence and the MATCHING SEMANTICS its records were produced
// under, and both are covered by a CRC. A log that says nothing about which matcher
// wrote it is the condition docs/SEMANTICS-VERSION.md exists to end, so a build that
// went back to writing six bytes here would be reintroducing it.
func TestNewLogCarriesHeaderAndChecksums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	writeLog(t, path, 5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) < SegHeaderBytesV3 || string(raw[:len(SegMagicV3)]) != SegMagicV3 {
		t.Fatalf("log does not begin with the segment header; first bytes %q", raw[:min(len(raw), 8)])
	}
	if got := int(binary.BigEndian.Uint64(raw[len(SegMagicV3) : len(SegMagicV3)+8])); got != 1 {
		t.Errorf("a fresh log declares base sequence %d, want 1", got)
	}
	if got := int(binary.BigEndian.Uint32(raw[len(SegMagicV3)+8 : len(SegMagicV3)+12])); got != matching.SemanticsVersion {
		t.Errorf("a fresh log declares semantics %d, want %d", got, matching.SemanticsVersion)
	}

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("read %d entries, want 5", len(entries))
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			t.Errorf("entry %d has seq %d", i, e.Seq)
		}
	}
}

// TestAlteredRecordIsRefused is the whole point. Before checksums this returned the
// corrupted entry with no error, and the venue booked it.
func TestAlteredRecordIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	writeLog(t, path, 5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Flip a byte inside the third record's payload. Find a digit well past the
	// header so the mutation lands in JSON rather than in framing.
	idx := -1
	for i := recordsBegin(t, raw) + 40; i < len(raw)-1; i++ {
		if raw[i] >= '1' && raw[i] <= '8' {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("found no digit to corrupt")
	}
	raw[idx]++ // still valid JSON, different value — exactly the dangerous case
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := ReadAll(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt — an altered record was accepted as truth", err)
	}
	// Entries before the damage are still returned, so an operator can see how far
	// the log was good.
	if len(entries) >= 5 {
		t.Errorf("returned %d entries; the corrupt one and everything after it must not be included", len(entries))
	}
}

// TestCorruptLengthPrefixIsRefusedWithoutAllocating — a flipped bit in the length
// must not turn recovery into a 4 GiB allocation.
func TestCorruptLengthPrefixIsRefusedWithoutAllocating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	writeLog(t, path, 3)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The first record's length prefix sits immediately after the header. Claim
	// nearly 4 GiB.
	binary.BigEndian.PutUint32(raw[recordsBegin(t, raw):recordsBegin(t, raw)+4], 0xFFFFFFF0)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := ReadAll(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll err = %v, want ErrCorrupt", err)
	}
	if len(entries) != 0 {
		t.Errorf("returned %d entries from a log whose first record is unreadable", len(entries))
	}
}

// TestZeroLengthRecordIsRefused — a zero length would otherwise loop forever
// producing empty reads.
func TestZeroLengthRecordIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	writeLog(t, path, 2)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	binary.BigEndian.PutUint32(raw[recordsBegin(t, raw):recordsBegin(t, raw)+4], 0)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadAll(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// TestTornTailIsNotCorruption keeps the distinction that matters. A crash mid-write
// is expected and must recover cleanly; only a COMPLETE record that fails its
// checksum is corruption. Conflating them would make every crash look like data
// loss, or worse, make data loss look like a crash.
func TestTornTailIsNotCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	writeLog(t, path, 5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, cut := range []int{1, 5, 17} { // mid-payload, mid-checksum, mid-length
		truncated := raw[:len(raw)-cut]
		p := filepath.Join(t.TempDir(), "torn.log")
		if err := os.WriteFile(p, truncated, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		entries, err := ReadAll(p)
		if err != nil {
			t.Errorf("truncated by %d bytes: err = %v, want a clean stop", cut, err)
		}
		if len(entries) == 0 {
			t.Errorf("truncated by %d bytes: lost every record, want the complete ones", cut)
		}
		if len(entries) > 5 {
			t.Errorf("truncated by %d bytes: returned %d entries", cut, len(entries))
		}
	}
}

// TestLegacyLogStillRecovers — the log format predates checksums by a release. An
// upgrade must not strand an existing file, so a headerless log is read without
// verification rather than refused.
func TestLegacyLogStillRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.log")

	// Build a v1 log by hand: [len][payload], no header, no CRC.
	modern := filepath.Join(dir, "modern.log")
	writeLog(t, modern, 4)
	raw, err := os.ReadFile(modern)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	body := raw[recordsBegin(t, raw):]
	var legacy []byte
	for len(body) > 0 {
		n := binary.BigEndian.Uint32(body[0:4])
		legacy = append(legacy, body[0:4]...)        // length
		legacy = append(legacy, body[8:8+int(n)]...) // payload, dropping the CRC
		body = body[8+int(n):]
	}
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatalf("a v1 log must still be readable: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("read %d entries from the v1 log, want 4", len(entries))
	}

	// Appending must keep the v1 file's own framing, not switch mid-file to a format
	// no reader could then parse.
	//
	// This half used to assert that by checking !w.Checksummed() after Open, which was
	// a PROXY for the property. The proxy stopped matching when Open learned to seal a
	// segment declaring somebody else's matching semantics (docs/SEMANTICS-VERSION.md
	// Rule 17): a v1 file declares none, so it is migrated into the set and the append
	// goes to a new segment beside it. The property is preserved and improved —
	// framing still never changes mid-file, and now it CANNOT — so the assertions
	// below are strictly stronger than the one they replace. The v1 bytes are asserted
	// BYTE-IDENTICAL across the append, which the old test could not say at all.
	before := readFile(t, path)
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	if !w.Checksummed() {
		t.Error("the appended-to segment is not checksummed; sealing the v1 file is what gets checksums onto it")
	}
	o, err := types.NewOrder("u", "X", types.SideBuy, types.OrderTypeLimit, 500, 1, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if _, err := w.AppendSubmit(o); err != nil {
		t.Fatalf("AppendSubmit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The v1 file itself is untouched — every byte of it, in place. It moved into the
	// set as segment 1, which is where the first rotation would have put it anyway.
	migrated := filepath.Join(dir, "legacy.log.0000000000000001")
	if after := readFile(t, migrated); !bytes.Equal(before, after) {
		t.Errorf("the v1 segment changed under an append: %d bytes before, %d after", len(before), len(after))
	}
	// And the appended record is NOT in it.
	if v1Entries := framesIn(t, migrated, false); len(v1Entries) != 4 {
		t.Errorf("the v1 segment holds %d records after the append, want the 4 it started with", len(v1Entries))
	}

	entries, err = ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll after append: %v", err)
	}
	if len(entries) != 5 {
		t.Errorf("read %d entries, want 5", len(entries))
	}
	if entries[4].Seq != 5 {
		t.Errorf("appended entry has seq %d, want 5", entries[4].Seq)
	}
}

// framesIn counts the complete records in a file, walking its framing directly so it
// cannot be fooled by a reader that has learned to skip something.
func framesIn(tb testing.TB, path string, checksummed bool) []int {
	tb.Helper()
	raw := readFile(tb, path)
	off := 0
	if checksummed {
		off = recordsBegin(tb, raw)
	}
	var out []int
	frame := 4
	if checksummed {
		frame = 8
	}
	for off+frame <= len(raw) {
		n := int(binary.BigEndian.Uint32(raw[off : off+4]))
		if n <= 0 || off+frame+n > len(raw) {
			break
		}
		out = append(out, off)
		off += frame + n
	}
	return out
}

// TestRecoverSurfacesCorruption — the error has to reach the caller. A venue must
// refuse to start rather than serve a book that does not match its log.
func TestRecoverSurfacesCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	writeLog(t, path, 4)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	binary.BigEndian.PutUint32(raw[recordsBegin(t, raw):recordsBegin(t, raw)+4], 0xFFFFFFF0)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Recover(mutCfg(), "", path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Recover err = %v, want ErrCorrupt — a corrupt log started a venue", err)
	}
	// And Open refuses too, since it reads the log to continue the sequence.
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open err = %v, want ErrCorrupt", err)
	}
}
