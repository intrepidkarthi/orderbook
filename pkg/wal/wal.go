// Package wal provides durable, append-only write-ahead logging and snapshot
// persistence for the matching engine — the crash-recovery storage backend.
//
// It records the ordered *command* stream to disk — every command that mutates
// the book: submits, cancels, reduces, the operator's account-wide cancel, and the
// conditional order types (stop, OCO, iceberg, pegged, trailing). A fresh engine
// replays the log — optionally starting from a snapshot — to reach identical book state, the same recovery contract LMAX (journal +
// snapshot + replay) and Binance (hourly snapshot + sequential replay) rely on.
// Recovery is bounded to O(recent) by snapshotting and replaying only the WAL tail
// after the snapshot's sequence.
//
// Records are length-prefixed, CRC-32C-checksummed JSON, written write-ahead
// (before the engine applies the command) so no acknowledged command is lost.
//
// The two failure modes are treated differently on purpose. A crash mid-write
// leaves a torn tail — a short final record — which the reader stops at cleanly,
// because that record was never acknowledged. A record that is complete but fails
// its checksum is media corruption: the bytes changed after they were written, and
// recovery refuses rather than stopping, since a quiet stop there is
// indistinguishable from a clean end of log while silently discarding every
// command after it.
package wal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// EntryKind is the type of a logged command.
type EntryKind uint8

const (
	KindSubmit    EntryKind = iota + 1 // a Process(order)
	KindCancel                         // a Cancel(id, user)
	KindReduce                         // a Reduce(id, newQty, user)
	KindCancelAll                      // a CancelAllForUser(user)
	KindStop                           // a ProcessStop(stop)
	KindOCO                            // a ProcessOCO(oco)
	KindIceberg                        // a ProcessIceberg(iceberg)
	KindPegged                         // a ProcessPegged(pegged)
	KindTrailing                       // a ProcessTrailingStop(trailing)
	KindReplace                        // a Replace(id, user, replacement)
)

// Entry is one durable command-log record.
//
// CancelID names the target order for both KindCancel and KindReduce — it is the
// engine order id in either case, and giving the reduce its own field would only
// invite the two drifting apart.
type Entry struct {
	Seq      int64        `json:"seq"`
	Kind     EntryKind    `json:"kind"`
	Order    *types.Order `json:"order,omitempty"`
	CancelID int64        `json:"cancel_id,omitempty"`
	UserID   string       `json:"user_id,omitempty"`
	// NewQty is the new TOTAL quantity of a KindReduce, matching Engine.Reduce.
	// A delta would not survive replay: the same delta applied to a differently
	// filled order yields a different size.
	NewQty int64 `json:"new_qty,omitempty"`

	// Conditional-order parameters. Each is meaningful for exactly one Kind; the
	// alternative — an Entry per order type — would duplicate the Order field five
	// times for no gain, since the wrapper types are a base order plus one or two
	// scalars.
	StopPrice  int64        `json:"stop_price,omitempty"`  // KindStop, KindOCO
	StopOrder  *types.Order `json:"stop_order,omitempty"`  // KindOCO: the stop leg
	DisplayQty int64        `json:"display_qty,omitempty"` // KindIceberg
	PegRef     string       `json:"peg_ref,omitempty"`     // KindPegged
	PegOffset  int64        `json:"peg_offset,omitempty"`  // KindPegged
	Trail      int64        `json:"trail,omitempty"`       // KindTrailing
}

// Header, record framing and the bounds that make a corrupt file safe to read.
//
// A file written by this package begins with Magic. Records after it are
// [4-byte length][4-byte CRC-32C of the payload][payload]. A file with no header
// is a v1 log — written before checksums existed — and is read without them; see
// ReadAll.
const (
	// Magic identifies the format and carries its version in the final byte, so a
	// future framing change is detectable rather than misread.
	Magic = "OBWAL\x01"

	// MaxRecordBytes bounds a record payload. The length prefix is four bytes read
	// off disk, so without a ceiling a single flipped bit in it asks for an
	// allocation of up to 4 GiB during recovery — the moment a venue can least
	// afford one. A command record is a few hundred bytes; 8 MiB is far beyond any
	// legitimate one and still small enough to refuse safely.
	MaxRecordBytes = 8 << 20
)

// ErrCorrupt reports a record that is present and complete but whose bytes do not
// match their checksum, or whose declared length is impossible.
//
// This is deliberately distinct from a torn tail. A crash mid-write leaves a short
// final record, which ReadAll stops at cleanly because the read comes up short —
// no checksum is involved. A COMPLETE record whose CRC disagrees means the bytes on
// disk were altered after they were written, and silently truncating there would
// discard acknowledged commands while looking like an ordinary clean stop. Recovery
// refuses instead: serving a book that does not match the log is worse than not
// starting.
var ErrCorrupt = errors.New("wal: corrupt record")

// crcTable is Castagnoli, which has hardware support on amd64 and arm64.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Writer is an append-only, durable command log. It is safe for concurrent use,
// but write it write-ahead — append (and Sync) before the engine applies the
// command — so a crash never loses an acknowledged order. Batch Sync (group
// commit) to amortise the fsync across many appends.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	w   *bufio.Writer
	seq int64
	// checksummed is false when appending to a headerless v1 file. Records keep
	// that file's framing rather than switching mid-file, which would leave a log
	// no reader could parse. Rotate to get checksums on an old file.
	checksummed bool
}

// Open opens (creating if needed) a WAL file for appending, recovering the last
// sequence number so new entries continue monotonically.
//
// A new or empty file gets the header and checksummed records. An existing file
// keeps whichever framing it already has: appending checksummed records to a
// headerless log would produce a file that is neither format.
func Open(path string) (*Writer, error) {
	checksummed, err := hasHeader(path)
	if err != nil {
		return nil, err
	}
	last, err := lastSeq(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, w: bufio.NewWriter(f), seq: last, checksummed: checksummed}
	if checksummed {
		// Only a brand-new file needs the header written; hasHeader reports true for
		// an empty file precisely so this happens once.
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if st.Size() == 0 {
			if _, err := w.w.WriteString(Magic); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
	}
	return w, nil
}

// hasHeader reports whether new records in path should carry checksums: true for a
// missing or empty file (a fresh log) and for one already carrying the header,
// false for a headerless v1 log.
func hasHeader(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil // a log that does not exist yet is written in the current format
		}
		return false, err
	}
	defer f.Close()
	buf := make([]byte, len(Magic))
	n, err := io.ReadFull(f, buf)
	if err == io.EOF || (err == io.ErrUnexpectedEOF && n == 0) {
		return true, nil // empty file
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return n == len(Magic) && string(buf) == Magic, nil
}

// Checksummed reports whether records this writer appends carry a CRC. It is false
// only when appending to a log written before checksums existed.
func (w *Writer) Checksummed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checksummed
}

func (w *Writer) append(e Entry) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	e.Seq = w.seq
	b, err := json.Marshal(e)
	if err != nil {
		w.seq--
		return 0, err
	}
	if len(b) > MaxRecordBytes {
		w.seq--
		return 0, fmt.Errorf("%w: record of %d bytes exceeds the %d-byte maximum", ErrCorrupt, len(b), MaxRecordBytes)
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(b)))
	n := 4
	if w.checksummed {
		binary.BigEndian.PutUint32(hdr[4:8], crc32.Checksum(b, crcTable))
		n = 8
	}
	if _, err := w.w.Write(hdr[:n]); err != nil {
		return 0, err
	}
	if _, err := w.w.Write(b); err != nil {
		return 0, err
	}
	return w.seq, nil
}

// AppendSubmit logs a Process(order). Call before submitting so the record
// captures the order as-submitted.
func (w *Writer) AppendSubmit(o *types.Order) (int64, error) {
	return w.append(Entry{Kind: KindSubmit, Order: o})
}

// AppendCancel logs a Cancel(id, user).
func (w *Writer) AppendCancel(orderID int64, userID string) (int64, error) {
	return w.append(Entry{Kind: KindCancel, CancelID: orderID, UserID: userID})
}

// AppendReduce logs a Reduce(id, newQty, user). newQty is the new total quantity.
//
// A reduce mutates the book — it is not a control command the snapshot happens to
// cover — so leaving it out of the log meant a restart resurrected the order at
// its original size, silently and with no error anywhere.
func (w *Writer) AppendReduce(orderID, newQty int64, userID string) (int64, error) {
	return w.append(Entry{Kind: KindReduce, CancelID: orderID, NewQty: newQty, UserID: userID})
}

// AppendCancelAll logs a CancelAllForUser(user) — the operator kill switch.
//
// It records the intent rather than the orders it removed, which is what makes
// replay correct: the log is written before the sweep, so at replay time the same
// point in the command stream holds the same book and the sweep removes exactly
// the same set. Logging the resulting ids instead would be logging an outcome and
// hoping it still applies.
func (w *Writer) AppendCancelAll(userID string) (int64, error) {
	return w.append(Entry{Kind: KindCancelAll, UserID: userID})
}

// AppendReplace logs a Replace. CancelID names the order being replaced and Order
// carries the replacement, so replay reproduces both halves as one step rather than
// as a cancel that might succeed followed by an entry that might not.
func (w *Writer) AppendReplace(orderID int64, userID string, replacement *types.Order) (int64, error) {
	return w.append(Entry{Kind: KindReplace, CancelID: orderID, UserID: userID, Order: replacement})
}

// AppendStop logs a ProcessStop. The trigger price is recorded alongside the order,
// because replaying the order without it would rest a plain order that never fires.
func (w *Writer) AppendStop(s *types.StopOrder) (int64, error) {
	if s == nil {
		return w.Seq(), nil
	}
	return w.append(Entry{Kind: KindStop, Order: s.Order, StopPrice: s.StopPrice})
}

// AppendOCO logs a ProcessOCO. Both legs are recorded: replaying only the primary
// would leave a position with no stop behind it, which is the opposite of what the
// client asked for.
func (w *Writer) AppendOCO(o *types.OCOOrder) (int64, error) {
	if o == nil || o.Stop == nil {
		return w.Seq(), nil
	}
	return w.append(Entry{Kind: KindOCO, Order: o.Primary, StopOrder: o.Stop.Order, StopPrice: o.Stop.StopPrice})
}

// AppendIceberg logs a ProcessIceberg. Order.Quantity is the total and DisplayQty the
// slice; without the slice size a replay would show the whole reserve.
func (w *Writer) AppendIceberg(ib *types.IcebergOrder) (int64, error) {
	if ib == nil {
		return w.Seq(), nil
	}
	return w.append(Entry{Kind: KindIceberg, Order: ib.Order, DisplayQty: ib.DisplayQty})
}

// AppendPegged logs a ProcessPegged.
func (w *Writer) AppendPegged(p *types.PeggedOrder) (int64, error) {
	if p == nil {
		return w.Seq(), nil
	}
	return w.append(Entry{Kind: KindPegged, Order: p.Order, PegRef: string(p.Ref), PegOffset: p.Offset})
}

// AppendTrailing logs a ProcessTrailingStop.
func (w *Writer) AppendTrailing(ts *types.TrailingStop) (int64, error) {
	if ts == nil {
		return w.Seq(), nil
	}
	return w.append(Entry{Kind: KindTrailing, Order: ts.Order, Trail: ts.Trail})
}

// Sync flushes buffered records and fsyncs the file — the durability point. Call
// it before acknowledging the commands since the last Sync (group commit).
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

// Seq returns the last written sequence number.
func (w *Writer) Seq() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// Close syncs and closes the WAL file.
func (w *Writer) Close() error {
	if err := w.Sync(); err != nil {
		return err
	}
	return w.f.Close()
}

// ReadAll reads every complete entry from a WAL file, in order, stopping cleanly
// at a torn tail (a partial record left by a crash mid-write). A missing file
// yields no entries.
func ReadAll(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)

	// Detect the framing. A header means every record carries a CRC; its absence
	// means a v1 log, which is read without checksums so an upgrade does not lose
	// the ability to recover — those records simply cannot be verified.
	checksummed := false
	if hdr, err := r.Peek(len(Magic)); err == nil && string(hdr) == Magic {
		if _, err := r.Discard(len(Magic)); err != nil {
			return nil, err
		}
		checksummed = true
	}

	var out []Entry
	for {
		var lenbuf [4]byte
		if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
			break // clean EOF or torn length prefix — stop at the last complete record
		}
		n := binary.BigEndian.Uint32(lenbuf[:])
		// Bound before allocating: this length came off disk and a flipped bit in it
		// must not turn recovery into a multi-gigabyte allocation.
		if n == 0 || n > MaxRecordBytes {
			return out, fmt.Errorf("%w: record %d declares %d bytes (limit %d)", ErrCorrupt, len(out)+1, n, MaxRecordBytes)
		}

		var want uint32
		if checksummed {
			var crcbuf [4]byte
			if _, err := io.ReadFull(r, crcbuf[:]); err != nil {
				break // torn checksum — the record was never completely written
			}
			want = binary.BigEndian.Uint32(crcbuf[:])
		}

		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			break // torn record body
		}
		if checksummed {
			if got := crc32.Checksum(buf, crcTable); got != want {
				// Complete on disk and altered since it was written. Stopping quietly
				// here would look identical to a clean end of log while discarding
				// everything after it.
				return out, fmt.Errorf("%w: record %d checksum %08x, want %08x", ErrCorrupt, len(out)+1, got, want)
			}
		}
		var e Entry
		if err := json.Unmarshal(buf, &e); err != nil {
			if checksummed {
				// The checksum passed, so these are the bytes that were written and
				// they are not a record. That is a bug or a format mismatch, not
				// media corruption, and it must not be swallowed.
				return out, fmt.Errorf("%w: record %d passed its checksum but does not decode: %v", ErrCorrupt, len(out)+1, err)
			}
			break // v1 log: unverifiable, so an undecodable record is treated as the tail
		}
		out = append(out, e)
	}
	return out, nil
}

func lastSeq(path string) (int64, error) {
	entries, err := ReadAll(path)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	return entries[len(entries)-1].Seq, nil
}

// Restore replays every entry into an engine (in log order), reproducing the
// recorded command stream. Equivalent to RestoreAfter(eng, entries, 0); use it
// when recovering from an empty engine with no snapshot.
func Restore(eng *matching.Engine, entries []Entry) {
	RestoreAfter(eng, entries, 0)
}

// RestoreAfter replays only the entries whose sequence is greater than afterSeq,
// which is how a snapshot and its log tail are joined: pass the snapshot's
// WALSeq. Passing 0 replays everything.
//
// Getting this boundary wrong is silent in both directions. Too low and the
// commands already folded into the snapshot are applied a second time, which
// double-books orders and corrupts the recovered state; too high and accepted
// commands are dropped. Neither produces an error — just a different book.
//
// Orders are replayed fresh so the engine reassigns ids deterministically — the
// id recorded by a cancel or a reduce therefore matches the replayed order.
// Cancels for already-gone orders are ignored (idempotent under redelivery).
//
// Replay runs with the engine in replay mode (SetReplaying) so its live-ingress
// admission controls — minimum resting time and the per-order size caps — do not
// re-litigate commands the log already recorded as accepted; re-checking them
// against replay-time timestamps would wrongly reject an accepted cancel and
// diverge the recovered book. The deterministic matching itself is unchanged.
func RestoreAfter(eng *matching.Engine, entries []Entry, afterSeq int64) {
	eng.SetReplaying(true)
	defer eng.SetReplaying(false)
	for _, e := range entries {
		if e.Seq <= afterSeq {
			continue
		}
		switch e.Kind {
		case KindSubmit:
			if e.Order != nil {
				eng.Process(e.Order.Fresh())
			}
		case KindCancel:
			_, _ = eng.Cancel(e.CancelID, e.UserID)
		case KindReduce:
			// Refusals are ignored for the same reason a cancel's are: the log is
			// written write-ahead, so a command the engine went on to refuse is on
			// disk too, and replay must refuse it identically rather than treat it
			// as corruption.
			_, _ = eng.Reduce(e.CancelID, e.NewQty, e.UserID)
		case KindCancelAll:
			eng.CancelAllForUser(e.UserID)
		case KindReplace:
			// Refusals are ignored for the same reason a cancel's are: the log records
			// what was attempted, and replay must reach the same outcome by attempting
			// the same thing.
			_, _ = eng.Replace(e.CancelID, e.UserID, e.Order.Fresh())
		case KindStop:
			if s, err := types.NewStopOrder(e.Order.Fresh(), e.StopPrice); err == nil {
				eng.ProcessStop(s)
			}
		case KindOCO:
			if e.StopOrder == nil {
				continue
			}
			s, err := types.NewStopOrder(e.StopOrder.Fresh(), e.StopPrice)
			if err != nil {
				continue
			}
			if o, err := types.NewOCOOrder(e.Order.Fresh(), s); err == nil {
				eng.ProcessOCO(o)
			}
		case KindIceberg:
			if ib, err := types.NewIcebergOrder(e.Order.Fresh(), e.DisplayQty); err == nil {
				eng.ProcessIceberg(ib)
			}
		case KindPegged:
			if p, err := types.NewPeggedOrder(e.Order.Fresh(), types.PegReference(e.PegRef), e.PegOffset); err == nil {
				eng.ProcessPegged(p)
			}
		case KindTrailing:
			if ts, err := types.NewTrailingStop(e.Order.Fresh(), e.Trail); err == nil {
				eng.ProcessTrailingStop(ts)
			}
		}
	}
}

// WriteSnapshot writes a snapshot to path durably: write to a temp file, fsync
// it, rename over the target, then fsync the parent directory.
//
// The rename alone is atomic but not durable. Without the first fsync the file's
// contents may still be in the page cache when the rename lands, so a crash can
// leave a snapshot that exists, has the right name and is empty or truncated —
// and Recover will load it. Without the second, the directory entry itself may
// not survive, leaving the target pointing at nothing. Both are the standard
// cost of a crash-atomic file swap.
//
// On any failure the temp file is removed rather than left beside the target,
// and the previous snapshot is untouched: a failed write never destroys the last
// good one.
func WriteSnapshot(path string, snap *matching.EngineSnapshot) (err error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory so a rename into it survives a crash. Opening a
// directory for read and syncing it is the portable POSIX form; platforms that
// refuse it report no error the caller can act on, so the failure is swallowed
// rather than failing a snapshot that is already on disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil //nolint:nilerr // the snapshot is written; directory durability is best-effort
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// Checkpoint takes a snapshot of eng, stamps it with the command-log sequence of
// the last command APPLIED to that engine, and writes it to path. Recovery then
// replays only the entries after lastAppliedSeq.
//
// lastAppliedSeq must be the sequence returned by the Append call for the last
// command the engine has actually processed — NOT Writer.Seq(). The log is
// written write-ahead, so entries can be on disk that the engine has not applied
// yet, and stamping the writer's latest sequence silently drops them at recovery.
// Call this from whichever goroutine applies commands, between commands, so no
// command can land in the gap.
func Checkpoint(path string, eng *matching.Engine, lastAppliedSeq int64) error {
	snap := eng.TakeSnapshot()
	snap.WALSeq = lastAppliedSeq
	return WriteSnapshot(path, snap)
}

// Recover rebuilds an engine from a snapshot plus the log tail after it: the
// standard bootstrap path, expressed once here so callers do not reimplement the
// sequence join and get the boundary wrong. A missing snapshot replays the whole
// log; a missing log yields the snapshot alone; neither present yields a fresh
// engine.
func Recover(config matching.Config, snapPath, walPath string) (*matching.Engine, error) {
	snap, err := ReadSnapshot(snapPath)
	if err != nil {
		return nil, err
	}
	entries, err := ReadAll(walPath)
	if err != nil {
		return nil, err
	}
	var eng *matching.Engine
	var after int64
	if snap != nil {
		if eng, err = matching.RestoreEngine(config, snap); err != nil {
			return nil, err
		}
		after = snap.WALSeq
	} else {
		eng = matching.NewEngine(config)
	}
	RestoreAfter(eng, entries, after)
	return eng, nil
}

// ReadSnapshot reads a snapshot from path; a missing file yields (nil, nil).
func ReadSnapshot(path string) (*matching.EngineSnapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s matching.EngineSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
