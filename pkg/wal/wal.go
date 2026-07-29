// Package wal provides durable, append-only write-ahead logging and snapshot
// persistence for the matching engine — the crash-recovery storage backend.
//
// It records the ordered *command* stream (submits and cancels) to disk. A fresh
// engine replays the log — optionally starting from a snapshot — to reach
// identical book state, the same recovery contract LMAX (journal + snapshot +
// replay) and Binance (hourly snapshot + sequential replay) rely on. Recovery is
// bounded to O(recent) by snapshotting and replaying only the WAL tail after the
// snapshot's sequence.
//
// Records are length-prefixed JSON, written write-ahead (before the engine
// applies the command) so no acknowledged command is lost. A crash mid-write
// leaves a torn tail, which the reader stops at cleanly.
package wal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
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
	KindSubmit EntryKind = iota + 1 // a Process(order)
	KindCancel                      // a Cancel(id, user)
)

// Entry is one durable command-log record.
type Entry struct {
	Seq      int64        `json:"seq"`
	Kind     EntryKind    `json:"kind"`
	Order    *types.Order `json:"order,omitempty"`
	CancelID int64        `json:"cancel_id,omitempty"`
	UserID   string       `json:"user_id,omitempty"`
}

// Writer is an append-only, durable command log. It is safe for concurrent use,
// but write it write-ahead — append (and Sync) before the engine applies the
// command — so a crash never loses an acknowledged order. Batch Sync (group
// commit) to amortise the fsync across many appends.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	w   *bufio.Writer
	seq int64
}

// Open opens (creating if needed) a WAL file for appending, recovering the last
// sequence number so new entries continue monotonically.
func Open(path string) (*Writer, error) {
	last, err := lastSeq(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, w: bufio.NewWriter(f), seq: last}, nil
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
	var lenbuf [4]byte
	binary.BigEndian.PutUint32(lenbuf[:], uint32(len(b)))
	if _, err := w.w.Write(lenbuf[:]); err != nil {
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
	var out []Entry
	for {
		var lenbuf [4]byte
		if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
			break // clean EOF or torn length prefix — stop at the last complete record
		}
		buf := make([]byte, binary.BigEndian.Uint32(lenbuf[:]))
		if _, err := io.ReadFull(r, buf); err != nil {
			break // torn record body
		}
		var e Entry
		if err := json.Unmarshal(buf, &e); err != nil {
			break // corrupt record — stop
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
// Orders are replayed fresh so the engine reassigns ids deterministically — a
// cancel's recorded id therefore matches the replayed order. Cancels for
// already-gone orders are ignored (idempotent under redelivery).
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
