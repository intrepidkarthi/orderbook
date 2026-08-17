// Package wal provides durable, append-only write-ahead logging and snapshot
// persistence for the matching engine — the crash-recovery storage backend.
//
// It records the ordered *command* stream to disk — every command that mutates
// the book: submits, cancels, reduces, the operator's account-wide cancel, and the
// conditional order types (stop, OCO, iceberg, pegged, trailing). A fresh engine
// replays the log — optionally starting from a snapshot — to reach identical book state, the same recovery contract LMAX (journal +
// snapshot + replay) and Binance (hourly snapshot + sequential replay) rely on.
// Snapshotting bounds the REPLAY to O(recent) — only the WAL tail after the
// snapshot's sequence is applied, at roughly 2µs per record. A restart costs three
// terms: O(book) to load the snapshot (~174ms at a 100,000-order book), O(RETAINED
// log) to read and checksum-verify every record still on disk including the ones the
// snapshot already covers, and O(tail) to apply what is left (~21ms for 10,000
// records). Recovery skips decoding and retaining the covered prefix, which makes its
// ALLOCATION flat in that prefix, but it still reads and verifies every byte of it —
// see Recover and walkSegment.
//
// The middle term is bounded by RETENTION and by nothing else. A log is a SET of
// segments (see segment.go); rotation cuts it into files, and Retain deletes a prefix
// of them once a verified snapshot covers it. Deletion is OFF by default, and a venue
// that leaves it off still gets slower to restart every day it stays up — about
// 44 GiB of journal a day at 2,500 messages/s. With a byte budget set, restart time is
// what the operator chose: reading and verifying costs about 2 s per GiB cold and
// 0.75 s per GiB warm, whatever the segment size. See
// docs/BENCHMARKS.md, docs/BOUNDED-RECOVERY.md and docs/LOG-ROTATION.md.
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
	"strings"
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

	// KindSetPhase records a Runner.SetPhase transition — the opening or closing
	// uncross, and the trading phase the venue moves into.
	//
	// The third control command to be applied and never written down, after
	// Reduce and Halt, and the most expensive of the three: the transition runs an
	// auction, so losing it does not merely restore the wrong enum, it makes the
	// replayed venue print trades that never happened (a lost move INTO pre-open
	// matches orders the live venue rested) or lose trades subscribers already
	// received (a lost move OUT of it skips the uncross). See
	// docs/JOURNAL-COMPLETENESS.md §3.
	KindSetPhase // a SetPhase(phase)

	// entryKindCount is a sentinel: keep it last, and unexported so it never
	// reaches disk and stays off the frozen surface.
	//
	// It exists so TestEveryEntryKindReplays can ENUMERATE this block. Journalled
	// is not the same as replayed: a kind can have a Writer method, a CommandLog
	// method and a logCommand case, and still be dropped on the floor by
	// restoreEntry's switch — a durable record that recovery silently discards.
	// The command-side guard in pkg/matching cannot see that half; this sentinel
	// is what lets the wal side check it. See docs/JOURNAL-COMPLETENESS.md §8.
	entryKindCount
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

	// Phase is the target trading phase of a KindSetPhase, written as its NAME
	// ("OPEN", "PRE_OPEN", …) rather than its ordinal.
	//
	// This is deliberately against the grain of the codebase: matching.EngineState
	// is a uint8 iota and EngineSnapshot.State is already encoded as its number, so
	// a number here would be consistent. Lifetime is what makes it wrong. A
	// snapshot is rewritten every checkpoint, so the oldest ordinal on disk is
	// minutes old; a segment is retained for as long as retention says and may be
	// archived for years (docs/LOG-ROTATION.md §5). Reordering the EngineState
	// block is a change nobody would think of as a format change, and it would
	// silently reinterpret every archived phase record. A name also fails loudly on
	// a value the reader has never heard of where an ordinal fails quietly, and an
	// operator reading a segment during an incident sees "PRE_OPEN" rather than 3.
	// Same argument that gave MarkPrice its own field instead of borrowing CancelID.
	//
	// omitempty keeps every pre-existing record byte-identical.
	Phase string `json:"phase,omitempty"` // KindSetPhase
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

// Writer is an append-only command log. It is safe for concurrent use, and it must
// be written write-ahead: append before the engine applies the command, so the log
// can never be missing something the engine has already done.
//
// Durability is a SEPARATE promise from write-ahead ordering, and conflating the two
// is a mistake this comment used to make. Append puts bytes in a bufio.Writer; only
// Sync makes them survive a crash. Whether a Sync happens before the engine applies
// the command is the CALLER'S policy, not this type's:
//
//   - matching.Runner's default path calls Append and does not call Sync, so an
//     acknowledged order can be lost in the window before the next group commit.
//     That window is the venue's recovery-point objective and it is meant to be
//     stated to clients, not assumed away — see docs/REPLICATION.md §4.
//   - cmd/obgw's -sync-every-command decorator (cmd/obgw/synclog.go) calls Sync
//     inside each Append, which is what actually earns "a crash never loses an
//     acknowledged order". TestSyncEveryCommandIsDurableBeforeApply proves it with a
//     second file descriptor; its "group commit leaves it buffered" subtest proves
//     the default does not, by seeing zero records through that same descriptor.
//
// This sentence claimed the strong guarantee unconditionally for four releases while
// the default path did not provide it. Batch Sync (group commit) to amortise the
// fsync across many appends, and know which of the two you are running.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	w   *bufio.Writer
	seq int64
	// checksummed is false when appending to a headerless v1 file. Records keep
	// that file's framing rather than switching mid-file, which would leave a log
	// no reader could parse. A framing change is legal at a segment boundary and
	// only there, so rotating really does get checksums onto an old file.
	checksummed bool
	onAppend    func(seq int64, record []byte)

	// The set this writer appends to, and where in it.
	stem string
	dir  string
	// path, base, written and segRecords describe the ACTIVE segment: the newest
	// member of the set and the only one anything ever writes to.
	path       string
	base       int64
	written    int64 // bytes in the active segment, buffered ones included
	segRecords int64
	// legacyStem is true while the active segment IS the stem — a log that has
	// never rotated. The first rotation migrates it into the set.
	legacyStem bool
	opts       Options
	rotations  int64

	// failed latches a durability failure. A log that cannot be flushed means every
	// command since the last successful sync is acknowledged and not durable, so
	// continuing to serve is continuing to lie; and a partially completed flush
	// leaves bufio.Writer's buffer in an indeterminate relationship to the file, so
	// appending behind it writes a record into the middle of a torn one. Clearing it
	// takes a restart, which is where an operator gets to decide whether the disk is
	// actually fixed.
	failed error

	// beforeRotateStep, when set, runs before each numbered step of the rotation
	// protocol and can fail it. Test-only: it is how the crash matrix in
	// docs/LOG-ROTATION.md §3.3 is exercised without killing a process.
	beforeRotateStep func(step int) error
}

// Options configure a Writer's segment set. The zero value is the shipped default:
// rotate at 128 MiB, keep everything, keep at least four sealed segments, archive
// nowhere.
//
// Rotation is on by default because it changes where bytes live and nothing about
// what they say, and a venue that does not rotate can never be bounded. DELETION is
// off by default — RetainBytes zero means keep everything, which is the old
// behaviour with better file names. Deleting a venue's journal is not a behaviour
// anybody should acquire by upgrading, and it is the one thing in this package that
// cannot be undone.
type Options struct {
	// MaxSegmentBytes is the size at which the active segment is sealed and a new
	// one started. Zero takes the default; negative disables rotation entirely.
	//
	// A single record may legitimately be up to MaxRecordBytes, so a limit below
	// that produces an oversized segment rather than an infinite rotation loop.
	MaxSegmentBytes int64
	// RetainBytes is the byte budget for the retained set. Zero keeps everything.
	//
	// A budget, not a bound. It is the point at which retention stops WANTING to
	// delete; MinSegments is a separate term that stops it being ABLE to, and it is
	// checked second, so the floor wins. The smallest set retention will leave is
	// therefore (MinSegments + 1) * MaxSegmentBytes — the sealed floor plus the active
	// segment — which at the defaults of 4 and 128 MiB is 640 MiB whatever number is
	// set here. Sizing a retained set means picking all three together; setting this
	// alone to something below the floor is silent, and RetentionResult.Skipped is
	// where a cycle says which term stopped it.
	RetainBytes int64
	// MinSegments is how many sealed segments are kept regardless of coverage. Not
	// a correctness term — the snapshot predicate is — but an operational one: it
	// keeps recent history on disk for forensics and keeps a reconnecting follower's
	// catch-up out of the snapshot-bootstrap path in the common case.
	//
	// It is also the floor under RetainBytes; see there.
	MinSegments int
	// ArchiveDir, when set, receives a byte-identical copy of every segment before
	// it is deleted. A failure to archive stops retention for that cycle.
	ArchiveDir string
}

// DefaultMaxSegmentBytes is 128 MiB: about 610,000 records at 220 bytes, roughly
// four minutes at 2,500 messages/s and roughly 350 segments a day. Small enough
// that retention and archival have useful granularity and an archived unit is a
// manageable object; large enough that a rotation's two fsyncs, link, unlink and
// directory fsync amortise over minutes rather than seconds.
const DefaultMaxSegmentBytes int64 = 128 << 20

// DefaultMinSegments is the sealed-segment floor retention will not go below.
const DefaultMinSegments = 4

func (o Options) withDefaults() Options {
	if o.MaxSegmentBytes == 0 {
		o.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
	if o.MinSegments == 0 {
		o.MinSegments = DefaultMinSegments
	}
	if o.MinSegments < 0 {
		o.MinSegments = 0
	}
	return o
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

// Open opens (creating if needed) a WAL for appending, recovering the last
// sequence number so new entries continue monotonically. It takes the default
// Options; OpenWith is where they are set.
//
// path names a set: the newest segment is the one appended to. A path with nothing
// at it gets a single file in the current format, exactly as it always did, and
// that file becomes segment 1 of a set the first time it rotates. An existing file
// keeps whichever framing it already has, because appending checksummed records to
// a headerless log would produce a file that is neither format.
func Open(path string) (*Writer, error) { return OpenWith(path, Options{}) }

// OpenWith is Open with the segment set's policy named.
//
// It never migrates a legacy file, never deletes anything and never rewrites a
// record. What it may do that Open did not is SEAL: if the newest segment ends in a
// partial record, it is left exactly as it is and the next record starts a new
// segment based at lastSeq+1.
//
// It also SEALS a segment whose declared matching semantics is not this build's, so
// this build's records never land behind a header that would then describe records it
// did not produce. That is what makes the mismatch Recover refuses self-healing: one
// rotation at the first restart after an upgrade, and the set is correct from then on.
// See openPastTheStamp and docs/SEMANTICS-VERSION.md §3.7. Open itself never REFUSES —
// it is the most permissive reader in this package, per docs/BOUNDED-RECOVERY.md §9.1,
// because cmd/obgw opens the log it has just recovered from and two readers of the same
// bytes disagreeing is how a benign file becomes an outage.
//
// That closes the defect docs/BOUNDED-RECOVERY.md §6.1 confirmed by experiment and
// left open for want of a safe fix. Open used to take O_APPEND and write the next
// record BEHIND the fragment's bytes; the fragment's length prefix then found enough
// following bytes to look complete, its CRC failed, and the venue refused to start
// on the SECOND restart after a crash. §6.1 said the fix was to truncate and that
// truncating in a recovery path needed its own spec, because a fragment is sometimes
// the only evidence a command was attempted. Segments give a fix that truncates
// nothing: leave the fragment, stop writing to that file forever, start a new one.
func OpenWith(path string, opts Options) (*Writer, error) {
	opts = opts.withDefaults()
	var (
		set  *segmentSet
		last int64
		sw   segWalk
	)
	// The enumeration and the newest segment's walk happen before anything is
	// created, so restarting them on a segment that retention removed underneath is
	// free of consequence.
	if err := retryWhileVanishing(func() error {
		var err error
		if set, err = enumerateSet(path); err != nil {
			return err
		}
		// A rotation that crashed between materialising a segment and linking it into
		// place leaves a temp behind. It is not in the set — it fails the 16-digit
		// pattern — so no reader has ever seen it; clearing it here keeps the
		// directory honest and is the one thing Open does that a reader would not.
		removeStaleTemps(set)
		last, sw, err = lastSeq(set)
		return err
	}); err != nil {
		return nil, err
	}
	// The one shape a downgrade cannot survive — numbered segments with NOTHING at
	// the stem — is repaired here, before any of the branches below, because none of
	// them would. It is the same kind of thing removeStaleTemps does and it is done
	// for a stronger reason: a stale temp is untidy, and an absent stem is an older
	// build finding no file at -wal, concluding there is no log, and starting an empty
	// venue beside every record the set holds.
	//
	// It is reachable without a bug in this package. The migration at the first
	// rotation renames the stem to segment 1 and writes the marker as two steps, and
	// nothing makes them one: a process killed between them, or a writeMarker that
	// fails with ENOSPC or EROFS — the disk condition this whole mechanism exists for
	// — leaves exactly this state. rotateLocked fails the append and halts the engine
	// when that happens, which is correct and is also why the repair belongs at the
	// next start rather than in the writer: the process that noticed is on its way
	// out. Before this, OpenWith's default branch inferred legacyStem from
	// newest.named, which is false once the rename has happened, so neither Open nor
	// any later rotation ever wrote the missing marker — confirmed to survive 46
	// further rotations. TestOpenHealsASetWhoseStemIsMissing.
	if err := healMissingStem(set, path); err != nil {
		return nil, err
	}

	w := &Writer{
		stem: path, dir: filepath.Dir(path),
		seq: last, opts: opts,
	}

	newest, have := set.newest()
	switch {
	case !have && set.marker:
		// A marker with no segments: a venue owns this path and its segments are gone
		// or were never written. Records must not go at the stem underneath a marker,
		// so start a proper segment at 1.
		return openFreshSegment(w, 1)
	case !have:
		// Nothing at this path. A fresh log is a single file at the stem, which is
		// what every existing deployment, runbook and test expects to find there —
		// the set materialises around it at the first rotation.
		//
		// That file now opens with a full segment header rather than the six-byte
		// Magic, and the reason is the stamp rather than tidiness. A log this build
		// writes must DECLARE the semantics its records were produced under, or the
		// first crash recovery on a brand-new venue meets a file carrying this
		// build's records and no statement of whose rules made them — which is
		// exactly the condition docs/SEMANTICS-VERSION.md §4 refuses to guess about.
		// The header also declares base 1 explicitly, which the six-byte form only
		// ever implied by being the only file there was.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		w.f, w.w = f, bufio.NewWriter(f)
		w.path, w.base, w.checksummed, w.legacyStem = path, 1, true, true
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if st.Size() == 0 {
			if _, err := w.w.Write(segHeaderV3(1, matching.SemanticsVersion)); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
		w.written = int64(SegHeaderBytesV3)
	case sw.torn:
		// Rule 8. The fragment stays where it is; this segment never takes another
		// record. The new segment's base is exactly last+1, which is what makes the
		// contiguity check treat a sealed torn segment as legal rather than as a gap.
		base := last + 1
		if newest.base >= base {
			// A segment holding a header and nothing but a torn first record. Its base
			// is already the next sequence, so a new segment cannot be named without
			// colliding with it. Move the fragment aside — no complete record is in it,
			// so nothing committed is lost — and start the segment cleanly.
			base = newest.base
			if err := setAside(newest.path); err != nil {
				return nil, err
			}
		}
		return openFreshSegment(w, base)
	case newest.semantics != matching.SemanticsVersion:
		// Rule 15 of docs/SEMANTICS-VERSION.md: the active segment declares somebody
		// else's matching semantics, so this build does not append behind its header.
		//
		// Without this the stamp is a lie in the one direction that matters. A venue
		// upgrades, restarts, Open resumes the newest segment — created by the
		// PREVIOUS build and declaring the previous semantics — and every record this
		// build writes lands in it. The segment then claims one semantics and holds
		// records from two, and the next crash recovery either refuses a tail that is
		// perfectly replayable or, with the override in hand, replays records it
		// should have refused. Either way the header has stopped describing its own
		// contents, which is the only thing it was for.
		//
		// It is also what makes the condition SELF-HEALING. Leaving the segment alone
		// and letting the gate be conservative means a venue that upgraded correctly
		// meets the refusal on every crash recovery until the segment fills, which at
		// the 128 MiB default is a long time to be one power loss away from an outage.
		// The cost is one rotation per upgrade.
		return openPastTheStamp(w, newest, last, sw)

	default:
		f, err := os.OpenFile(newest.path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		w.f, w.w = f, bufio.NewWriter(f)
		w.path, w.base = newest.path, newest.base
		w.checksummed = newest.checksummed()
		w.legacyStem = !newest.named
		w.written, w.segRecords = newest.size, sw.records
	}
	return w, nil
}

// openPastTheStamp puts this build's records somewhere other than behind a header
// that declares a different matching semantics, and points w at it.
//
// Two shapes, because the general one is arithmetically impossible on the second.
//
// A segment that HOLDS RECORDS is sealed: it keeps every byte it has, and a new
// segment based at last+1 takes the appends. A legacy stem is migrated into the set
// first, by the same rename-and-mark the first rotation would have done, so the v1
// file's own bytes and framing are never touched and this build's records go into a
// new checksummed segment beside it. That is the Writer doc comment's "rotate to get
// checksums on an old file" becoming true a second time, for a second reason.
//
// A segment that holds NO records is REPLACED IN PLACE, because rotation cannot
// express it: the next segment's base is last+1, an empty segment's last is base−1,
// so the new segment would claim the base the old one already has and collide with
// its own filename — EEXIST at exactly the moment a venue is trying to start. There
// is nothing to preserve in a file with no records, so the header is rewritten
// through the same crash-atomic temp-and-rename WriteSnapshot uses. See
// docs/SEMANTICS-VERSION.md Rules 15 to 17.
func openPastTheStamp(w *Writer, newest segment, last int64, sw segWalk) (*Writer, error) {
	if sw.records == 0 {
		base := newest.base
		if !newest.named {
			// The stem, holding a header and nothing else. Its base is implicit and
			// is 1, whatever shape the header it carries has.
			base = 1
		}
		if err := replaceSegmentHeader(newest.path, base); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(newest.path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		w.f, w.w = f, bufio.NewWriter(f)
		w.path, w.base = newest.path, base
		w.checksummed, w.legacyStem = true, !newest.named
		w.written, w.segRecords = int64(SegHeaderBytesV3), 0
		// Not a rotation: nothing was sealed and no segment was created. Rotations
		// counts sealed segments, and a header swap on an empty file seals none.
		return w, nil
	}
	if !newest.named {
		// The migration docs/LOG-ROTATION.md §7 performs at the first rotation,
		// performed here instead because this Open is the first moment the stem stops
		// being able to describe the whole log. openFreshSegment writes the marker,
		// since the stem is absent the instant the rename lands.
		if err := os.Rename(w.stem, segPath(w.stem, 1)); err != nil {
			return nil, err
		}
	}
	out, err := openFreshSegment(w, last+1)
	if err != nil {
		return nil, err
	}
	out.rotations++
	return out, nil
}

// replaceSegmentHeader swaps a record-free segment's header for this build's,
// crash-atomically: a complete header into a temp file, fsynced, renamed over the
// target, then the directory fsynced. A crash at any point leaves either the old
// header or the new one, and both are valid segments holding no records.
func replaceSegmentHeader(path string, base int64) error {
	tmp := path + ".hdr.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(segHeaderV3(base, matching.SemanticsVersion)); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// openFreshSegment materialises <stem>.<base> and points w at it. It is the same
// protocol rotation uses, for the same reason: a reader must never meet a segment
// whose header is missing.
//
// If nothing is at the stem it also writes the marker, and that is not tidiness. A
// numbered segment with an ABSENT stem is the one shape a downgrade cannot survive:
// an older build finds no file at -wal, concludes there is no log, and starts an
// empty venue beside everything. The other shapes are all detectable — an old build
// that appends into a legacy stem alongside a numbered segment produces an overlap,
// which this build refuses on the next start — so the marker is what turns the silent
// case into a loud one.
func openFreshSegment(w *Writer, base int64) (*Writer, error) {
	if _, err := os.Lstat(w.stem); os.IsNotExist(err) {
		if err := writeMarker(w.stem); err != nil {
			return nil, err
		}
	}
	f, err := w.materialiseSegment(base)
	if err != nil {
		return nil, err
	}
	w.f, w.w = f, bufio.NewWriter(f)
	w.path, w.base = segPath(w.stem, base), base
	w.checksummed, w.legacyStem = true, false
	w.written, w.segRecords = int64(SegHeaderBytesV3), 0
	return w, nil
}

// setAside renames a file out of the set without destroying it. Used for exactly
// one thing: a segment that holds a torn first record and no complete one, whose
// declared base is the sequence the next segment must have. The bytes survive under
// a name no enumerator will match, so ReadAll still reports the set as it is and the
// evidence that a command was attempted is still on disk.
func setAside(path string) error {
	for i := 0; ; i++ {
		aside := path + ".torn"
		if i > 0 {
			aside = fmt.Sprintf("%s.torn.%d", path, i)
		}
		if _, err := os.Lstat(aside); err == nil {
			continue
		}
		return os.Rename(path, aside)
	}
}

// healMissingStem writes the marker when the set holds numbered segments and there
// is no file at the stem at all.
//
// Only that shape. A stem that exists is left exactly as it is, whatever it holds —
// a marker, or a pre-rotation file still holding records 1..k — because rewriting
// either would destroy information. A set with no numbered segments is not this
// shape: there is nothing for an old build to miss.
func healMissingStem(set *segmentSet, stem string) error {
	// Only a definite absence acts. Anything else — the stem is there holding a
	// marker or a pre-rotation file, or the stat itself failed — is left exactly as
	// it is, because writing over a stem this function cannot read is how a repair
	// turns into damage. enumerateSet has already put a present stem in the set.
	if _, err := os.Lstat(stem); !os.IsNotExist(err) {
		return nil
	}
	// With the stem absent, every member of the set came from the directory scan, so
	// a non-empty set here means numbered segments and no stem. An empty one is a
	// path with no log at it, which is not this shape and needs no marker.
	if len(set.segs) == 0 {
		return nil
	}
	return writeMarker(stem)
}

// removeStaleTemps clears <stem>.<16 digits>.tmp left by a rotation that crashed
// between writing a segment's header and linking it into place.
func removeStaleTemps(set *segmentSet) {
	if set.dir == "" {
		return
	}
	ents, err := os.ReadDir(set.dir)
	if err != nil {
		return
	}
	stemName := filepath.Base(set.stem)
	for _, ent := range ents {
		name := strings.TrimSuffix(ent.Name(), ".tmp")
		if name == ent.Name() {
			continue
		}
		// A segment's temp, or the marker's. Both are written, fsynced and renamed or
		// linked into place, so one left behind is the residue of a crash between those
		// steps and is never anything a reader has seen.
		if _, ok := segBaseFromName(stemName, name); ok || name == stemName+".marker" {
			_ = os.Remove(filepath.Join(set.dir, ent.Name()))
		}
	}
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
	if w.failed != nil {
		return 0, w.failed
	}
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
	// Rotation is decided HERE and nowhere else: on the appending goroutine, inside
	// the same critical section that assigns the sequence, before the frame header is
	// BUILT — not merely before it is written.
	//
	// Under w.mu because Sync takes w.mu, flushes the buffer and fsyncs the fd — a
	// rotation that could land between those two lines would flush buffered bytes to
	// the old fd and fsync the new one, and the venue would report durable a set of
	// records that are not. Under one lock that is structurally impossible rather
	// than carefully avoided, and there is exactly one place where w.f and w.w
	// change, which is what SetOnAppend's "in log order" guarantee needs.
	//
	// Before the header write because append writes the frame header and the payload
	// as two separate buffered writes. A rotation between them would leave a header
	// in segment n whose payload is in segment n+1: unreadable by every reader here
	// and indistinguishable from a torn tail followed by a segment of garbage.
	//
	// Before the header is BUILT because a rotation can change the FRAMING. A v1
	// headerless segment rotates into an OBWAL\x02 one whose records carry CRCs, and
	// a header computed from the old framing lands four bytes short inside a segment
	// that declares every record checksummed. The reader takes the record's first
	// four payload bytes as its checksum, refuses, and the set is unreadable from
	// that segment onward — permanently, and invisibly until the next restart, since
	// Open reads only the newest segment. TestRotatingAV1LogKeepsTheWholeSetReadable.
	//
	// The frame's own size is therefore measured against the framing of the segment
	// the record would land in if it did NOT rotate, which is the segment whose
	// budget the test is about.
	//
	// The segRecords term is not decoration. A single record may legitimately be up
	// to MaxRecordBytes, so a limit set below that must produce one oversized segment
	// rather than an infinite rotation loop.
	frame := int64(4)
	if w.checksummed {
		frame = 8
	}
	if w.opts.MaxSegmentBytes > 0 && w.segRecords > 0 &&
		w.written+frame+int64(len(b)) > w.opts.MaxSegmentBytes {
		if err := w.rotateLocked(w.seq); err != nil {
			// Rule 5: a rotation that fails at any step fails the append. The sequence
			// rolls back, the record is not written, onAppend does not fire, and the
			// error reaches Runner.logCommand, which halts the engine. The property a
			// log shipper depends on is that a sequence handed to onAppend is a
			// sequence that exists.
			//
			// It latches for the same reason a failed Sync does. A rotation that got as
			// far as linking the new segment into place and then failed leaves a writer
			// whose active segment is the OLD one, so continuing to append there would
			// write records carrying sequences the new segment's name already claims —
			// an overlap, self-inflicted, that the next start would refuse.
			w.seq--
			w.failed = err
			return 0, err
		}
	}

	// The frame, from the framing of the segment this record is actually going into.
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
	w.written += int64(n) + int64(len(b))
	w.segRecords++
	if w.onAppend != nil {
		w.onAppend(w.seq, b)
	}
	return w.seq, nil
}

// rotateLocked seals the active segment and makes a new one based at base. Called
// with w.mu held, from append and from nowhere else.
//
// The steps are docs/LOG-ROTATION.md §3.2, and steps 3 to 5 are a link-and-unlink
// dance where os.Create would do. The reason is the crash matrix: it deletes the
// state "the segment exists and its header does not" from existence, rather than
// requiring every reader to have a rule for it. The alternative — create, write the
// header, and teach recovery that a segment shorter than its header is one it should
// rewrite — was rejected because it makes a READER repair a file, and a reader that
// writes is a reader that can turn a diagnosis into damage.
//
// link rather than rename because rename overwrites. EEXIST here means something
// else is writing this set, and the right answer is to fail the append (which halts
// the engine) rather than to overwrite a file that may hold records.
func (w *Writer) rotateLocked(base int64) error {
	if base >= 1e16 {
		return fmt.Errorf("wal: sequence %d needs more than %d digits, which the segment naming cannot express", base, segDigits)
	}
	if err := w.step(1); err != nil {
		return err
	}
	if err := w.w.Flush(); err != nil {
		return err
	}
	if err := w.step(2); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}

	// The migration, and it happens here rather than at Open on purpose. A venue's
	// first rotation is the first moment the stem stops being able to describe the
	// whole log, so it is the first moment a downgrade could read the stem, find no
	// segments and start an empty venue beside four hundred million records. Before
	// that instant the stem IS the log and an old build reads it correctly.
	if w.legacyStem {
		if err := os.Rename(w.stem, segPath(w.stem, 1)); err != nil {
			return err
		}
		w.path, w.legacyStem = segPath(w.stem, 1), false
		if err := writeMarker(w.stem); err != nil {
			return err
		}
	}

	f, err := w.materialiseSegment(base)
	if err != nil {
		return err
	}
	if err := w.step(7); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.f.Close(); err != nil {
		_ = f.Close()
		return err
	}
	w.f, w.w = f, bufio.NewWriter(f)
	w.path, w.base = segPath(w.stem, base), base
	w.checksummed = true
	w.written, w.segRecords = int64(SegHeaderBytesV3), 0
	w.rotations++
	return nil
}

// materialiseSegment creates <stem>.<base> with its header already durable, and
// returns the fd positioned to take the first record.
//
// The returned fd is opened on the FINAL path, not the temp the segment was built
// through, and that is not cosmetic. An *os.File remembers the name it was opened
// with for the life of the fd, and every write, flush, fsync and close error it
// produces is a *PathError carrying that name. Returning the temp's fd meant every
// durability failure on the active segment named <base>.tmp — a file that does not
// exist, because it was unlinked at rotation time. An operator following
// docs/RUNBOOKS.md "The disk filled up" greps for the filename in the error and finds
// nothing; worse, §3.3 teaches that a stray <base>.tmp means a rotation that crashed
// between materialising a segment and linking it into place, so the message points
// the diagnosis at a crashed rotation when the condition is a flush failure on a live
// segment. Observed on a real ENOSPC: "write /Volumes/WALTINY/venue/
// v.wal.0000000000078054.tmp: no space left on device" while ls showed the segment
// present at 1,069,056 bytes and no .tmp anywhere.
//
// The reopen cannot lose the header: durability belongs to the inode, which the link
// preserved, and it was fsynced before the link. TestTheActiveSegmentsFdNamesTheFile.
func (w *Writer) materialiseSegment(base int64) (*os.File, error) {
	target := segPath(w.stem, base)
	tmp := target + ".tmp"
	if err := w.step(3); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	// The stamp comes from the constant, never from a literal. A literal here would
	// diverge from matching.SemanticsVersion the first time the constant moved, and
	// every test that reads a header back would still pass — which is why
	// TestTheStampIsWired asserts this specific wiring rather than the shape of the
	// bytes.
	if _, err := f.Write(segHeaderV3(base, matching.SemanticsVersion)); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := w.step(4); err != nil {
		return fail(err)
	}
	if err := os.Link(tmp, target); err != nil {
		return fail(fmt.Errorf("wal: segment %s already exists — another writer owns this set: %w",
			filepath.Base(target), err))
	}
	if err := w.step(5); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.Remove(tmp); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := w.step(6); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syncDir(w.dir); err != nil {
		_ = f.Close()
		return nil, err
	}
	// O_APPEND, like every other fd this package writes through, so the write offset
	// is the end of the file rather than something this function has to keep track of
	// across a reopen. The new fd is taken before the old one is released: a failure
	// here fails the rotation, which fails the append and halts the engine, and that
	// is the right answer for "cannot open a file created two syscalls ago" — but it
	// must not also cost the caller the fd it already had.
	named, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = named.Close()
		return nil, err
	}
	return named, nil
}

// step runs the injected fault hook for one numbered step of the rotation protocol.
// Nil in every build that is not a test.
func (w *Writer) step(n int) error {
	if w.beforeRotateStep == nil {
		return nil
	}
	return w.beforeRotateStep(n)
}

// writeMarker puts the 18-byte set marker at the stem, durably.
//
// The marker is what makes a downgrade loud. Without it an older build pointed at a
// rotated set finds no file at -wal, concludes there is no log, starts an EMPTY
// VENUE and begins writing sequence 1 beside segments holding four hundred million
// records. With it, the old build peeks six bytes, does not find OBWAL\x01, treats
// the file as a headerless v1 log, reads "OBWA" as a length prefix of 1,329,747,777
// bytes and refuses from both ReadAll and Open. A loud refusal on a downgrade, where
// the alternative is a silent empty venue.
//
// It is advisory: a set without one is valid, which is what makes the migration
// crash-safe at every point.
func writeMarker(stem string) error {
	tmp := stem + ".marker.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(segHeader(markerBase)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, stem)
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

// AppendSetPhase logs a trading-phase transition, by name.
//
// Like AppendHalt it records what was ATTEMPTED: a transition to the phase the
// venue is already in is a no-op on the engine and is still written down, because
// replay reaches the same verdict by attempting the same thing and a filter here
// would put a second copy of Engine.SetPhase's early-return in the writer.
//
// An undeclared EngineState would render as "OPEN" through String's default arm,
// which would durably record a transition under a name meaning a different phase.
// matching.TestEngineStateNamesRoundTrip is what keeps String total over the
// declared block; see docs/JOURNAL-COMPLETENESS.md §4.1.
func (w *Writer) AppendSetPhase(phase matching.EngineState) (int64, error) {
	return w.append(Entry{Kind: KindSetPhase, Phase: phase.String()})
}

// Sync flushes buffered records and fsyncs the file — the durability point. Call
// it before acknowledging the commands since the last Sync (group commit).
//
// A failure LATCHES. Every subsequent append and sync returns the same error until
// the process restarts, and an embedder is expected to halt the book and fail
// readiness on it rather than log it and carry on.
//
// Two reasons, and the second is the sharper one. A log that cannot be flushed means
// every command since the last successful sync is acknowledged and not durable, so
// continuing to serve is continuing to lie — which is exactly what a full disk used
// to produce: fifty log lines a second, /readyz still green, and the write-ahead
// property silently gone. And a partially completed flush leaves bufio.Writer's
// buffer in an indeterminate relationship to the file, so appending behind it writes
// a record into the middle of a torn one. Latching also stops the engine flapping
// open and closed as space transiently appears; clearing it takes a restart, which
// is where an operator gets to decide whether the disk is actually fixed.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncLocked()
}

func (w *Writer) syncLocked() error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.w.Flush(); err != nil {
		w.failed = fmt.Errorf("wal: %s is no longer being journalled and this writer will not be used again: %w", w.stem, err)
		return w.failed
	}
	if err := w.f.Sync(); err != nil {
		w.failed = fmt.Errorf("wal: %s is no longer being journalled and this writer will not be used again: %w", w.stem, err)
		return w.failed
	}
	return nil
}

// Failed reports the latched durability failure, or nil. An embedder polls it to
// decide readiness; there is no way to clear it short of a restart.
func (w *Writer) Failed() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

// Seq returns the last written sequence number.
func (w *Writer) Seq() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// Rotations counts the segments this writer has sealed. It is the number
// docs/BENCHMARKS.md publishes rotation's cost against, so it is exported rather
// than inferred from a directory listing.
func (w *Writer) Rotations() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rotations
}

// ActiveSegment is the path of the segment records are currently written to, and
// Base is the sequence its first record carries. Both are what a test or an
// operator-facing tool needs to aim at a specific file.
func (w *Writer) ActiveSegment() (path string, base int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path, w.base
}

// Close syncs and closes the WAL file. The file descriptor is released even when
// the sync fails, because leaking it would turn a disk problem into a second one.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.syncLocked()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadAll reads every complete entry from a WAL, in order, stopping cleanly at a
// torn tail (a partial record left by a crash mid-write). A missing log yields no
// entries.
//
// path names a SET, not necessarily a file: the stem plus its numbered segments,
// concatenated in base order into one flat slice. A log that has never rotated is a
// set of one, so this is the same slice it always was.
//
// One qualifier is the price of retention, and it is new: ReadAll returns what is
// STILL ON DISK. Once retention has deleted a prefix of the set, the first entry's
// Seq is the retention floor rather than 1, and an operator asking "what happened to
// order X" may need the archive instead. The floor is legible without tooling — it
// is the oldest segment's name.
//
// It is the whole retained log, deliberately: an operator reaching for it wants what
// is on disk, and a dozen callers index the result by ordinal. Recovery and Open take
// the same walk with a retention boundary (see walkSegment), so all three agree
// exactly on the framing and on every checksum: no reader in this package accepts
// bytes another would call corrupt on those grounds.
//
// They differ in one thing, and only one: each decodes the records it hands back and
// no more. ReadAll hands back every record, so it decodes every record, which makes
// it the strictest reader here — the only one that reports a payload that passes its
// CRC and is not a record, wherever in the file it sits. Recover decodes the tail it
// applies; Open decodes the record whose sequence it resumes from. walkSegment's
// second paragraph says what that costs and why it was accepted.
func ReadAll(path string) ([]Entry, error) {
	w, err := walkSet(path, 0)
	return w.entries, err
}

// retainNothing asks walkLog to keep no entries at all. No ordinal reaches it, so
// every record is still read and verified and none is retained — which is what
// lastSeq wants: one number, no slice.
const retainNothing = int64(math.MaxInt64)

// logWalk is what one pass over a whole segment set learned.
type logWalk struct {
	// entries are the retained records: sequence after+1 and beyond, in log order,
	// concatenated across segments.
	entries []Entry
	// records is the number of complete, verified records the set holds.
	records int64
	// skipped counts records that were read and verified but not retained.
	skipped int
	// lastSeq is the Seq of the last complete record, 0 for an empty or absent set.
	lastSeq int64
	// present distinguishes an empty log from an absent one. A snapshot ahead of a
	// log that does not exist yet is not a condition worth reporting.
	present bool
	// suspect reports that a parsed record's Seq was not the sequence its segment's
	// declared base and its own position imply, i.e. the property the skip rests on
	// does not hold in this file. The caller decides what to do about it; the walk
	// itself never guesses.
	suspect bool
}

// segWalk is what one pass over ONE segment learned.
type segWalk struct {
	// records is the number of complete, verified records in the segment.
	records int64
	// lastSeq is the Seq the segment's last complete record should carry, which is
	// base-1 for a segment holding none.
	lastSeq int64
	// skipped counts records read and verified but not retained.
	skipped int
	// suspect is this segment's contribution to logWalk.suspect.
	suspect bool
	// torn reports bytes after the last complete record: a crash mid-write. Open
	// uses it to seal the segment rather than append behind the fragment.
	torn bool
	// end is the byte offset immediately after the last complete record.
	end int64
}

// walkSegment reads one segment front to back, verifying every record, and retains
// only those whose sequence is past afterSeq.
//
// A segment declares where its sequence space starts, so the per-record arithmetic
// is seq = base + ordinal - 1. For a log that has never rotated the base is 1 and
// every expression below is character-for-character what it was before segments
// existed — which is both the backward-compatibility argument and the correctness
// one. The skip's invariant was never really "ordinal equals sequence"; it was "the
// file declares where its sequence space starts and its records agree", and a
// single file declared it by being the only file.
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
// The skip rests on a record's sequence being the one its segment's declared base
// and its position imply. That is a property of the files this package writes —
// Writer.append increments seq and writes one record under one mutex — and not a
// law, so it is checked rather than trusted: record 1 is parsed and must carry the
// declared base, every record from the boundary onward is parsed, and each parsed
// record must carry the sequence the arithmetic predicts. A disagreement sets
// suspect and the caller re-reads the set whole.
//
// The base comes from the header and the name, never from record 1. Deriving it
// from the data would make the data unable to disagree with itself: the anchor
// would be deleted in effect while appearing to be present, and a segment whose
// record 1 was altered, or two writers' records interleaved under O_APPEND, would
// be accepted without a murmur.
//
// A v1 headerless segment parses every record regardless. It has no checksum, so
// "decodes" is its only integrity signal and the only guard against a misframed
// walk after a damaged length prefix (see the break below). v1 therefore gets the
// allocation saving and not the parse saving, and its invariant check is total
// rather than sampled.
//
// afterSeq of 0 retains everything and behaves exactly as a full parse, record for
// record and error for error.
func walkSegment(seg segment, afterSeq int64, out *[]Entry) (segWalk, error) {
	sw := segWalk{lastSeq: seg.base - 1, end: seg.headerBytes()}
	f, err := os.Open(seg.path)
	if err != nil {
		if os.IsNotExist(err) {
			// Retention runs in the same process on the checkpoint cadence, so a read
			// that enumerated and then opened can lose a segment between the two. It
			// is never a gap — retention deletes oldest-first and never the active
			// segment — so the answer is to start the read again rather than to report
			// a hole that does not exist.
			return sw, errSegmentVanished
		}
		return sw, err
	}
	defer f.Close()
	size := seg.size
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	if hb := seg.headerBytes(); hb > 0 {
		if _, err := f.Seek(hb, io.SeekStart); err != nil {
			return sw, err
		}
	}
	r := bufio.NewReader(f)
	// A header means every record carries a CRC; its absence means a v1 segment,
	// read without checksums so an upgrade does not lose the ability to recover —
	// those records simply cannot be verified.
	checksummed := seg.checksummed()

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
		seq := seg.base + ordinal - 1
		if _, err := io.ReadFull(r, hdr[:4]); err != nil {
			break // clean EOF or torn length prefix — stop at the last complete record
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		// Bound before allocating: this length came off disk and a flipped bit in it
		// must not turn recovery into a multi-gigabyte allocation.
		if n == 0 || n > MaxRecordBytes {
			return sw, fmt.Errorf("%w: %s record %d (sequence %d) declares %d bytes (limit %d)",
				ErrCorrupt, seg.name, ordinal, seq, n, MaxRecordBytes)
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
				// everything after it. The record is named by its segment, its ordinal
				// WITHIN that segment counting the ones this walk skipped, and the
				// sequence the two imply: RUNBOOKS tells an operator to recover the
				// records before the corrupt one, and across a set of segments a bare
				// per-file ordinal is ambiguous while a sequence is not.
				return sw, fmt.Errorf("%w: %s record %d (sequence %d) checksum %08x, want %08x",
					ErrCorrupt, seg.name, ordinal, seq, got, want)
			}
		}

		// Parse record 1 (the anchor: a segment whose first record does not carry the
		// base it declares must be caught before any record is discarded), everything
		// from the boundary onward, and — on a v1 segment — everything.
		parse := afterSeq == 0 || !checksummed || ordinal == 1 || seq >= afterSeq
		retain := seq > afterSeq
		if parse {
			var e Entry
			if err := json.Unmarshal(buf, &e); err != nil {
				if checksummed {
					// The checksum passed, so these are the bytes that were written and
					// they are not a record. That is a bug or a format mismatch, not
					// media corruption, and it must not be swallowed.
					return sw, fmt.Errorf("%w: %s record %d (sequence %d) passed its checksum but does not decode: %v",
						ErrCorrupt, seg.name, ordinal, seq, err)
				}
				break // v1 log: unverifiable, so an undecodable record is treated as the tail
			}
			lastParsed, sw.lastSeq = ordinal, e.Seq
			if e.Seq != seq {
				sw.suspect = true
			}
			if retain {
				*out = append(*out, e)
			}
		}
		if !retain {
			sw.skipped++
		}
		sw.records = ordinal
		sw.end += int64(len(hdr[:4])) + int64(n)
		if checksummed {
			sw.end += 4
		}
		cur, prev = prev, buf
	}

	// The last record of a skipping walk is usually parsed already — the boundary
	// rule reaches the end of the segment. It is not when the segment stops short of
	// the boundary, which is exactly the case worth reporting, so parse the retained
	// tail payload for its sequence. A v1 segment never reaches here: it parses every
	// record, so lastParsed is always the last one.
	if checksummed && sw.records > 0 && lastParsed != sw.records {
		var e Entry
		last := seg.base + sw.records - 1
		if err := json.Unmarshal(prev, &e); err != nil {
			return sw, fmt.Errorf("%w: %s record %d (sequence %d) passed its checksum but does not decode: %v",
				ErrCorrupt, seg.name, sw.records, last, err)
		}
		sw.lastSeq = e.Seq
		if e.Seq != last {
			sw.suspect = true
		}
	}
	sw.torn = sw.end < size
	return sw, nil
}

// walkSet reads a whole segment set in base order, verifying every record of every
// retained segment, and retains only those past afterSeq.
//
// The set is enumerated and structurally validated before a record is read, so a
// missing or renamed segment is reported as what it is rather than as a corrupt
// record four hundred thousand records later. Contiguity is the one check that
// cannot be made from the directory alone, because base(S₍ᵢ₊₁₎) has to be compared
// against last(Sᵢ); it is made as the walk crosses each boundary, which is still
// before the following segment's records reach the engine.
func walkSet(stem string, afterSeq int64) (logWalk, error) {
	var w logWalk
	err := retryWhileVanishing(func() error {
		set, err := enumerateSet(stem)
		if err != nil {
			w = logWalk{present: set != nil && set.present}
			return err
		}
		w, err = walkSetIn(set, afterSeq)
		return err
	})
	return w, err
}

// errSegmentVanished is retention deleting a segment under a reader's feet. It never
// escapes this package: every entry point that can meet it restarts its read.
var errSegmentVanished = errors.New("wal: segment vanished mid-read")

// retryWhileVanishing runs a read that enumerates the set, restarting it if
// retention deleted something while it ran.
//
// This is how term (e) of the retention predicate is satisfied — by making the
// READER restartable rather than by making retention wait. No lease, no refcount, no
// lock, and nothing that could let a stuck consumer stop a venue reclaiming disk.
// Retention deletes oldest-first, one prefix per checkpoint interval, and never the
// active segment, so a retry always converges; the bound exists so that a genuine
// filesystem problem surfaces as an error rather than as a spin.
func retryWhileVanishing(read func() error) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		if err = read(); !errors.Is(err, errSegmentVanished) {
			return err
		}
	}
	return fmt.Errorf("wal: segments kept disappearing while reading the set: %w", err)
}

// walkSetIn is walkSet over an already-enumerated set, so a caller that had to
// enumerate first — Recover, which validates the set against its snapshot before
// reading anything — does not pay for a second directory read, and so the fallback
// re-reads the same files the first pass saw.
func walkSetIn(set *segmentSet, afterSeq int64) (logWalk, error) {
	w := logWalk{present: set.present}
	for i, seg := range set.segs {
		sw, err := walkSegment(seg, afterSeq, &w.entries)
		w.records += sw.records
		w.skipped += sw.skipped
		if sw.suspect {
			w.suspect = true
		}
		w.lastSeq = sw.lastSeq
		if err != nil {
			return w, err
		}
		if i+1 < len(set.segs) {
			if err := contiguityError(seg, sw.lastSeq, set.segs[i+1]); err != nil {
				return w, err
			}
		}
	}
	return w, nil
}

// lastSeq returns the sequence of the last complete record of a set, which is what
// Open resumes from. Only the NEWEST segment is read: it walks and verifies every
// frame of that segment and decodes its first and last records, so an obgw restart
// no longer pays a second full parse of the log on top of Recover's.
//
// Reading only the newest is the decision docs/BOUNDED-RECOVERY.md §9.1 left open.
// It argued that Open must be the most permissive reader here — recovery's
// strictness moves with the snapshot boundary and Open does not know where the
// boundary is, so an Open stricter than the laxest Recover could turn a successful
// recovery into a failed start, an outage manufactured by two readers of the same
// bytes disagreeing. It did not say whether Open must re-verify SEALED segments. It
// must not: cmd/obgw calls Recover and then Open on the same path, Recover has just
// read and verified every retained byte, and a second full pass would double the
// restart cost rotation exists to bound in order to re-check something checked
// seconds ago.
//
// So what Open checks of the SEALED segments is only what their directory entries can
// answer: their names, their headers, their declared bases, and that no two of them
// claim the same base. NOT contiguity — that needs the previous segment's last record
// sequence, which only comes from reading its records, which is exactly what this does
// not do. Open therefore succeeds on a set with a missing middle segment, an overlap,
// or a CRC-damaged sealed segment, all of which Recover and ReadAll refuse. Open is
// not an integrity check and must not be used as one: anything that opens a set it did
// not first Recover or ReadAll is extending a set nothing has checked.
func lastSeq(set *segmentSet) (int64, segWalk, error) {
	seg, ok := set.newest()
	if !ok {
		return 0, segWalk{}, nil
	}
	var discard []Entry
	sw, err := walkSegment(seg, retainNothing, &discard)
	if err != nil {
		return 0, sw, err
	}
	return sw.lastSeq, sw, nil
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
		restoreEntry(eng, e)
	}
}

// restoreEntry applies one record to an engine and reports whether its kind was
// recognised at all.
//
// The boolean is the whole reason this is a function rather than the body of
// RestoreAfter's loop. An unrecognised kind falls out of the switch and is
// silently discarded, which is correct for a FORWARD-compatibility skip — an older
// reader meeting a record a newer build wrote should ignore it and diverge
// detectably rather than refuse to start — and is indistinguishable from the bug
// where a kind this build writes itself has no arm here. TestEveryEntryKindReplays
// enumerates entryKindCount against this return value, so the second case is a
// test failure while the first stays a skip.
func restoreEntry(eng *matching.Engine, e Entry) bool {
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
			return true
		}
		s, err := types.NewStopOrder(e.StopOrder.Fresh(), e.StopPrice)
		if err != nil {
			return true
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
	case KindSetPhase:
		// Replay RE-RUNS the uncross, and it must. Restoring the phase field alone
		// would leave the crossed book that pre-open accumulated unresolved and the
		// auction's prints missing from a tape subscribers already received — the
		// very divergence this record exists to fix. The uncross is a pure function
		// of the book, the book at this point in the tape is reproduced by the
		// records before it, and replay mode suppresses only the minimum resting
		// time and the band-breach pause, neither of which is on the uncross path.
		//
		// An unparseable name is skipped rather than guessed at. That is the point
		// of writing the phase as a name: a phase this build has never heard of
		// leaves the venue in the last phase it does understand and diverges
		// detectably, where an ordinal would have decoded into a valid-looking
		// state nobody defined. See docs/JOURNAL-COMPLETENESS.md §4.1 and §4.3.
		if p, err := matching.ParseEngineState(e.Phase); err == nil {
			eng.SetPhase(p)
		}
	default:
		return false
	}
	return true
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
	// Segments counts the members of the set that were read. One means a log that
	// has never rotated.
	Segments int
	// RetainedBytes is the size of the set on disk — what this restart had to read,
	// and the number -wal-retain budgets. Restart time is O(this), not O(total
	// history), which is the whole point of rotation plus retention.
	RetainedBytes int64
	// Floor is the base sequence of the oldest retained segment, 1 until retention
	// has deleted something. It is the number an operator needs before deleting a
	// snapshot: below the floor the log cannot be replayed from the beginning,
	// because the beginning is not there.
	Floor int64
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

	// Semantics is matching.SemanticsVersion of THIS BUILD — the number the gate
	// compared everything against. It is in the report so a log line can name it
	// without the embedder importing pkg/matching to find out.
	Semantics int
	// SnapshotSemantics is the semantics the snapshot declares, 0 when there is no
	// snapshot or when it predates the stamp. It is a REPORT and never a gate: a
	// snapshot is not replayed, so restoring a book an older build actually had is
	// the documented upgrade procedure rather than an error. After an upgrade it
	// disagreeing with Semantics is the normal state, which is why it is reported
	// rather than refused.
	SnapshotSemantics int
	// LogSemantics is the distinct set of semantics the segments on disk declare,
	// ascending, with 0 meaning "declares none". More than one entry is not a
	// corruption: a set legitimately spans an upgrade, and that is precisely the fact
	// the gate needed.
	LogSemantics []int
	// SemanticsAccepted reports that records from a build other than this one were
	// replayed because RecoverOptions.AcceptSemantics named their version. If this is
	// routinely true in the field, the number has become a formality and the
	// diagnosis is that the refusal is firing on cases it was designed not to.
	SemanticsAccepted bool
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
// Restart time is O(RETAINED log), which is a number the operator chose — and it is
// only bounded if they chose one. Reading and verifying every retained byte is still
// linear in what is on disk; rotation cuts the log into segments and Retain deletes a
// prefix of them once a verified snapshot covers it, but deletion is off by default.
// Without it, a venue that runs continuously still gets slower to restart every day
// it stays up. See docs/LOG-ROTATION.md and docs/PRODUCTION-READINESS.md, "Running
// continuously".
//
// walPath names a SET: the stem plus its numbered segments. Recovery enumerates them
// in base order and refuses to start on a set with a hole in it — a missing middle
// segment, an overlap, or a snapshot that sits below the oldest retained sequence —
// because every one of those recovers into a plausible book that is missing commands,
// with every remaining record verifying perfectly.
//
// It also refuses on a fourth condition, where the bytes are intact, the files are all
// present, and a MEANING has moved: records it is about to replay were written by a
// build whose matching behaviour is not this one's. That is ErrSemanticsMismatch, and
// RecoverWithOptions is where the deliberate override lives.
func Recover(config matching.Config, snapPath, walPath string) (*matching.Engine, error) {
	eng, _, err := RecoverWithOptions(config, snapPath, walPath, RecoverOptions{})
	return eng, err
}

// RecoverWithReport is Recover plus what the read saw. Recover is this function with
// the report dropped; embedders that want to log the conditions in RecoverReport —
// cmd/obgw does — call this instead.
func RecoverWithReport(config matching.Config, snapPath, walPath string) (*matching.Engine, RecoverReport, error) {
	return RecoverWithOptions(config, snapPath, walPath, RecoverOptions{})
}

// RecoverWithOptions is RecoverWithReport with the deliberate deviations named.
//
// The only deviation there is, and the only one this function will ever grow, is
// RecoverOptions.AcceptSemantics: the list of matching semantics versions whose
// records this recovery will replay besides this build's. Recover and
// RecoverWithReport are this function with the zero options, which is the default and
// refuses.
//
// It refuses if and only if it is about to APPLY a record from a segment whose
// declared semantics is not this build's. A mismatched segment whose records are all
// covered by the snapshot is read, CRC-verified, skipped, reported and never refused
// — it contributes nothing to the recovered book, so refusing on it is refusing on a
// file that could be deleted with no effect. That is the same move
// docs/BOUNDED-RECOVERY.md §5.2 already made when it let the decode check travel with
// the snapshot boundary. See docs/SEMANTICS-VERSION.md §3.
func RecoverWithOptions(config matching.Config, snapPath, walPath string, opts RecoverOptions) (*matching.Engine, RecoverReport, error) {
	rep := RecoverReport{Semantics: matching.SemanticsVersion}
	// The snapshot first, always: a venue whose base is unreadable should say so
	// without first spending a minute reading a log it is not going to use.
	snap, err := ReadSnapshot(snapPath)
	if err != nil {
		return nil, rep, err
	}
	var after int64
	if snap != nil {
		after = snap.WALSeq
		rep.SnapshotSemantics = snap.Semantics
	}
	rep.SnapshotSeq = after

	// The set is enumerated and validated against the snapshot before a record is
	// read. The floor check is the tripwire every retention bug trips: if the oldest
	// retained sequence is more than one past the snapshot's WALSeq, the commands in
	// between are in no file this venue can read, and recovering anyway would produce
	// a plausible book that skipped them with no error anywhere.
	var walk logWalk
	if err := retryWhileVanishing(func() error {
		set, err := enumerateSet(walPath)
		if err != nil {
			return err
		}
		rep.Segments = len(set.segs)
		rep.RetainedBytes = set.bytes()
		rep.Floor = set.floor()
		rep.FellBack = false
		rep.LogSemantics = logSemantics(set)
		rep.SemanticsAccepted = false
		if err := set.validateAgainstSnapshot(after, snap != nil); err != nil {
			return err
		}
		// Stage one of the semantics gate, before a byte of any record is read. A
		// sealed segment's span comes from the 16-digit filenames alone, so a
		// mismatch that would be replayed is refused in milliseconds rather than
		// after reading gigabytes. It joins the startup validations above and is
		// checked AFTER them: a gap or an overlap is a more fundamental condition
		// than a version, and the override must not be able to mask one.
		if err := gateSealedSegments(set, after, opts); err != nil {
			return err
		}
		if walk, err = walkSetIn(set, after); err != nil {
			return err
		}
		if after > 0 && walk.suspect {
			// A parsed record's sequence was not the one its segment's declared base
			// implies, so the records this walk already discarded may be exactly the
			// ones that needed applying. Re-read from byte zero rather than guess an
			// offset: continuing forward would turn a detected anomaly into undetected
			// data loss.
			rep.FellBack = true
			if walk, err = walkSetIn(set, 0); err != nil {
				return err
			}
		}
		// Stage two: the active segment, decided from its last complete record's
		// sequence rather than from the directory, because the directory does not
		// bound it above. Still before RestoreEngine and before a single command is
		// applied.
		if err := gateNewestSegment(set, after, walk.lastSeq, opts); err != nil {
			return err
		}
		rep.SemanticsAccepted = replaySemanticsDiffer(set, after, walk.lastSeq)
		return nil
	}); err != nil {
		return nil, rep, err
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
