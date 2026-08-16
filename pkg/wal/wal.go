// Package wal provides durable, append-only write-ahead logging and snapshot
// persistence for the matching engine — the crash-recovery storage backend.
//
// It records the ordered *command* stream to disk — every command that mutates
// the book: submits, cancels, reduces, the operator's account-wide cancel, and the
// conditional order types (stop, OCO, iceberg, pegged, trailing). A fresh engine
// replays the log — optionally starting from a snapshot — to reach identical book state, the same recovery contract LMAX (journal +
// snapshot + replay) and Binance (hourly snapshot + sequential replay) rely on.
// Snapshotting bounds the REPLAY to O(recent) — only the WAL tail after the
// snapshot's sequence is applied, at roughly 2µs per record. It does not bound the
// restart. A restart costs three terms: O(book) to load the snapshot (~174ms at a
// 100,000-order book), O(total log) to read and checksum-verify every record
// including the ones the snapshot already covers, and O(tail) to apply what is left
// (~21ms for 10,000 records). Recovery skips decoding and retaining the covered
// prefix, which makes its ALLOCATION flat in that prefix, but it still reads and
// verifies every byte of it — see Recover and walkLog — and nothing here rotates or
// truncates the file, so the middle term grows for as long as the venue runs. See
// docs/BENCHMARKS.md and docs/BOUNDED-RECOVERY.md.
//
// Records are length-prefixed, CRC-32C-checksummed JSON, written write-ahead:
// appended before the engine applies the command, so the log is never missing
// something the book did and recovery never produces a book that ran ahead of
// its own journal.
//
// # Durability is the caller's boundary, not this package's
//
// An earlier version of this comment said write-ahead ordering meant "no
// acknowledged command is lost". That was an overclaim, and a reader on
// r/highfreqtrading was right to push on it. Write-ahead is ordered against
// APPLY, not against the acknowledgement a client receives. Append writes into a
// buffer; only Sync makes a record survive the process. Whatever the embedder
// does between those two points is a window in which an order can be
// acknowledged and then vanish.
//
// Closing that window is the embedder's decision rather than this package's,
// because the two answers differ by a factor of hundreds. cmd/obgw
// group-commits every 20ms by default, so a crash loses at most that much, and
// takes -sync-every-command for anyone who wants acknowledgement to follow
// durability instead — correct, and roughly 210× the cost (docs/BENCHMARKS.md).
// Sync before you acknowledge, or state your window. Do not assume this package
// chose for you.
//
// The two failure modes are treated differently on purpose. A crash mid-write
// leaves a torn tail — a short final record — which the reader stops at cleanly,
// because a partial record cannot be applied; whether a client was already told
// about that command is the ack-window question above. A record that is complete
// but fails its checksum is media corruption: the bytes changed after they were
// written, and recovery refuses rather than stopping, since a quiet stop there is
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
	"math"
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

	// Control commands. They change no order, which is why they went unlogged for
	// four releases — and the engine state they DO change is state a venue is
	// judged on. Appended at the end of the block, never inserted: a reader with
	// records already on disk must not have them silently renumbered.
	KindHalt       // a Halt()
	KindResume     // a Resume()
	KindCancelOnly // a SetCancelOnly()
	KindSetMark    // a SetMarkPrice(price)
	KindBust       // a Bust(tradeID, reason)
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

	// MarkPrice is the new mark/index reference of a KindSetMark, in ticks. It gets
	// its own field rather than borrowing CancelID, which the Runner reuses as an
	// int64 payload internally: the log is read by tooling and by humans, and a
	// mark price filed under "cancel_id" is a trap laid for whoever reads it next.
	MarkPrice int64 `json:"mark_price,omitempty"`

	// KindBust: the annulled print and the operator's reason. TradeID is a trade
	// id, not an order id, which is why it does not share CancelID — the two
	// sequence spaces are independent and a record that confused them would be
	// unrecoverable rather than merely wrong.
	TradeID    int64  `json:"trade_id,omitempty"`
	BustReason string `json:"bust_reason,omitempty"`
}

// Header, record framing and the bounds that make a corrupt file safe to read.
//
// A file written by this package begins with Magic. Records after it are
// [4-byte length][4-byte CRC-32C of the payload][payload]. A file with no header
// is a v1 log — written before checksums existed — and is read without them; see
// ReadAll.
const (
	// SnapMagic does for a snapshot what Magic does for the log: identifies the
	// format and carries its version, so a file written by an older build is
	// recognised as such rather than misread.
	//
	// The snapshot went without one for four releases, and the asymmetry was the
	// problem. Every log record carries a CRC and a complete record that fails it
	// refuses to start the venue — but the snapshot is the BASE those records are
	// applied on top of, so a wrong snapshot is strictly worse than a wrong record,
	// and it had no integrity check at all. A torn snapshot was already impossible
	// (WriteSnapshot renames a fully-synced temp file into place, which is atomic).
	// Media corruption was not: most bit flips break the JSON and are caught by the
	// parser, but a flip inside a number parses perfectly and silently restores a
	// book that never existed.
	SnapMagic = "OBSNAP\x01"

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
	onAppend    func(seq int64, record []byte)
}

// SetOnAppend registers fn to be called after every successful append, with the
// record's payload bytes exactly as written — sequence assigned, in log order.
// It is the live tail: a log shipper subscribes here and sees the same stream a
// reader of the file would, without polling the file (see docs/REPLICATION.md).
//
// Bytes, not an Entry, and the -race run of the replication drills is why. An
// Entry holds pointers into live engine state — the order the engine is about
// to mutate as it fills — so an Entry handed to another goroutine is a data
// race wearing a convenience API. The payload bytes were already produced to
// write the record and are never touched again: fn may retain the slice, hand
// it to another goroutine, or write it to a wire verbatim, and a subscriber
// that wants the Entry back decodes it where the engine is not.
//
// The remaining obligations mirror CommandLog's, because fn runs on the
// appending goroutine — the matching goroutine when this Writer is a Runner's
// command log — and under the Writer's lock, which is what makes "in log
// order" a guarantee rather than a probability. fn must not block, and must
// not call back into this Writer; a shipper that cannot keep up must shed its
// consumer, never slow this call.
//
// The record is not necessarily durable when fn sees it: append precedes Sync.
// A consumer that must never run ahead of the primary's disk should key its
// shipping on its own Sync cadence rather than on this hook.
//
// A nil fn clears the hook. Not safe to call concurrently with appends — set it
// before the Writer is handed to a Runner.
func (w *Writer) SetOnAppend(fn func(seq int64, record []byte)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onAppend = fn
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
	if w.onAppend != nil {
		w.onAppend(w.seq, b)
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

// AppendHalt logs a Halt. Control commands are logged even when they are no-ops on
// the live engine — a Halt of an already-halted venue emits no event but is still a
// command that was issued — because the log records what was ATTEMPTED and replay
// reaches the same state by attempting the same thing. Filtering here would put a
// second, subtler copy of the engine's transition rules in the writer.
func (w *Writer) AppendHalt() (int64, error) { return w.append(Entry{Kind: KindHalt}) }

// AppendResume logs a Resume.
func (w *Writer) AppendResume() (int64, error) { return w.append(Entry{Kind: KindResume}) }

// AppendCancelOnly logs a SetCancelOnly.
func (w *Writer) AppendCancelOnly() (int64, error) { return w.append(Entry{Kind: KindCancelOnly}) }

// AppendSetMark logs a SetMarkPrice, in ticks.
func (w *Writer) AppendSetMark(price int64) (int64, error) {
	return w.append(Entry{Kind: KindSetMark, MarkPrice: price})
}

// AppendBust logs a trade bust. Like every other record here it is written before
// the engine applies it, so a bust an operator was told succeeded cannot be a bust
// the venue forgets — which is the entire reason a bust is a logged command rather
// than a note in an operator's runbook.
func (w *Writer) AppendBust(tradeID int64, reason string) (int64, error) {
	return w.append(Entry{Kind: KindBust, TradeID: tradeID, BustReason: reason})
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
//
// It is the whole log, deliberately: an operator reaching for it wants what is on
// disk, and a dozen callers index the result by ordinal. Recovery and Open take the
// same walk with a retention boundary (see walkLog), so all three agree exactly on
// the framing and on every checksum: no reader in this package accepts bytes another
// would call corrupt on those grounds.
//
// They differ in one thing, and only one: each decodes the records it hands back and
// no more. ReadAll hands back every record, so it decodes every record, which makes
// it the strictest reader here — the only one that reports a payload that passes its
// CRC and is not a record, wherever in the file it sits. Recover decodes the tail it
// applies; Open decodes the record whose sequence it resumes from. walkLog's second
// paragraph says what that costs and why it was accepted.
func ReadAll(path string) ([]Entry, error) {
	w, err := walkLog(path, 0)
	return w.entries, err
}

// retainNothing asks walkLog to keep no entries at all. No ordinal reaches it, so
// every record is still read and verified and none is retained — which is what
// lastSeq wants: one number, no slice.
const retainNothing = int64(math.MaxInt64)

// logWalk is what one pass over a log file learned.
type logWalk struct {
	// entries are the retained records: ordinal after+1 and beyond, in log order.
	entries []Entry
	// records is the ordinal of the last complete, verified record — the number of
	// records the file holds.
	records int64
	// skipped counts records that were read and verified but not retained.
	skipped int
	// lastSeq is the Seq of the last complete record, 0 for an empty or absent file.
	lastSeq int64
	// present distinguishes an empty log from an absent one. A snapshot ahead of a
	// log that does not exist yet is not a condition worth reporting.
	present bool
	// suspect reports that a parsed record's Seq was not its ordinal, i.e. the
	// property the skip rests on does not hold in this file. The caller decides what
	// to do about it; walkLog itself never guesses.
	suspect bool
}

// walkLog reads a log file front to back, verifying every record, and retains only
// those at ordinal afterSeq+1 and beyond.
//
// Every record is read in full and its CRC checked whether it is retained or not.
// That is the point of the design and not an oversight: seeking past the covered
// prefix would be faster and would stop detecting media corruption behind the
// snapshot, permanently, since each checkpoint moves the boundary further along and
// buries the damage deeper. What a skipped record saves is the json.Unmarshal and
// the retained Entry, which is where recovery's allocation lives.
//
// What that costs is one check, and it is worth naming precisely rather than filing
// under "verification". Bytes that changed on disk fail their CRC and are refused
// wherever they sit, covered or not; that property is untouched. Bytes that are
// complete, pass their CRC and are not valid JSON are a different animal — a writer
// bug or a format mismatch, not media — and finding them costs exactly the decode
// this walk stopped doing. So they are reported at and past the boundary and not
// strictly behind it, where the only way to see them is to pay the whole bill the
// skip exists to avoid. The recovered book is unaffected either way: RestoreAfter
// drops a covered record for its sequence whether or not it decoded. What is given
// up is the diagnostic, and it is given up permanently, because each checkpoint moves
// the boundary further past it. That is a decision — docs/BOUNDED-RECOVERY.md §5.2 —
// and it is why ReadAll still decodes everything.
//
// The skip rests on the ordinal of a record being its sequence number — true of
// every file this package writes, because Writer.append increments seq and writes
// one record under one mutex. It is a property of these files and not a law, so it
// is checked rather than trusted: record 1 is parsed, so is every record from
// ordinal afterSeq onward, and each parsed record must carry Seq equal to its
// ordinal. A disagreement sets suspect and the caller re-reads the file whole.
//
// A v1 headerless log parses every record regardless. It has no checksum, so
// "decodes" is its only integrity signal and the only guard against a misframed
// walk after a damaged length prefix (see the break below). v1 therefore gets the
// allocation saving and not the parse saving, and its invariant check is total
// rather than sampled.
//
// afterSeq of 0 retains everything and behaves exactly as a full parse, record for
// record and error for error.
func walkLog(path string, afterSeq int64) (logWalk, error) {
	var w logWalk
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return w, nil
		}
		return w, err
	}
	defer f.Close()
	w.present = true
	r := bufio.NewReader(f)

	// Detect the framing. A header means every record carries a CRC; its absence
	// means a v1 log, which is read without checksums so an upgrade does not lose
	// the ability to recover — those records simply cannot be verified.
	checksummed := false
	if hdr, err := r.Peek(len(Magic)); err == nil && string(hdr) == Magic {
		if _, err := r.Discard(len(Magic)); err != nil {
			return w, err
		}
		checksummed = true
	}

	// Two payload buffers, swapped after every verified record and grown to the
	// largest record seen. cur receives the record being read; prev holds the last
	// complete one, so a torn read of the next record cannot destroy it — which is
	// what lets the walk name the last record's sequence without retaining anything.
	//
	// Reusing them is only safe because a decoded Entry keeps nothing that points into
	// the bytes it came from: encoding/json copies strings, Entry has no []byte or
	// json.RawMessage field, and neither does types.Order. Adding one would make every
	// retained Entry alias the buffer the next record overwrites.
	var cur, prev []byte
	var lastParsed int64 // ordinal of the most recently parsed record
	// Declared once, outside the loop. io.ReadFull takes an io.Reader, so the slice
	// handed to it escapes; leaving these inside the loop costs eight heap bytes per
	// record, which is invisible next to parsing every record and is most of what is
	// left once parsing stops.
	var hdr [8]byte
	for ordinal := int64(1); ; ordinal++ {
		if _, err := io.ReadFull(r, hdr[:4]); err != nil {
			break // clean EOF or torn length prefix — stop at the last complete record
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		// Bound before allocating: this length came off disk and a flipped bit in it
		// must not turn recovery into a multi-gigabyte allocation.
		if n == 0 || n > MaxRecordBytes {
			return w, fmt.Errorf("%w: record %d declares %d bytes (limit %d)", ErrCorrupt, ordinal, n, MaxRecordBytes)
		}

		var want uint32
		if checksummed {
			if _, err := io.ReadFull(r, hdr[4:8]); err != nil {
				break // torn checksum — the record was never completely written
			}
			want = binary.BigEndian.Uint32(hdr[4:8])
		}

		if cap(cur) < int(n) {
			cur = make([]byte, n)
		}
		buf := cur[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			break // torn record body
		}
		if checksummed {
			if got := crc32.Checksum(buf, crcTable); got != want {
				// Complete on disk and altered since it was written. Stopping quietly
				// here would look identical to a clean end of log while discarding
				// everything after it. The record is named by its ordinal in the file,
				// counting the ones this walk skipped: RUNBOOKS tells an operator to
				// recover the records before the corrupt one, so a number that counted
				// only retained records would send them to the wrong place.
				return w, fmt.Errorf("%w: record %d checksum %08x, want %08x", ErrCorrupt, ordinal, got, want)
			}
		}

		// Parse record 1 (the anchor: a segment that starts at some sequence other
		// than 1 must be caught before any record is discarded), everything from the
		// boundary onward, and — on a v1 log — everything.
		parse := afterSeq == 0 || !checksummed || ordinal == 1 || ordinal >= afterSeq
		retain := ordinal > afterSeq
		if parse {
			var e Entry
			if err := json.Unmarshal(buf, &e); err != nil {
				if checksummed {
					// The checksum passed, so these are the bytes that were written and
					// they are not a record. That is a bug or a format mismatch, not
					// media corruption, and it must not be swallowed.
					return w, fmt.Errorf("%w: record %d passed its checksum but does not decode: %v", ErrCorrupt, ordinal, err)
				}
				break // v1 log: unverifiable, so an undecodable record is treated as the tail
			}
			lastParsed, w.lastSeq = ordinal, e.Seq
			if e.Seq != ordinal {
				w.suspect = true
			}
			if retain {
				w.entries = append(w.entries, e)
			}
		}
		if !retain {
			w.skipped++
		}
		w.records = ordinal
		cur, prev = prev, buf
	}

	// The last record of a skipping walk is usually parsed already — the boundary
	// rule reaches the end of the file. It is not when the log stops short of the
	// boundary, which is exactly the case worth reporting, so parse the retained
	// tail payload for its sequence. A v1 log never reaches here: it parses every
	// record, so lastParsed is always the last one.
	if checksummed && w.records > 0 && lastParsed != w.records {
		var e Entry
		if err := json.Unmarshal(prev, &e); err != nil {
			return w, fmt.Errorf("%w: record %d passed its checksum but does not decode: %v", ErrCorrupt, w.records, err)
		}
		w.lastSeq = e.Seq
		if e.Seq != w.records {
			w.suspect = true
		}
	}
	return w, nil
}

// lastSeq returns the sequence of the last complete record, which is what Open
// resumes from. It walks and verifies every frame and decodes record 1 and the last
// record, so an obgw restart no longer pays a second full parse of the log on top of
// Recover's.
//
// That makes Open the most permissive reader in this package, and it has to be.
// Recovery's strictness moves with the snapshot boundary; Open does not know where
// the boundary is. Were Open any stricter than the laxest Recover, a venue could
// recover its book successfully and then fail to open the very log it recovered from
// — an outage manufactured by two readers of the same bytes disagreeing. So Open
// checks what Open depends on, all of it: every frame, every checksum, and the
// sequence of the record it is about to append behind. A payload that passes its CRC
// and does not decode in the middle of the file is not one of those things, and
// ReadAll is where a caller goes to find it.
func lastSeq(path string) (int64, error) {
	w, err := walkLog(path, retainNothing)
	if err != nil {
		return 0, err
	}
	return w.lastSeq, nil
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
		case KindHalt:
			eng.Halt()
		case KindResume:
			eng.Resume()
		case KindCancelOnly:
			eng.SetCancelOnly()
		case KindSetMark:
			// A refused step (ErrMarkStepTooLarge) is ignored for the same reason a
			// cancel's refusal is: the guard is deterministic, so replaying the same
			// call against the same state reaches the same verdict.
			_ = eng.SetMarkPrice(e.MarkPrice)
		case KindBust:
			// Refusals replay as refusals here too, and this one has teeth: a bust
			// recorded against a trade id the replayed engine has not reached yet
			// would be refused, which is why the log is replayed in order and the
			// trade counter is restored from the snapshot before the tail runs.
			_ = eng.Bust(e.TradeID, e.BustReason)
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
	body, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	b := make([]byte, 0, len(SnapMagic)+4+len(body))
	b = append(b, SnapMagic...)
	b = binary.BigEndian.AppendUint32(b, crc32.Checksum(body, crcTable))
	b = append(b, body...)

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

// RecoverReport describes what a recovery READ, as opposed to what it applied.
//
// It exists because two conditions a recovery can meet are worth naming and neither
// is worth refusing to start over: a log whose sequences are not its ordinals, and a
// snapshot that is ahead of its log. Both leave the recovered book correct and both
// say something about the files that outlives the restart.
type RecoverReport struct {
	// SnapshotSeq is the snapshot's WALSeq, or 0 when there is no snapshot.
	SnapshotSeq int64
	// LogLastSeq is the sequence of the last complete record in the log, 0 if there
	// are none.
	LogLastSeq int64
	// Skipped counts records read and CRC-verified but not retained.
	Skipped int
	// Applied counts the records the sequence filter let through — those with Seq
	// greater than SnapshotSeq.
	Applied int
	// FellBack reports that the covered-prefix skip was abandoned and the log was
	// re-read whole, because a record's sequence was not its ordinal. If this is
	// routinely true the invariant is weaker than this package believes and restarts
	// are slower than before, not faster; it is surfaced so that is visible rather
	// than mysterious.
	FellBack bool
	// SnapshotAhead reports a snapshot whose WALSeq is beyond the log's last record.
	//
	// Recovery from that pair is still correct — the missing records' effects are
	// already folded into the snapshot — which is why it is reported and not refused.
	// It is reachable after an ordinary crash: a checkpoint does not sync the log
	// first, and the log group-commits, so a snapshot can be durable while records it
	// covers are still buffered. What outlives the restart is that Open resumes the
	// sequence from the log, behind the snapshot's WALSeq, so the venue reuses
	// sequences the stale snapshot already claims to cover — until the next
	// checkpoint lands, a second crash would have RestoreAfter skip them. Naming the
	// condition is what lets an operator force a checkpoint and close it.
	SnapshotAhead bool
}

// Recover rebuilds an engine from a snapshot plus the log tail after it: the
// standard bootstrap path, expressed once here so callers do not reimplement the
// sequence join and get the boundary wrong. A missing snapshot replays the whole
// log; a missing log yields the snapshot alone; neither present yields a fresh
// engine.
//
// The snapshot bounds what is APPLIED. It now also bounds what is PARSED: records
// the snapshot already covers are read and CRC-verified but not decoded or retained,
// so recovery's allocation is flat in the covered prefix instead of linear in it.
// The bytes are still all read and every checksum is still checked, wherever in the
// log the damage is — see walkLog for why that is not negotiable.
//
// One check does move with the boundary, and it is the only one: a record that is
// complete, passes its CRC and does not decode is refused at or past the snapshot's
// sequence and is walked past strictly behind it. The recovered book is the same
// either way — a covered record is dropped for its sequence regardless — so this
// starts a venue that the previous full-parse recovery refused to start. walkLog
// gives the reasoning; docs/BOUNDED-RECOVERY.md §5.2 records it as a decision.
//
// Restart time is reduced and not bounded. Reading and verifying is still O(total
// log), and nothing here rotates, truncates or archives the file, so a venue that
// runs continuously still gets slower to restart every day it stays up. Only
// rotation removes that; see docs/BOUNDED-RECOVERY.md and
// docs/PRODUCTION-READINESS.md, "Running continuously".
func Recover(config matching.Config, snapPath, walPath string) (*matching.Engine, error) {
	eng, _, err := RecoverWithReport(config, snapPath, walPath)
	return eng, err
}

// RecoverWithReport is Recover plus what the read saw. Recover is this function with
// the report dropped; embedders that want to log the conditions in RecoverReport —
// cmd/obgw does — call this instead.
func RecoverWithReport(config matching.Config, snapPath, walPath string) (*matching.Engine, RecoverReport, error) {
	var rep RecoverReport
	// The snapshot first, always: a venue whose base is unreadable should say so
	// without first spending a minute reading a log it is not going to use.
	snap, err := ReadSnapshot(snapPath)
	if err != nil {
		return nil, rep, err
	}
	var after int64
	if snap != nil {
		after = snap.WALSeq
	}
	rep.SnapshotSeq = after

	walk, err := walkLog(walPath, after)
	if err != nil {
		return nil, rep, err
	}
	if after > 0 && walk.suspect {
		// A parsed record's sequence was not its ordinal, so the records this walk
		// already discarded may be exactly the ones that needed applying. Re-read the
		// file from byte zero rather than guess an offset: continuing forward would
		// turn a detected anomaly into undetected data loss.
		rep.FellBack = true
		if walk, err = walkLog(walPath, 0); err != nil {
			return nil, rep, err
		}
	}
	rep.Skipped = walk.skipped
	rep.LogLastSeq = walk.lastSeq
	for _, e := range walk.entries {
		if e.Seq > after {
			rep.Applied++
		}
	}
	rep.SnapshotAhead = after > 0 && walk.present && walk.lastSeq < after

	var eng *matching.Engine
	if snap != nil {
		if eng, err = matching.RestoreEngine(config, snap); err != nil {
			return nil, rep, err
		}
	} else {
		eng = matching.NewEngine(config)
	}
	// Ordinal arithmetic decided what was KEPT; the sequence filter decides what is
	// applied out of that. The order matters and is not symmetric: a retained record
	// whose sequence is covered is still dropped here, but a record the walk discarded
	// by ordinal never reaches this filter at all. That is only safe because a
	// disagreement between ordinal and sequence sets walk.suspect above and sends the
	// whole file back through the full-parse path — which is why the anchor at record 1
	// and the parse from the boundary onward are not decoration.
	RestoreAfter(eng, walk.entries, after)
	return eng, rep, nil
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
	// A file without the magic is a snapshot from before checksums existed. It is read
	// without one rather than refused, for the same reason a headerless log is: an
	// upgrade must not cost a venue its ability to recover. Those bytes simply cannot
	// be verified, and rewriting the file on the next checkpoint fixes that.
	if len(b) >= len(SnapMagic)+4 && string(b[:len(SnapMagic)]) == SnapMagic {
		want := binary.BigEndian.Uint32(b[len(SnapMagic) : len(SnapMagic)+4])
		body := b[len(SnapMagic)+4:]
		if got := crc32.Checksum(body, crcTable); got != want {
			// Refused, not repaired. A snapshot is the base every log record after it
			// is applied to, so starting from one whose bytes have changed produces a
			// book that is wrong in a way nothing downstream can detect — and the
			// venue would trade against it.
			return nil, fmt.Errorf("%w: snapshot %s checksum %08x, want %08x", ErrCorrupt, path, got, want)
		}
		b = body
	}
	var s matching.EngineSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
