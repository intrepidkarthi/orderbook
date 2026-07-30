package wal

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
func TestNewLogCarriesHeaderAndChecksums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	writeLog(t, path, 5)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) < len(Magic) || string(raw[:len(Magic)]) != Magic {
		t.Fatalf("log does not begin with the magic header; first bytes %q", raw[:min(len(raw), 8)])
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
	for i := len(Magic) + 40; i < len(raw)-1; i++ {
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
	binary.BigEndian.PutUint32(raw[len(Magic):len(Magic)+4], 0xFFFFFFF0)
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
	binary.BigEndian.PutUint32(raw[len(Magic):len(Magic)+4], 0)
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
	body := raw[len(Magic):]
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

	// Appending must keep the file's own framing, not switch mid-file to a format
	// no reader could then parse.
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	if w.Checksummed() {
		t.Error("appending to a v1 log switched framing mid-file")
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
	binary.BigEndian.PutUint32(raw[len(Magic):len(Magic)+4], 0xFFFFFFF0)
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
