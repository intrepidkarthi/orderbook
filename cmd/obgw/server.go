package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Config configures the reference server.
type Config struct {
	Addr string
	// Symbol is the instrument a one-book venue serves. Prefer Symbols for more
	// than one; setting either is enough, and applyDefaults keeps them in step.
	Symbol string
	// Symbols is the venue's instrument list. Each becomes an independent book:
	// its own matching goroutine, command log, market-data feed and rate gate,
	// with ids partitioned so they stay unique across all of them. More than one
	// requires DataDir. See docs/MULTI-SYMBOL.md.
	Symbols []string
	// DataDir holds the venue manifest and, for a multi-symbol venue, one log and
	// snapshot per instrument. A one-book venue keeps using WALPath and
	// SnapshotPath unchanged.
	DataDir     string
	Incarnation string
	// Accounts maps username to password. Authentication defaults to DENY: an
	// empty map rejects every login rather than admitting everyone, because the
	// failure mode of the other default is an open venue.
	//
	// Ignored when Auth is set. Prefer Auth: this field holds plaintext secrets in
	// the process, and if it came from a command line it is also in `ps` output for
	// every user on the host.
	Accounts map[string]string
	// Auth decides logins. When nil, one is built from Accounts.
	//
	// This is the seam a real deployment replaces. Credential storage is the one
	// decision a library must not make for you — see orderentry.Authenticator.
	Auth orderentry.Authenticator
	// TLS, when set, wraps every listener. Nil means the venue speaks plaintext and
	// sends credentials in the clear, which is a thing to do on a loopback interface
	// during development and nowhere else.
	TLS *tls.Config
	// OutboundDepth bounds each connection's send queue. A client that stops
	// reading is disconnected rather than allowed to back up into the venue.
	OutboundDepth int
	StreamRing    int
	RatePerSec    float64
	Burst         float64
	// WALPath, when set, turns on durability: every command is written to the log
	// before it is applied, and the server recovers from it on start.
	WALPath string
	// SnapshotPath is where periodic checkpoints are written. Recovery APPLIES only
	// the log tail after the snapshot, and PARSES only that tail — but it still reads
	// and checksum-verifies every RETAINED byte, so this bounds replay work and
	// recovery's allocation. What bounds restart TIME is WALRetainBytes, because that
	// is what bounds how much log there is to read. See wal.Recover.
	SnapshotPath string
	// WALSegmentBytes is the size at which the log rotates into a new segment. Zero
	// takes wal.DefaultMaxSegmentBytes (128 MiB); negative disables rotation.
	WALSegmentBytes int64
	// WALRetainBytes is the byte budget for the retained log. Zero — the default —
	// keeps everything, which is the behaviour every earlier release had, with
	// better file names. Deleting a venue's journal is not something anybody should
	// acquire by upgrading.
	//
	// It converts directly into restart time: reading and CRC-verifying costs about
	// 2 s per GiB cold on the hardware docs/BENCHMARKS.md was measured on, so an
	// operator who wants a one-second cold restart budget picks about 500 MiB, plus
	// O(book) for the snapshot.
	//
	// It is a budget, not a bound, and WALRetainSegments is what puts a floor under
	// it. The retained set never falls below (WALRetainSegments + 1) x
	// WALSegmentBytes, which at the defaults of 4 and 128 MiB is 640 MiB — so 500 MiB
	// here yields 640 MiB and roughly 1.3 s unless WALSegmentBytes comes down with it.
	// A venue that wants a small retained set wants small segments: 500 MiB of budget
	// against 16 MiB segments has an 80 MiB floor and does what it says.
	WALRetainBytes int64
	// WALAcceptSemantics lists the matching semantics versions whose records recovery
	// may replay besides this build's, from -wal-accept-semantics. Empty is the
	// default and refuses: a journal written by a build whose matching behaviour is
	// not this one's replays into a book the venue that wrote it never had, and
	// recovery says so and stops rather than starting confidently.
	//
	// It names versions rather than being a boolean so it goes stale on the next
	// bump; see wal.RecoverOptions.AcceptSemantics.
	WALAcceptSemantics []int
	// WALRetainSegments is how many sealed segments are kept regardless of coverage.
	// Zero takes wal.DefaultMinSegments.
	//
	// It is checked AFTER the byte budget and it wins, so it is the term that decides
	// the smallest the retained set can be. See WALRetainBytes.
	WALRetainSegments int
	// WALArchiveDir, when set, receives a byte-identical copy of every segment
	// before it is deleted. A venue running retention WITHOUT archival has a
	// recovery point equal to its newest snapshot and is one corrupt snapshot away
	// from nothing, which is why this is the first flag to set after WALRetainBytes.
	WALArchiveDir string
	// WALMinFree is the low-water mark: below it the venue warns and runs retention
	// immediately instead of waiting for the next checkpoint. Zero takes 2 GiB.
	WALMinFree int64
	// WALMinFreeStop is the stop-water mark: below it every book goes cancel-only.
	// Zero takes 256 MiB.
	//
	// Cancel-only rather than halt, and the reason is the client's rather than the
	// venue's: a venue that stops accepting EVERYTHING leaves participants holding
	// positions they cannot withdraw, on a venue whose disk is about to fill.
	// Cancel-only lets them get flat and removes the largest source of log growth.
	// It does not stop the log growing, because a cancel is a record too, which is
	// why the halt on a failed sync exists below it and is not optional.
	WALMinFreeStop int64
	// CheckpointEvery bounds how much log a restart must replay. Zero disables
	// checkpointing, which is legal but means replay grows without limit.
	CheckpointEvery time.Duration
	// SyncEveryCommand fsyncs each command before it is applied, so durability
	// precedes acknowledgement. Off by default: it puts a disk write in the
	// matching path and costs roughly 210× the group-committed default. Leaving
	// it off means an acknowledged order can be lost if the process dies inside
	// the 20ms sync window — see syncingLog and pkg/wal's package comment.
	SyncEveryCommand bool
	// Profiling mounts net/http/pprof on the admin listener. Off by default: a heap
	// dump is a snapshot of everything the venue holds, and /debug/pprof/profile
	// costs 30 seconds of CPU. Turn it on when you need to ask the process what it
	// is retaining, which is a question metrics cannot answer.
	Profiling bool
	// MDAddr, when set, starts a market-data listener on its own port. Order entry
	// is authenticated and per-account; market data is anonymous and identical for
	// everyone. Sharing one port would put an unauthenticated subscriber on the same
	// code path as order entry, which is the wrong default however carefully it is
	// written.
	MDAddr string
	// SessionClose reports when the trading session ends, which is what a DAY order
	// expires at. Nil means the venue has declared no session and DAY orders are
	// refused.
	SessionClose func() time.Time
	// ExpireEvery drives the expiry sweep. The engine expires lazily on command
	// arrival, which is enough for correctness — an expired order is gone before it
	// could match — but in a quiet market a market-data subscriber would see depth
	// that should have left. Zero disables the ticker.
	ExpireEvery time.Duration
	// MDRetain bounds the market-data gap-fill ring. A subscriber further behind than
	// this is told to resubscribe from a snapshot rather than served a partial answer.
	MDRetain int
	// AdminAddr, when set, starts an HTTP listener serving /metrics, /healthz and
	// /readyz on its own port. Empty means the venue trades unobserved, which is
	// legal here and is not something to do on purpose — see admin.go.
	AdminAddr string
}

func (c *Config) applyDefaults() {
	if c.Symbol == "" && len(c.Symbols) == 0 {
		c.Symbol = "X"
	}
	// Symbols is the venue's instrument list; Symbol is the one-instrument spelling
	// of the same thing and stays supported, because every existing deployment and
	// every test uses it. Keeping them in step here rather than at each use site
	// means nothing downstream has to know which one the caller set.
	if len(c.Symbols) == 0 {
		c.Symbols = []string{c.Symbol}
	}
	if c.Symbol == "" {
		c.Symbol = c.Symbols[0]
	}
	if c.OutboundDepth <= 0 {
		c.OutboundDepth = 1024
	}
	if c.StreamRing <= 0 {
		c.StreamRing = 8192
	}
	if c.RatePerSec <= 0 {
		c.RatePerSec = 1000
	}
	if c.Burst <= 0 {
		c.Burst = 200
	}
	if c.Incarnation == "" {
		c.Incarnation = "INC0000001"
	}
	if c.MDRetain <= 0 {
		c.MDRetain = 1 << 16
	}
	if c.ExpireEvery <= 0 {
		c.ExpireEvery = time.Second
	}
	if c.WALMinFree == 0 {
		c.WALMinFree = 2 << 30
	}
	if c.WALMinFreeStop == 0 {
		// Far more than a clean shutdown needs — flush a buffer, fsync a segment,
		// write a snapshot, fsync a directory — and chosen to be obviously sufficient
		// rather than tuned. What the rule is about is that the number is not zero and
		// is not the same as the threshold at which writes actually start failing.
		c.WALMinFreeStop = 256 << 20
	}
}

// walOptions is the segment-set policy every book's Writer is opened with.
func (c *Config) walOptions() wal.Options {
	return wal.Options{
		MaxSegmentBytes: c.WALSegmentBytes,
		RetainBytes:     c.WALRetainBytes,
		MinSegments:     c.WALRetainSegments,
		ArchiveDir:      c.WALArchiveDir,
	}
}

// Server is an order-entry gateway over one or more instruments.
//
// Each symbol is an independent book — its own matching goroutine, command log,
// market-data feed and rate gate — and the only thing shared across them is the id
// space, partitioned so that sharing costs no coordination. There is deliberately
// no order of events ACROSS symbols: a venue-wide sequence needs a serialisation
// point every command passes through, which is the bottleneck sharding exists to
// remove. See docs/MULTI-SYMBOL.md.
//
// The account layer is venue-wide and singular on purpose: one Registry, one
// publisher, one stream per account. A client holds one order-entry session and
// sees one ordered conversation, whatever mix of instruments it trades — which is
// what makes a client id enough to name an order without also naming a symbol.
type Server struct {
	cfg   Config
	books *bookSet
	// manifest binds each symbol to its id-space partition. Nil for a one-book
	// venue, which uses shard 0 and therefore the small dense ids it always had.
	manifest *matching.Manifest

	// runner, gate, feed and wal resolve to the FIRST book. They predate
	// multi-symbol and are kept because a one-book venue is still the common case
	// and every caller of them means "the book" — but anything that must work
	// across instruments goes through s.books instead.
	runner *matching.Runner
	gate   *gateway.Gateway
	reg    *orderentry.Registry
	pub    *orderentry.Publisher
	feed   *marketdata.Feed

	// metrics is a sink like the others, so it counts exactly what the book saw
	// rather than what the gateway believed it sent.
	auth      orderentry.Authenticator
	metrics   *observability.Collector
	applyHist *observability.Histogram
	admin     admin

	// What the venue refused, and what it waits on — docs/LAG-AND-SHED.md.
	//
	// refused is indexed by wire reason code, resolved once at startup, so a refusal
	// costs an array index and one atomic add rather than a map lookup under a lock
	// on the path that runs hardest exactly when the venue has least to spare.
	refused        [refusedSeriesCount]*observability.Counter
	shedUnreported *observability.Counter
	// loginRefused counts refusals taken BEFORE a session exists, which speak a
	// different vocabulary (soup reject bytes, not wire reason codes) and so cannot
	// be folded into refused above. Separate metric, not a fudged label — see
	// docs/LAG-AND-SHED.md §4.6.
	loginRefused map[byte]*observability.Counter
	// The three histograms are venue-wide rather than per book, and deliberately:
	// observability.Histogram has no label support, and a checkpoint's or an fsync's
	// cost is mostly the DEVICE's, which every book on this node shares. Which book
	// stopped is answered by the labelled gauges instead.
	appendHist *observability.Histogram
	syncHist   *observability.Histogram
	snapHist   *observability.Histogram
	// startedAt is what a snapshot age counts from when checkpointing is configured
	// and no file has landed yet. Not what it counts from once one has — see
	// Server.snapshotAgeSeconds.
	startedAt time.Time

	wal      *wal.Writer
	ln       net.Listener
	mdLn     net.Listener
	wg       sync.WaitGroup
	quit     chan struct{}
	closeOne sync.Once

	// walFailed is set when any book's log stops being writable. It fails readiness
	// venue-wide, because a node that cannot journal one of its books is a node the
	// orchestrator should take out of rotation rather than one it should keep half
	// of. walFree is the last free-space sample, exported as a gauge.
	walFailed atomic.Bool
	walFree   atomic.Int64
	// walStopped records that the stop-water mark put the venue into cancel-only. It
	// is a separate fact from the engine's trading state, which an operator can also
	// change by hand, and it is readable from any goroutine — Engine.State is not.
	walStopped atomic.Bool

	// live connections, so shutdown can unblock handlers parked in a read.
	// Closing the listener stops new accepts but does nothing to established
	// sockets, so without this Close waits forever on its own handlers.
	connMu sync.Mutex
	conns  map[net.Conn]struct{}
}

// NewServer builds a server and its engine. With WALPath set it recovers from
// disk first, then serves — which is the whole point of shipping a log.
func NewServer(cfg Config) (*Server, error) {
	started := time.Now()
	cfg.applyDefaults()

	reg := orderentry.NewRegistry(cfg.Incarnation, cfg.StreamRing)
	pub := orderentry.NewPublisher(reg, 1<<15)
	// The collector is a sink like the other two, so it counts what the book saw
	// rather than what the gateway believed it sent. Attaching it here rather than
	// wrapping the handlers is the difference between metrics that can disagree with
	// the engine and metrics that cannot.
	col := observability.NewCollector()
	// The naming index goes FIRST, and the order is load-bearing. It runs on the
	// matching goroutine and makes an order addressable by its client's own id the
	// moment the engine accepts it; everything after it may lag. Behind the publisher
	// — which is where this lived until a soak found it — a cancel arriving while the
	// pump was behind was refused for an order that was live in the book, and the
	// client, correctly, never asked again. See pkg/orderentry/nameindex.go.
	//
	// MultiSink rather than replacing the publisher: Config.EventSink is a single
	// slot, so attaching the feed on its own would silently stop every execution
	// report the order-entry side depends on.
	// One book per symbol. Recovery, adoption and the log are per instrument,
	// because each is independently consistent: no invariant spans two books, so
	// there is no venue-wide recovery step and nothing a skewed set of restarts
	// could violate (docs/MULTI-SYMBOL.md §4.4).
	set := newBookSet()
	// The durability histograms are registered only by a venue that has a log. A
	// venue with none has nothing to time, and exporting two histograms that can
	// only ever read zero would put two more families on a page §14 already worries
	// is getting long.
	var appendHist, syncHist *observability.Histogram
	var manifest *matching.Manifest
	if len(cfg.Symbols) > 1 {
		if cfg.DataDir == "" {
			return nil, fmt.Errorf("obgw: %d symbols need DataDir", len(cfg.Symbols))
		}
		var err error
		if manifest, err = matching.LoadManifest(cfg.manifestPath()); err != nil {
			return nil, fmt.Errorf("obgw: manifest: %w", err)
		}
	}

	for _, symbol := range cfg.Symbols {
		shardIndex := 0
		if manifest != nil {
			var err error
			if shardIndex, err = manifest.IndexFor(symbol); err != nil {
				return nil, fmt.Errorf("obgw: shard index for %s: %w", symbol, err)
			}
		}
		// A feed per book: market data is per instrument and a subscription names
		// exactly one (wire v4), so one shared feed would interleave two books into
		// a single sequence space that no subscriber could untangle. The feed shares
		// the venue's incarnation — both edges describe the same run, and a
		// subscriber reconnecting after a restart is refused for the same reason an
		// order-entry client is.
		feed := marketdata.NewFeed(cfg.Incarnation, cfg.MDRetain)
		// The naming index goes FIRST, and the order is load-bearing. It runs on the
		// matching goroutine and makes an order addressable by its client's own id
		// the moment the engine accepts it; everything after it may lag. Behind the
		// publisher — which is where this lived until a soak found it — a cancel
		// arriving while the pump was behind was refused for an order that was live
		// in the book, and the client, correctly, never asked again.
		//
		// The index, the publisher and the collector are venue-wide and shared by
		// every book; only the feed is per instrument.
		sink := matching.MultiSink{orderentry.NewNameIndex(reg), pub, feed, col}

		eng := matching.DefaultConfig(symbol)
		eng.DedupClientOrderIDs = 4096
		eng.SessionClose = cfg.SessionClose
		eng.ShardIndex = shardIndex

		walPath, _ := cfg.paths(symbol)
		// Refused at startup rather than at the first checkpoint tick. An archive
		// directory that IS the log directory destroys every segment it reports having
		// archived, and the moment an operator finds that out should not be the moment
		// the disk is filling. wal.Retain refuses it again on its own account.
		if walPath != "" {
			if err := wal.CheckArchiveDir(cfg.WALArchiveDir, filepath.Dir(walPath)); err != nil {
				return nil, fmt.Errorf("obgw: %s: %w", symbol, err)
			}
		}
		var (
			w         *wal.Writer
			recovered *matching.Engine
			// recoveredThrough is the log sequence the recovered engine stands at: the
			// last record folded into it, counting the snapshot and the replayed tail
			// together. It is what the Runner must be told, because it is what the next
			// checkpoint stamps and the restart after that replays from. Zero for a
			// venue with no log, which is also where a fresh one starts.
			recoveredThrough int64
		)
		// recoveredIn is how long this book took to go from "we have a configuration"
		// to "this book can run": the recovery read, BOTH adoptions, and opening the
		// log for appending. NaN for a venue with no log, which did not recover —
		// zero would read as "recovered instantly", which is the one answer that is
		// never true. See registerGauges.
		recoveredIn := math.NaN()
		if walPath != "" {
			if appendHist == nil {
				appendHist = col.Histogram(walAppendLatencyMetric)
				syncHist = col.Histogram(walSyncLatencyMetric)
			}
			// The clock starts HERE, not around wal.RecoverWithOptions alone. The
			// operator's question is "how long was my venue down", and the two Adopts
			// below are O(book): at 100 K orders they are not a rounding error on a
			// 174 ms recovery. Measuring the narrow interval would produce a number
			// reliably smaller than the truth, which is the worst kind of wrong for a
			// figure that feeds a recovery time objective.
			//
			// It excludes process start, flag parsing and listener bind. Those are
			// constant, and they are not what grows.
			recoveryStart := time.Now()
			// Recover with NO event sink attached. Replaying the log re-emits every
			// historical event, and a publisher attached during recovery would fan a
			// lifetime of executions at whoever connected next.
			_, snapPath := cfg.paths(symbol)
			var err error
			var rep wal.RecoverReport
			opts := wal.RecoverOptions{AcceptSemantics: cfg.WALAcceptSemantics}
			if recovered, rep, err = wal.RecoverWithOptions(eng, snapPath, walPath, opts); err != nil {
				return nil, fmt.Errorf("obgw: recover %s: %w", symbol, err)
			}
			// The larger of the two, not the log's last record alone: after a crash a
			// checkpoint can be durable while records it covers are still buffered, so
			// the snapshot is legitimately ahead of the log (rep.SnapshotAhead) and its
			// position is the one the engine actually holds.
			recoveredThrough = rep.LogLastSeq
			if rep.SnapshotSeq > recoveredThrough {
				recoveredThrough = rep.SnapshotSeq
			}
			if n := recovered.OrderCount(); n > 0 {
				log.Printf("obgw: %s recovered %d resting orders from %s (%d records applied, %d read past)",
					symbol, n, walPath, rep.Applied, rep.Skipped)
			}
			// Two conditions that leave the recovered book correct and still say
			// something about the files. Neither is a reason to refuse to start; both
			// are things an operator should be told rather than left to infer.
			if rep.SnapshotAhead {
				log.Printf("obgw: %s snapshot is stamped at log sequence %d but the log ends at %d — "+
					"the missing records are already folded into the snapshot, so this recovery is correct, "+
					"but the venue will reuse those sequence numbers until the next checkpoint lands. "+
					"Force a checkpoint to close it.", symbol, rep.SnapshotSeq, rep.LogLastSeq)
			}
			if rep.SemanticsAccepted {
				// Loud, because this is a venue serving a book built by replaying
				// another build's rules. It is a deliberate act and it should stop
				// being needed after the next checkpoint.
				log.Printf("obgw: %s REPLAYED RECORDS FROM ANOTHER MATCHER. This build matches at semantics %d; "+
					"the log declares %v, and -wal-accept-semantics let it through. The recovered book is what "+
					"THIS build's rules produce from those commands, which is not necessarily what the venue that "+
					"wrote them served. Force a checkpoint and remove the flag. "+
					"See docs/RUNBOOKS.md \"Upgrading across a semantics change\".",
					symbol, rep.Semantics, rep.LogSemantics)
			} else if len(rep.LogSemantics) > 1 || (len(rep.LogSemantics) == 1 && rep.LogSemantics[0] != rep.Semantics) ||
				(rep.SnapshotSeq > 0 && rep.SnapshotSemantics != rep.Semantics) {
				// A set that spans an upgrade, or a snapshot an older build wrote, with
				// nothing left to replay from the older half. That is the documented
				// upgrade path completing correctly, and it is reported rather than
				// refused — see docs/SEMANTICS-VERSION.md §3.1.
				log.Printf("obgw: %s matching semantics %d; snapshot declares %d, log segments declare %v — "+
					"nothing from another matcher was replayed.",
					symbol, rep.Semantics, rep.SnapshotSemantics, rep.LogSemantics)
			}
			if rep.FellBack {
				log.Printf("obgw: %s log record sequences are not the ones their segments' declared bases imply, "+
					"so recovery re-read the whole log rather than skipping the part the snapshot covers. The "+
					"recovered book is correct; the restart was slower than it needed to be. "+
					"See docs/BOUNDED-RECOVERY.md and docs/LOG-ROTATION.md.", symbol)
			}
			if rep.Floor > 1 {
				// Once retention has fired, "delete the snapshot and replay from the
				// beginning" stops being a procedure that works: the beginning is not
				// there. The floor is legible from ls, and it is said out loud at every
				// start so an operator reaching for that runbook has already seen it.
				log.Printf("obgw: %s retained log starts at sequence %d across %d segments (%.1f MiB) — "+
					"everything below that is deleted or archived, so the snapshot is the only base this log can be "+
					"joined to. See docs/RUNBOOKS.md \"A corrupt snapshot\".",
					symbol, rep.Floor, rep.Segments, float64(rep.RetainedBytes)/(1<<20))
			}
			// Rebuild the session layer's index over the recovered book. Recovery used
			// to restore the book and nothing else, which left every recovered order
			// unnameable: a client could see them in a Query reply and could not cancel
			// or reduce them, and a fill against one produced no execution report at all
			// because the publisher had no record of the order.
			reg.Adopt(recovered.RestingOrders())
			// Seed the market-data feed too, so a subscriber's first snapshot shows the
			// venue as it actually is. A feed starting empty against a recovered book
			// would show only what has changed since the restart, which is almost
			// nothing — the same shape of bug the session index had before v0.12.0.
			feed.Adopt(recovered.RestingOrders(), recovered.LastTradePrice())
			// Only now do the sinks go on, so live events are published and replayed
			// history is not.
			recovered.SetEventSink(sink)
			if w, err = wal.OpenWith(walPath, cfg.walOptions()); err != nil {
				return nil, fmt.Errorf("obgw: open wal %s: %w", symbol, err)
			}
			recoveredIn = float64(time.Since(recoveryStart).Nanoseconds())
		}

		eng.EventSink = sink

		// Only populate Log when there really is one. Assigning a nil *wal.Writer to
		// the CommandLog interface field yields a NON-nil interface holding a nil
		// pointer, so the Runner's `log != nil` check passes and the first command
		// dereferences nil. This is the standard Go typed-nil trap and it cost a
		// segfault on the first run with durability disabled.
		rc := matching.RunnerConfig{Engine: eng, QueueSize: 8192, LastApplied: recoveredThrough}
		if w != nil {
			// timedLog goes INSIDE syncingLog, never outside. Outside, the append
			// histogram would contain syncingLog's fsync and would be a copy of the
			// sync histogram under a different name, in the one mode where durability
			// matters most. See timedLog and TestAppendLatencyExcludesTheSync.
			timed := &timedLog{inner: w, hist: appendHist}
			if cfg.SyncEveryCommand {
				// The group-commit loop does not run in this mode, so the decorator's
				// own fsync is the only place sync latency can come from.
				rc.Log = &syncingLog{w: w, inner: timed, hist: syncHist}
			} else {
				rc.Log = timed
			}
		}
		// recovered is nil without a WAL, in which case NewRunnerFor builds a fresh
		// engine from the config. With one, the recovered book is what we serve —
		// building from the bare config here is how the first attempt silently threw
		// away everything it had just read back from disk. The book and the log
		// position travel together for the same reason: handing over the book and
		// leaving the position at zero is how a checkpoint taken before the first
		// order of the session came to claim it covered none of it.
		runner := matching.NewRunnerFor(recovered, rc)

		// The failure counter is registered even for a book that never fails one,
		// because a series that appears the first time it moves is a series no alert
		// was written against. Age says the recovery base is stale; this says why.
		var snapFailures *observability.Counter
		if _, snapPath := cfg.paths(symbol); snapPath != "" {
			snapFailures = col.Counter(snapshotFailuresMetric,
				"Checkpoints that could not be written, per instrument. The PREVIOUS snapshot stays in force and "+
					"retention still runs against it, so nothing is destroyed — what grows is the next restart. Alert on any increase.",
				observability.Label{Name: "symbol", Value: symbol})
		}
		set.add(&symbolBook{
			symbol: symbol, shardIndex: shardIndex,
			runner: runner, feed: feed, wal: w,
			gate:          gateway.New(runner, gateway.Config{Rate: cfg.RatePerSec, Burst: cfg.Burst}),
			snapFailures:  snapFailures,
			recoveredInNs: recoveredIn,
		})
	}
	primary := set.first()

	auth := cfg.Auth
	if auth == nil {
		auth = orderentry.NewStaticAccounts(cfg.Accounts)
	}

	srv := &Server{
		cfg:        cfg,
		books:      set,
		manifest:   manifest,
		auth:       auth,
		wal:        primary.wal,
		runner:     primary.runner,
		gate:       primary.gate,
		feed:       primary.feed,
		metrics:    col,
		applyHist:  col.Histogram(applyLatencyMetric),
		appendHist: appendHist,
		syncHist:   syncHist,
		startedAt:  started,
		reg:        reg,
		pub:        pub,
		quit:       make(chan struct{}),
		conns:      map[net.Conn]struct{}{},
	}
	// The snapshot-duration histogram belongs to the checkpoint loop, so it is
	// registered only by a venue that checkpoints at all.
	for _, b := range set.all() {
		if _, snapPath := cfg.paths(b.symbol); snapPath != "" {
			srv.snapHist = col.Histogram(snapshotDurationMetric)
			break
		}
	}
	// Seed the readiness path's cached snapshot mtime before anything can probe.
	//
	// This is what makes the first /readyz after a restart honest: a venue recovered
	// onto a base backdated two hours reports 7200 seconds on its first probe and is
	// degraded immediately, rather than reporting the freshest possible reading at the
	// moment it is at its most exposed.
	for _, b := range set.all() {
		srv.refreshSnapshotMTime(b)
	}
	srv.registerRefusalCounters()
	srv.registerGauges()
	return srv, nil
}

// Addr reports the bound address, valid after Listen.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Listen binds the socket(s) without serving, so a test can learn the port.
func (s *Server) Listen() error {
	ln, err := s.listen(s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	if s.cfg.MDAddr != "" {
		mdLn, err := s.listen(s.cfg.MDAddr)
		if err != nil {
			_ = ln.Close()
			return err
		}
		s.mdLn = mdLn
	}
	// The admin edge comes up with the sockets, not with the market: an operator
	// wants /metrics answering while the venue is still recovering its log, which is
	// exactly when they most want to know what it is doing.
	if err := s.startAdmin(); err != nil {
		_ = ln.Close()
		if s.mdLn != nil {
			_ = s.mdLn.Close()
		}
		return err
	}
	return nil
}

// listen binds addr, wrapped in TLS when the venue has been given a certificate.
//
// The handshake happens on the connection's own goroutine, inside the login deadline
// that already bounds a silent peer — so a client that completes a TCP connection and
// then stalls the handshake is dropped by the same timeout that drops one which
// connects and never logs in. A handshake on the accept loop would let one slow peer
// hold up every other connection.
func (s *Server) listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if s.cfg.TLS == nil {
		return ln, nil
	}
	return tls.NewListener(ln, s.cfg.TLS), nil
}

// MDAddr reports the bound market-data address, valid after Listen when configured.
func (s *Server) MDAddr() net.Addr {
	if s.mdLn == nil {
		return nil
	}
	return s.mdLn.Addr()
}

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	go s.pub.Pump()
	go s.expireLoop()
	if s.mdLn != nil {
		go func() {
			if err := s.serveMarketData(); err != nil {
				log.Printf("obgw: market data: %v", err)
			}
		}()
	}
	if s.durable() {
		// The group-commit loop is what creates the durability window. In
		// per-command mode every record is already on disk before it was
		// applied, so the ticker would only re-sync a synced file.
		if !s.cfg.SyncEveryCommand {
			go s.syncLoop()
		}
		if s.cfg.CheckpointEvery > 0 {
			go s.checkpointLoop()
		}
	}
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil // an expected close, not a failure
			default:
				return err
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// Close stops accepting, drops live connections, drains the matcher and stops the
// pump — in that order, so nothing is published after the streams stop being read
// and no producer is left waiting on a matcher that has gone.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.quit)
		if s.ln != nil {
			_ = s.ln.Close()
		}
		if s.mdLn != nil {
			_ = s.mdLn.Close()
		}
		// Established connections must be closed explicitly: handlers are parked
		// in a blocking read, and closing the listener does not reach them.
		s.connMu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.connMu.Unlock()
		// Before the wait, not after: the admin server's goroutine is in the same
		// WaitGroup, and http.Server.Serve does not return until Shutdown is called.
		s.closeAdmin()

		s.wg.Wait()
		for _, b := range s.books.all() {
			b.runner.Close()
		}
		s.pub.Close()
		// The logs are closed last, after every matcher has stopped producing, and
		// Close syncs — so a clean shutdown loses nothing.
		for _, b := range s.books.all() {
			if b.wal != nil {
				if err := b.wal.Close(); err != nil {
					log.Printf("obgw: %s wal close: %v", b.symbol, err)
				}
			}
		}
	})
}

// durable reports whether any book is journalled.
func (s *Server) durable() bool {
	for _, b := range s.books.all() {
		if b.wal != nil {
			return true
		}
	}
	return false
}

// expireLoop drives time-in-force expiry on a ticker, so a DAY or GTD order leaves
// the book at its deadline rather than at the next command. Correctness does not
// depend on it — an expired order is removed before anything can match against it —
// but market data would otherwise show depth that should have gone.
func (s *Server) expireLoop() {
	t := time.NewTicker(s.cfg.ExpireEvery)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			for _, b := range s.books.all() {
				b.runner.ExpireDue()
			}
		}
	}
}

// syncLoop group-commits the log. Syncing per command would put a disk write in
// the matching path; syncing never would make the log decorative. The interval is
// the durability window: a crash loses at most this much.
//
// A failed sync HALTS the book and fails readiness. It used to log the error and
// carry on, which is worse than it sounds: a full disk produced a venue that kept
// accepting orders, kept acknowledging them, kept matching them and stopped
// journalling, at fifty log lines a second with /readyz still green. Every
// acknowledgement after the first failed sync was a lie, and the venue was the only
// party that could have known.
//
// The honest limit, stated because it is the kind of thing that gets overclaimed:
// commands acknowledged inside the 20 ms window that ENDED in the failed sync were
// acknowledged before they were durable, and their fate is whatever the disk did.
// That is the existing, documented durability window (-sync-every-command closes
// it). A full disk does not widen it; it is just the moment it matters.
func (s *Server) syncLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			for _, b := range s.books.all() {
				if b.wal == nil {
					continue
				}
				// Timed here rather than inside pkg/wal, and timed even when it
				// fails: an fsync that returned an error still spent the time.
				//
				// The p99 of this histogram is not only a latency number. It is the
				// VARIABLE HALF of the venue's published recovery point objective:
				// this loop is one goroutine, so a sync that takes 200 ms delays the
				// next tick by 200 ms and the real window is 20 ms + this, not 20 ms.
				//
				// Its _count is also the only heartbeat this goroutine has. walFailed
				// latches on a sync that FAILS; a sync that never HAPPENS moves
				// nothing, and the venue would go on acknowledging orders it was not
				// making durable. The count advances at ~50/s on a completely quiet
				// venue, which is the point — a heartbeat that stopped when the market
				// went quiet would stop exactly when nobody is watching.
				start := time.Now()
				err := b.wal.Sync()
				if s.syncHist != nil {
					s.syncHist.Observe(time.Since(start))
				}
				if err != nil {
					s.failDurability(b, err)
				}
			}
		}
	}
}

// failDurability halts a book whose log can no longer be written, once.
//
// The Writer latches the failure, so this is idempotent by construction; the sync
// once here is about the log line and the halt, not about the state. Halting rather
// than degrading is the point: the venue stops being able to accept a command it
// cannot journal, and the orchestrator takes the node out of rotation because
// readiness fails. Clearing it needs a restart, which is where an operator decides
// whether the disk is actually fixed.
func (s *Server) failDurability(b *symbolBook, err error) {
	s.walFailed.Store(true)
	b.walFailOnce.Do(func() {
		log.Printf("obgw: %s WAL SYNC FAILED — halting the book and failing readiness. "+
			"Every command acknowledged since the last successful sync may not be durable, and no further "+
			"command will be accepted: %v", b.symbol, err)
		b.runner.Halt()
	})
}

// checkpointLoop is where a restart's cost is decided, in both of its halves.
//
// The snapshot bounds how much log a restart has to APPLY, and to parse and hold in
// memory. It is taken on the matching goroutine and stamped with the log position it
// is consistent with, so recovery replays only the tail after it.
//
// Retention bounds how much a restart READS, and it runs from here too, immediately
// after the snapshot it is predicated on becomes durable. That sentence is the whole
// of docs/LOG-ROTATION.md: a snapshot alone leaves the read O(total history), because
// every byte of the set is still opened and checksum-verified however recent the
// snapshot is. Deleting a prefix of the SET — the log is a set of segments, not one
// file — under a predicate that cannot outrun the snapshot is what makes the read
// O(retained log) instead, which is a number the operator chose with -wal-retain.
// Unset, which is the default, and it is still O(total history).
//
// It does not sync the log before writing the snapshot, so a checkpoint can be
// durable while records it covers are still in the writer's 20ms group-commit
// buffer. A crash in that window leaves a snapshot ahead of its log, which recovers
// correctly and is reported by wal.RecoverReport.SnapshotAhead. Closing the window
// means syncing the log first, which moves the checkpoint's pause cost onto the
// matching goroutine; see docs/BOUNDED-RECOVERY.md §4.1.
func (s *Server) checkpointLoop() {
	t := time.NewTicker(s.cfg.CheckpointEvery)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			// Per book, and they land at different instants. That is correct rather
			// than tolerated: no invariant spans two books, so there is no venue-wide
			// point in the command order for them to share (docs/MULTI-SYMBOL.md §4.4).
			for _, b := range s.books.all() {
				walPath, snapPath := s.cfg.paths(b.symbol)
				s.checkDiskSpace(b, walPath)
				if snapPath == "" {
					continue
				}
				snap, err := b.runner.Checkpoint()
				if err != nil {
					return // shutting down
				}
				// This times the WRITE and not the pause, and the omission is
				// deliberate. b.runner.Checkpoint() above serialises the book on the
				// MATCHING goroutine — that is the half that stops trading — but it is
				// a synchronous send through the command queue, so a stopwatch around
				// it from here would measure queue wait plus work. Under load the wait
				// dominates, and an operator watching this rise would conclude their
				// book had grown when in fact their queue had. Measuring the pause
				// honestly means timing it inside dispatch, which needs a hook into
				// pkg/matching this slice declines to add; what covers it meanwhile is
				// obgw_message_apply_latency_ns, where a client actually feels it.
				snapStart := time.Now()
				if err := wal.WriteSnapshot(snapPath, snap); err != nil {
					log.Printf("obgw: %s checkpoint: %v", b.symbol, err)
					if b.snapFailures != nil {
						b.snapFailures.Add(1)
					}
				} else if s.snapHist != nil {
					s.snapHist.Observe(time.Since(snapStart))
				}
				// Refresh the readiness path's cached mtime from here, where a stat is
				// already among friends. Unconditionally, including after a failure:
				// the file that is still there is the base a restart would use, and its
				// mtime is the age an operator has to be told about. If this loop dies
				// the cache stops being refreshed, and because it holds an mtime rather
				// than an age, the age it yields goes on climbing — which is precisely
				// the alert.
				s.refreshSnapshotMTime(b)
				// Retention runs whether or not this checkpoint landed, and skipping it
				// when the write failed was backwards: the write fails on a full disk, which
				// is precisely when freeing segments is the only automatic thing left that
				// can help. It is safe because Retain does not trust this write — it re-reads
				// the snapshot from disk and verifies it, so a failed checkpoint simply
				// leaves the PREVIOUS snapshot in force and retention deletes only what that
				// one already covers.
				s.retain(b, walPath, snapPath)
			}
		}
	}
}

// retain runs one retention cycle against the snapshot that was just made durable.
//
// Immediately AFTER a successful WriteSnapshot, never on the matching goroutine,
// and it re-reads the snapshot from disk rather than trusting the write it just did:
// a snapshot that exists and fails its checksum is one recovery refuses, so gating
// deletion on existence would delete the fallback for a snapshot that cannot be
// used. WriteSnapshot's temp-fsync-rename-fsync sequence is what guarantees the
// snapshot is durable before the first unlink, so there is no window in which a
// segment is gone and the snapshot covering it is not on disk.
func (s *Server) retain(b *symbolBook, walPath, snapPath string) {
	if walPath == "" || s.cfg.WALRetainBytes <= 0 {
		return
	}
	res, err := wal.Retain(walPath, snapPath, s.cfg.walOptions())
	if err != nil {
		log.Printf("obgw: %s retention: %v", b.symbol, err)
		return
	}
	if len(res.Deleted) > 0 {
		log.Printf("obgw: %s retention deleted %d segment(s) (%s..%s), archived %d; retained log is now %.1f MiB from sequence %d",
			b.symbol, len(res.Deleted), res.Deleted[0], res.Deleted[len(res.Deleted)-1],
			len(res.Archived), float64(res.RetainedBytes)/(1<<20), res.Floor)
	}
	// Why a cycle deleted less than the budget asked for, said out loud once per
	// distinct reason rather than computed and thrown away.
	//
	// This is the only place an operator can learn that the set is sitting above
	// -wal-retain on purpose. The commonest reason is the segment floor: the byte
	// budget is checked before the -wal-retain-segments floor and the floor wins, so
	// the retained set can never fall below (floor + 1) x -wal-segment-bytes — 640 MiB
	// at the defaults — whatever byte number was asked for. Watching a disk fill while
	// a configured retention deletes nothing, with nothing in the log saying why, is
	// the shape this avoids.
	//
	// Only on change, because Skipped is non-empty on most cycles in a healthy venue
	// and a line every 30 seconds is a line nobody reads. Unsynchronised because
	// retention runs only from checkpointLoop's goroutine, whether directly or through
	// checkDiskSpace.
	if res.Skipped != "" && res.Skipped != b.lastRetainSkip {
		log.Printf("obgw: %s retention kept %.1f MiB, above the %.1f MiB budget: %s",
			b.symbol, float64(res.RetainedBytes)/(1<<20), float64(s.cfg.WALRetainBytes)/(1<<20), res.Skipped)
	}
	b.lastRetainSkip = res.Skipped
}

// checkDiskSpace samples free space on the log's filesystem and acts on the two
// thresholds. Cheap, once per checkpoint tick, and not on the matching goroutine.
//
// Cancel-only at the stop-water mark reuses an existing, defined, client-visible
// state: orderbook_phase already reads 2 for it, clients already receive
// ReasonHalted for a refused new order, and the runbook already documents what it
// means. Inventing a ReasonDiskFull would be a wire change, and "why" belongs in the
// metric, the log line and the runbook rather than in a byte a trading client is
// expected to branch on.
func (s *Server) checkDiskSpace(b *symbolBook, walPath string) {
	if walPath == "" {
		return
	}
	free, ok := wal.FreeBytes(walPath)
	if !ok {
		return // the platform cannot answer; the latched sync failure is the whole guard
	}
	s.walFree.Store(free)
	// What the venue will actually do about it, so the log line and the behaviour
	// agree. Retention deletes nothing without -wal-retain, which is the default, and
	// a line that says "running retention now" against that configuration is a venue
	// telling an operator it is reclaiming space while the space keeps falling.
	remedy := "running retention now"
	if s.cfg.WALRetainBytes <= 0 {
		remedy = "-wal-retain is unset, so nothing will be reclaimed; set it, or free space by hand"
	}
	switch {
	case free < s.cfg.WALMinFreeStop:
		if !b.diskStopped {
			b.diskStopped = true
			s.walStopped.Store(true)
			log.Printf("obgw: %s DISK NEARLY FULL — %.0f MiB free on %s, below the %.0f MiB stop-water mark. "+
				"Going CANCEL-ONLY: new orders are refused, cancels and reduces are accepted so participants can "+
				"get flat. This does NOT clear when space returns; it clears on a restart. %s. "+
				"Free space or the venue will halt on the first failed sync.",
				b.symbol, float64(free)/(1<<20), walPath, float64(s.cfg.WALMinFreeStop)/(1<<20), remedy)
			b.runner.SetCancelOnly()
		}
		// And retention runs here too, every tick, not only in the low-water branch.
		// The switch is ordered by severity, so below the stop-water mark the low-water
		// case is unreachable — which meant the one automatic mechanism that can free
		// space was skipped exactly when it was the only one left.
		_, snapPath := s.cfg.paths(b.symbol)
		s.retain(b, walPath, snapPath)
	case free < s.cfg.WALMinFree:
		log.Printf("obgw: %s disk low — %.0f MiB free on %s, below the %.0f MiB low-water mark; %s",
			b.symbol, float64(free)/(1<<20), walPath, float64(s.cfg.WALMinFree)/(1<<20), remedy)
		_, snapPath := s.cfg.paths(b.symbol)
		s.retain(b, walPath, snapPath)
	}
}

func (s *Server) trackConn(c net.Conn) {
	s.connMu.Lock()
	s.conns[c] = struct{}{}
	s.connMu.Unlock()
}

func (s *Server) untrackConn(c net.Conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

// Connection lifecycle bounds. An order-entry session is long-lived and mostly
// idle, so the read timeout is generous relative to the heartbeat: a client that
// is alive will always have sent or been sent something well inside it.
const (
	idleTimeout       = 30 * time.Second
	heartbeatInterval = 5 * time.Second
	writeTimeout      = 10 * time.Second
	loginTimeout      = 10 * time.Second
)

// session is one authenticated connection.
type session struct {
	srv     *Server
	conn    net.Conn
	account string
	out     chan []byte
	closed  chan struct{}
	once    sync.Once
	// cancelOnDisconnect is read by the handler after the connection drops and
	// written by the read loop, so it is atomic rather than a plain bool.
	cancelOnDisconnect atomic.Bool
	// emitted is the highest stream sequence followStream has queued for this
	// connection. Anything that claims "everything up to Seq has reached you" has to
	// wait on this, not merely on the publisher — see waitForStream.
	emitted atomic.Uint64

	// entered remembers which instrument each client id was sent for, so a cancel,
	// reduce or replace naming only a ClOrdID can be routed to the right book.
	//
	// This exists because of the one thing that cannot be done instead: resolving
	// the engine order id up front to read its shard field. The naming index is
	// written by the MATCHING goroutine when the engine accepts an order, so a
	// cancel that arrives while its own Enter is still queued would resolve to
	// nothing and be refused for an order that is about to exist — precisely the
	// orphaned-order defect a soak found and pkg/orderentry/nameindex.go exists to
	// prevent. The session already knows the answer: it read the Enter, and the
	// Enter carried the symbol.
	//
	// Written and read only on the read loop, so no lock. Bounded, because a
	// long-lived session must not turn its own client ids into a leak; eviction is
	// harmless since the registry can still route anything older.
	enteredMu sync.Mutex
	entered   map[string]string
	enterQ    []string
}

// maxRememberedEnters bounds session.entered. It only has to cover the window
// between an Enter being read and the engine accepting it, which is microseconds;
// this is four orders of magnitude of headroom.
const maxRememberedEnters = 8192

// rememberEnter records the instrument a client id was sent for.
func (sess *session) rememberEnter(clOrdID, symbol string) {
	if clOrdID == "" {
		return
	}
	sess.enteredMu.Lock()
	defer sess.enteredMu.Unlock()
	if sess.entered == nil {
		sess.entered = map[string]string{}
	}
	if _, dup := sess.entered[clOrdID]; !dup {
		sess.enterQ = append(sess.enterQ, clOrdID)
		if len(sess.enterQ) > maxRememberedEnters {
			delete(sess.entered, sess.enterQ[0])
			sess.enterQ = sess.enterQ[1:]
		}
	}
	sess.entered[clOrdID] = symbol
}

// bookFor routes an order to its instrument's book. The order carries its symbol,
// so this cannot miss — buildOrder already refused anything the venue does not
// serve, and every conditional order is built through it.
func (sess *session) bookFor(o *types.Order) *symbolBook {
	if o != nil {
		if b := sess.srv.books.bySymbol(o.Symbol); b != nil {
			return b
		}
	}
	return sess.srv.books.first()
}

// bookForClOrdID routes a command that names an order by the client's own id.
//
// The session's own record comes first because it is the only source that knows
// about an order the engine has not accepted yet. The registry is the fallback,
// and it covers what the session cannot: an order entered on an earlier connection
// and still resting, whose engine id carries its shard.
func (sess *session) bookForClOrdID(clOrdID string) *symbolBook {
	sess.enteredMu.Lock()
	symbol, ok := sess.entered[clOrdID]
	sess.enteredMu.Unlock()
	if ok {
		if b := sess.srv.books.bySymbol(symbol); b != nil {
			return b
		}
	}
	if id, ok := sess.srv.reg.OrderIDFor(sess.account, clOrdID); ok {
		if b := sess.srv.books.byOrderID(id); b != nil {
			return b
		}
	}
	// A one-book venue always has an answer, and giving it one here keeps every
	// single-symbol path exactly as it was.
	if len(sess.srv.books.symbols()) == 1 {
		return sess.srv.books.first()
	}
	return nil
}

func (s *Server) handle(conn net.Conn) {
	s.trackConn(conn)
	defer func() {
		s.untrackConn(conn)
		_ = conn.Close()
	}()

	buf := make([]byte, wire.MaxPayload)

	// An unauthenticated peer gets the least patience: connect-and-say-nothing
	// is the cheapest possible resource-exhaustion attack.
	_ = conn.SetReadDeadline(time.Now().Add(loginTimeout))

	// Login must be the first packet. Anything else is a protocol error and the
	// connection is dropped without a reply — an unauthenticated peer gets no
	// information about the venue.
	pkt, err := wire.ReadPacket(conn, buf)
	if err != nil || pkt.Type != wire.PacketLoginRequest {
		return
	}
	req, err := wire.DecodeLoginRequest(pkt.Payload)
	if err != nil {
		return
	}

	// One call, one bool, and deliberately no branch on which half was wrong. A
	// venue that answers "no such account" faster than "wrong password" will tell
	// anyone who asks which of its participants exist.
	if !s.auth.Authenticate(req.Username, req.Password) {
		// Through rejectLogin, not straight to the socket: a refusal the page cannot
		// see is a refusal that does not exist during the incident it matters in, and
		// credential stuffing is exactly that incident.
		s.rejectLogin(conn, wire.RejectNotAuthorised)
		return
	}

	// Resume, if asked. A cursor from another incarnation, or one already evicted,
	// is refused explicitly rather than quietly served from the start — the client
	// must know it has a gap.
	var backlog []orderentry.Msg
	if req.Session != "" {
		backlog, err = s.reg.Resume(req.Session, req.Username, req.Sequence)
		if err != nil {
			code := wire.RejectBadSequence
			if errors.Is(err, orderentry.ErrNoSuchStream) {
				code = wire.RejectNoSession
			}
			s.rejectLogin(conn, code)
			return
		}
	}

	stream := s.reg.Stream(req.Username)
	acc, err := wire.EncodeLoginAccepted(nil, wire.LoginAccepted{
		Session:  s.reg.Incarnation(),
		Sequence: stream.Seq(),
	})
	if err != nil {
		return
	}
	if err := wire.WritePacket(conn, wire.PacketLoginAccepted, acc); err != nil {
		return
	}

	sess := &session{
		srv:     s,
		conn:    conn,
		account: req.Username,
		out:     make(chan []byte, s.cfg.OutboundDepth),
		closed:  make(chan struct{}),
	}
	defer sess.close()
	// The sweep runs after the read loop returns, i.e. once the connection is
	// genuinely gone, so a client that logs out cleanly still gets it — dropping the
	// socket and logging out are the same thing to a book that must not keep quoting
	// on behalf of somebody who can no longer manage it.
	defer sess.pullBookIfRequested()

	go sess.writeLoop()
	go sess.heartbeat()
	go sess.followStream(stream, req.Sequence, backlog)

	sess.readLoop(buf)
}

func (sess *session) close() {
	sess.once.Do(func() { close(sess.closed) })
}

// readLoop applies inbound commands until the peer goes away.
func (sess *session) readLoop(buf []byte) {
	for {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		default:
		}

		// An idle peer must not hold a goroutine, a buffer and a stream forever.
		// The deadline is refreshed on every packet, and the client's heartbeat
		// counts, so a live-but-quiet session stays up while a dead one is reaped.
		_ = sess.conn.SetReadDeadline(time.Now().Add(idleTimeout))

		pkt, err := wire.ReadPacket(sess.conn, buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A malformed frame or an idle timeout is terminal; drop rather
				// than guess at what the peer meant.
				return
			}
			return
		}
		switch pkt.Type {
		case wire.PacketUnsequenced:
			sess.apply(pkt.Payload)
		case wire.PacketClientHeartbt:
			// nothing to do; the read itself is the liveness signal
		case wire.PacketLogoutRequest:
			return
		}
	}
}

// apply turns one inbound message into an engine command, dispatching on the
// declared message type. Inferring the type from payload length would mean any
// future message sharing a length with an existing one is silently misread.
func (sess *session) apply(payload []byte) {
	defer sess.srv.observeApply(time.Now())

	msgType, ok := wire.MsgTypeOf(payload)
	if !ok {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	if v, _ := wire.VersionOf(payload); v != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	switch msgType {
	case wire.MsgEnter:
		sess.enter(payload)
	case wire.MsgCancel:
		sess.cancel(payload)
	case wire.MsgReduce:
		sess.reduce(payload)
	case wire.MsgReplaceOrder:
		sess.replaceOrder(payload)
	case wire.MsgEnterDated:
		sess.enterDated(payload)
	case wire.MsgEnterStop:
		sess.enterStop(payload)
	case wire.MsgEnterOCO:
		sess.enterOCO(payload)
	case wire.MsgEnterIceberg:
		sess.enterIceberg(payload)
	case wire.MsgEnterPegged:
		sess.enterPegged(payload)
	case wire.MsgEnterTrailing:
		sess.enterTrailing(payload)
	case wire.MsgMassCancel:
		sess.massCancel(payload)
	case wire.MsgCancelOnDisconnect:
		sess.setCancelOnDisconnect(payload)
	case wire.MsgQuery:
		go sess.reportOpenOrders() // reads the book and drains the pump; not on the read loop
	default:
		sess.reject("", orderentry.ReasonMalformed)
	}
}

func (sess *session) enter(payload []byte) {
	m, err := wire.DecodeEnter(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// Built through the same path as every conditional entry. This handler used to
	// carry its own copy of the side/type/TIF mapping, and the two drifted the moment
	// DAY and GTD were added: a plain Enter carrying the new TIF bytes fell through to
	// the default and rested as GTC, so an order the client believed had a deadline
	// would have lived forever. One mapping, one place.
	o, reason := sess.buildOrder(wire.BaseOrder{
		ClOrdID: m.ClOrdID, Symbol: m.Symbol, Side: m.Side, Type: m.Type,
		TIF: m.TIF, PostOnly: m.PostOnly, Price: m.Price, Quantity: m.Quantity,
	})
	if reason != 0 {
		sess.reject(m.ClOrdID, reason)
		return
	}

	if !sess.bookFor(o).gate.Allow(o, time.Now()) {
		sess.reject(m.ClOrdID, orderentry.ReasonThrottled)
		return
	}

	// Fire and forget: the outcome arrives on the event stream. The synchronous
	// API would hand back the engine-owned order, which this goroutine must not
	// read while the matcher is mutating it.
	if err := sess.bookFor(o).runner.TryEnqueue(o); err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(m.ClOrdID, reason)
	}
}

// buildOrder turns a wire base-order block into an engine order, applying the same
// rules Enter does: the account comes from the authenticated session and never from
// the wire, and an order naming a different instrument is refused rather than being
// booked here anyway.
//
// reason is non-zero when the order cannot be built, so each caller rejects with the
// client's own id rather than a bare code.
func (sess *session) buildOrder(b wire.BaseOrder) (*types.Order, uint16) {
	if sess.srv.books.bySymbol(b.Symbol) == nil {
		return nil, orderentry.ReasonMalformed
	}
	// Venue-wide ClOrdID uniqueness, enforced rather than assumed. The naming index
	// is keyed by account and client id with no symbol, so a repeat while the first
	// order is still live would overwrite it and silently retarget this account's
	// next cancel. Harmless at a one-instrument venue and a trap the moment there
	// are two — see docs/MULTI-SYMBOL.md §4.5, which spends this rule to keep
	// Cancel/Reduce/ReplaceOrder naming an order by client id alone.
	if sess.srv.reg.IsLiveClOrdID(sess.account, b.ClOrdID) {
		return nil, orderentry.ReasonDuplicateClOrd
	}
	// Recorded here, on the read loop, before the command is enqueued: a cancel
	// read a moment later must be routable even though the engine has not accepted
	// this order yet. See session.entered.
	sess.rememberEnter(b.ClOrdID, b.Symbol)
	side := types.SideBuy
	if b.Side == wire.SideSell {
		side = types.SideSell
	}
	otype := types.OrderTypeLimit
	if b.Type == wire.TypeMarket {
		otype = types.OrderTypeMarket
	}
	tif := types.TIFGoodTillCancel
	switch b.TIF {
	case wire.TIFImmediateOrCanc:
		tif = types.TIFImmediateOrCancel
	case wire.TIFFillOrKill:
		tif = types.TIFFillOrKill
	case wire.TIFDay:
		tif = types.TIFDay
	case wire.TIFGoodTillDate:
		// GTD has nowhere on a plain Enter to carry its deadline, so it is only
		// legal via EnterDated. Refused rather than quietly downgraded to GTC,
		// which would leave an order the client believes is dated resting forever.
		return nil, orderentry.ReasonMalformed
	}
	// b.Symbol, not the configured one: buildOrder validated it against the venue's
	// book set above, and stamping the gateway's default here instead is how every
	// order at a two-book venue silently ended up on the first book.
	o, err := types.NewOrder(sess.account, b.Symbol, side, otype, b.Price, b.Quantity, tif)
	if err != nil {
		return nil, orderentry.ReasonMalformed
	}
	o.ClientOrderID = b.ClOrdID
	o.PostOnly = b.PostOnly
	return o, 0
}

// submitConditional applies the admission gate and enqueues, so every conditional
// entry is rate-limited and backpressured exactly like a plain Enter. A path that
// skipped the gate would be a way around the venue's own throttle.
func (sess *session) submitConditional(clOrdID string, o *types.Order, enqueue func() error) {
	if !sess.bookFor(o).gate.Allow(o, time.Now()) {
		sess.reject(clOrdID, orderentry.ReasonThrottled)
		return
	}
	if err := enqueue(); err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(clOrdID, reason)
	}
}

// enterDated places an order carrying its own expiry (GTD).
//
// The deadline is validated by the engine, which refuses one already in the past
// rather than accepting the order and expiring it on the next command.
func (sess *session) enterDated(payload []byte) {
	m, err := wire.DecodeEnterDated(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// The TIF byte is forced rather than trusted: this message exists precisely to
	// carry a deadline, and an EnterDated claiming to be GTC would be a contradiction
	// the engine would then have to guess about.
	m.Order.TIF = wire.TIFGoodTillCancel
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	o.TimeInForce = types.TIFGoodTillDate
	o.ExpiresAt = time.Unix(0, m.ExpiresAt).UTC()

	if !sess.bookFor(o).gate.Allow(o, time.Now()) {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonThrottled)
		return
	}
	if err := sess.bookFor(o).runner.TryEnqueue(o); err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(m.Order.ClOrdID, reason)
	}
}

// enterStop places a stop or stop-limit order.
func (sess *session) enterStop(payload []byte) {
	m, err := wire.DecodeEnterStop(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	stop, err := types.NewStopOrder(o, m.StopPrice)
	if err != nil {
		// A non-positive stop price lands here. Refused rather than treated as
		// "trigger now": an order that fires on arrival is a market order.
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.bookFor(stop.Order).runner.TryEnqueueStop(stop)
	})
}

// enterOCO places a primary order paired with a stop leg.
func (sess *session) enterOCO(payload []byte) {
	m, err := wire.DecodeEnterOCO(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	primary, reason := sess.buildOrder(m.Primary)
	if reason != 0 {
		sess.reject(m.Primary.ClOrdID, reason)
		return
	}
	// The stop leg inherits symbol, side, quantity and TIF from the primary; only
	// its own identifier and prices come off the wire. Legs of differing size would
	// leave a residual position behind whichever fired.
	leg := m.Primary
	leg.ClOrdID = m.StopClOrdID
	leg.Type = wire.TypeMarket
	leg.Price = 0
	if m.StopLimitPrice != 0 {
		leg.Type = wire.TypeLimit
		leg.Price = m.StopLimitPrice
	}
	stopOrder, reason := sess.buildOrder(leg)
	if reason != 0 {
		sess.reject(m.StopClOrdID, reason)
		return
	}
	stop, err := types.NewStopOrder(stopOrder, m.StopPrice)
	if err != nil {
		sess.reject(m.StopClOrdID, orderentry.ReasonMalformed)
		return
	}
	oco, err := types.NewOCOOrder(primary, stop)
	if err != nil {
		sess.reject(m.Primary.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Primary.ClOrdID, primary, func() error {
		return sess.bookFor(oco.Primary).runner.TryEnqueueOCO(oco)
	})
}

// enterIceberg places an order that shows only a slice at a time.
func (sess *session) enterIceberg(payload []byte) {
	m, err := wire.DecodeEnterIceberg(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	ib, err := types.NewIcebergOrder(o, m.DisplayQty)
	if err != nil {
		// A display size that is zero, negative, or larger than the total.
		sess.reject(m.Order.ClOrdID, orderentry.ReasonInvalidQuantity)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.bookFor(ib.Order).runner.TryEnqueueIceberg(ib)
	})
}

// enterPegged places an order that tracks a reference price.
func (sess *session) enterPegged(payload []byte) {
	m, err := wire.DecodeEnterPegged(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// The peg computes the price, so a client-supplied one is refused rather than
	// silently overwritten — otherwise a client believes it set a price the venue
	// then replaced.
	if m.Order.Price != 0 {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	// The engine computes the price AND sets the type (ProcessPegged), so the order
	// is built as a market order here: types.NewOrder refuses a limit order priced at
	// zero, and zero is the only honest price to send for an order whose price the
	// venue derives.
	m.Order.Type = wire.TypeMarket
	var ref types.PegReference
	switch m.Ref {
	case wire.PegBid:
		ref = types.PegToBid
	case wire.PegAsk:
		ref = types.PegToAsk
	case wire.PegMid:
		ref = types.PegToMid
	default:
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	pegged, err := types.NewPeggedOrder(o, ref, m.Offset)
	if err != nil {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.bookFor(pegged.Order).runner.TryEnqueuePegged(pegged)
	})
}

// enterTrailing places a stop whose trigger follows the market.
func (sess *session) enterTrailing(payload []byte) {
	m, err := wire.DecodeEnterTrailing(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	ts, err := types.NewTrailingStop(o, m.Trail)
	if err != nil {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.bookFor(ts.Order).runner.TryEnqueueTrailing(ts)
	})
}

func (sess *session) cancel(payload []byte) {
	m, err := wire.DecodeCancel(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// The order is named when the command is applied, not now. Now is too early: the
	// Enter that creates it may still be in the queue ahead of this cancel, and a
	// client told "no such order" does not ask again — the order would rest in the
	// book, addressable by nobody, until the venue restarted. Measured at 12,843
	// orphaned orders in thirty seconds at 10,000 messages a second. See docs/SOAK.md.
	//
	// The closure runs on the matching goroutine, so the reject on the miss path is a
	// non-blocking send onto this connection's bounded queue and nothing else.
	//
	// The account is the session's, so a client can only ever cancel its own.
	// The book comes from the session's own record of where this client id was
	// sent, so it is known even while the Enter is still queued. Resolution of the
	// engine id still happens inside the closure, on the matching goroutine, for
	// the reason above.
	target := sess.bookForClOrdID(m.ClOrdID)
	if target == nil {
		sess.reject(m.ClOrdID, orderentry.ReasonUnknownOrder)
		return
	}
	err = target.runner.TryEnqueueCancelBy(sess.account, func() (int64, bool) {
		id, ok := sess.srv.reg.OrderIDFor(sess.account, m.ClOrdID)
		if !ok {
			sess.reject(m.ClOrdID, orderentry.ReasonUnknownOrder)
		}
		return id, ok
	})
	if err != nil {
		sess.reject(m.ClOrdID, enqueueRefusalReason(err))
	}
}

// reduce shrinks a resting order in place, keeping its queue position.
//
// The enqueue happens here on the read loop rather than in a goroutine, so a
// reduce stays behind the order it names: dispatching it concurrently would let
// it overtake its own Enter and be refused for an order that does not exist yet.
// Only the wait for the outcome is moved off the read loop, because the matcher
// must never be able to stall a connection's ingress.
//
// Unlike a cancel, the outcome is reported. A reduce fails for reasons the client
// caused and can correct — asking to grow, or to shrink below what is already
// filled — and a client that hears nothing cannot tell that from a reduce still
// in flight.
func (sess *session) reduce(payload []byte) {
	m, err := wire.DecodeReduce(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// A non-positive size is refused here rather than passed down: the engine
	// would reject it anyway, and there is no reason to spend a queue slot and a
	// log record on a message that cannot succeed.
	if m.Quantity <= 0 {
		sess.reject(m.ClOrdID, orderentry.ReasonInvalidQuantity)
		return
	}
	// Named at apply time, for the same reason a cancel is. See sess.cancel.
	//
	// The account is the session's, so the engine's own (orderID, userID) check
	// makes naming another client's order impossible rather than merely refused.
	target := sess.bookForClOrdID(m.ClOrdID)
	if target == nil {
		sess.reject(m.ClOrdID, orderentry.ReasonUnknownOrder)
		return
	}
	done, err := target.runner.TryReduceAsyncBy(m.Quantity, sess.account, func() (int64, bool) {
		return sess.srv.reg.OrderIDFor(sess.account, m.ClOrdID)
	})
	if err != nil {
		sess.reject(m.ClOrdID, enqueueRefusalReason(err))
		return
	}
	go func() {
		select {
		case rerr := <-done:
			if rerr != nil {
				sess.reject(m.ClOrdID, orderentry.ReasonFor(rerr))
			}
			// Success needs nothing here: the engine emits Replaced, which reaches
			// this client over its own stream like every other outcome.
		case <-sess.closed:
		case <-sess.srv.quit:
		}
	}()
}

// waitForStream blocks until this connection has queued every stream message up to
// target, so a message asserting "everything through Seq has reached you" is true
// when it is written rather than merely true of the publisher.
//
// Draining the publisher is not enough on its own. That only moves events into the
// account's stream; the connection receives them from a separate polling goroutine
// (followStream), so a reply written directly to the outbound queue can overtake
// stream messages it claims to come after. A client applying them in arrival order
// would then apply the same execution twice — the exact failure the drain was
// introduced to prevent.
//
// Bounded: a client that is not being served within the timeout gets the message
// anyway rather than never getting one, since a late boundary is recoverable and a
// missing terminator is not.
func (sess *session) waitForStream(target uint64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for sess.emitted.Load() < target {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		default:
		}
		if time.Now().After(deadline) {
			log.Printf("obgw: %s stream lagged past %v waiting for seq %d", sess.account, timeout, target)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// replaceOrder cancels one order and enters another atomically.
//
// Enqueued on the read loop so the replace cannot overtake the order it names; only
// the outcome is awaited elsewhere. There is no bespoke acknowledgement — a
// successful replace is the Canceled for the old ClOrdID followed by the Accepted for
// the new one, which describes it exactly, and a failure is a CmdReject naming the
// original.
func (sess *session) replaceOrder(payload []byte) {
	m, err := wire.DecodeReplaceOrder(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	replacement, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	// The replacement is new liquidity, so it passes the admission gate exactly as a
	// plain Enter would. Replace must not be a way around the venue's throttle.
	if !sess.bookFor(replacement).gate.Allow(replacement, time.Now()) {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonThrottled)
		return
	}
	// Named at apply time, for the same reason a cancel is. See sess.cancel.
	done, err := sess.bookFor(replacement).runner.TryReplaceAsyncBy(sess.account, replacement, func() (int64, bool) {
		return sess.srv.reg.OrderIDFor(sess.account, m.OrigClOrdID)
	})
	if err != nil {
		sess.reject(m.Order.ClOrdID, enqueueRefusalReason(err))
		return
	}
	go func() {
		select {
		case rerr := <-done:
			if rerr != nil {
				// The cancel half failed, so nothing was replaced and nothing new was
				// entered. Named by the ORIGINAL id: that is the order the client was
				// wrong about.
				sess.reject(m.OrigClOrdID, orderentry.ReasonFor(rerr))
			}
		case <-sess.closed:
		case <-sess.srv.quit:
		}
	}()
}

// massCancel pulls everything the account has resting.
//
// The enqueue is on the read loop so the sweep cannot overtake an order entered
// immediately before it — the ordering argument that applies to reduce applies with
// more force here, since a sweep that ran early would leave behind the very order
// the client was trying to get rid of.
//
// The acknowledgement is written only after the publisher has been drained, so every
// Canceled the sweep produced has already reached the client. Without that, an ack
// saying "12 orders cancelled" could arrive before any of the twelve Canceled
// messages, and a client applying them in order would briefly believe it had a book
// the venue had already emptied.
func (sess *session) massCancel(payload []byte) {
	if _, err := wire.DecodeMassCancel(payload); err != nil {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// A mass cancel means "pull everything I have", and an account can be resting
	// on every instrument the venue serves, so this fans across all of them. The
	// ack's count is the total: a client that got one number per book would have to
	// know how many books there are, which is not something the wire tells it.
	all := sess.srv.books.all()
	dones := make([]<-chan int, 0, len(all))
	for _, b := range all {
		done, err := b.runner.TryCancelAllAsync(sess.account)
		if err != nil {
			sess.reject("", enqueueRefusalReason(err))
			return
		}
		dones = append(dones, done)
	}
	go func() {
		var n int
		for _, done := range dones {
			select {
			case got := <-done:
				n += got
			case <-sess.closed:
				return
			case <-sess.srv.quit:
				return
			}
		}
		sess.srv.pub.Wait()
		seq := sess.srv.reg.Stream(sess.account).Seq()
		sess.waitForStream(seq, 5*time.Second)
		b, encErr := wire.EncodeMassCancelAck(nil, wire.MassCancelAck{
			Version: wire.Version, Count: uint32(n), Seq: seq,
		})
		if encErr != nil {
			log.Printf("obgw: encode mass cancel ack: %v", encErr)
			return
		}
		sess.send(b)
	}()
}

// setCancelOnDisconnect turns the account's book into something that does not
// outlive this connection.
//
// Acknowledged explicitly because a client must never be guessing about a control
// that decides whether its orders survive: silence would leave "enabled" and
// "message ignored" indistinguishable.
func (sess *session) setCancelOnDisconnect(payload []byte) {
	m, err := wire.DecodeCancelOnDisconnect(payload)
	if err != nil {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	sess.cancelOnDisconnect.Store(m.Enabled)
	b, encErr := wire.EncodeCODAck(nil, wire.CODAck{Version: wire.Version, Enabled: m.Enabled})
	if encErr != nil {
		return
	}
	sess.send(b)
}

// pullBookIfRequested runs the cancel-on-disconnect sweep. It is called once the
// connection is gone, so there is nobody to acknowledge to and nothing to wait for;
// the point is that the book does not keep quoting on behalf of a client that can no
// longer manage it.
//
// The sweep is account-wide because orders are not tagged with the session that
// entered them. An account holding a second connection loses those orders too, which
// is documented on the wire type rather than left to be discovered.
func (sess *session) pullBookIfRequested() {
	if !sess.cancelOnDisconnect.Load() {
		return
	}
	// Not when the VENUE is the one going away. A graceful shutdown drops every
	// connection at once, and firing the sweep here would journal a cancel for every
	// order held by every cancel-on-disconnect session — permanently destroying books
	// that are supposed to come back after the restart. The control means "if I lose
	// my session", not "if the venue closes for the day".
	select {
	case <-sess.srv.quit:
		return
	default:
	}
	for _, b := range sess.srv.books.all() {
		if _, err := b.runner.TryCancelAllAsync(sess.account); err != nil {
			log.Printf("obgw: cancel-on-disconnect for %s on %s: %v", sess.account, b.symbol, err)
			// A SHED only, and the discrimination matters because the runbook entry
			// for this counter says any increase means orders were left resting and
			// must be reconciled by hand.
			//
			// ErrShuttingDown does leave the orders resting, and it is not that: the
			// venue is closing for the day and those orders are supposed to come back
			// after the restart. The quit check above closes most of that window, but
			// a session whose read loop returned microseconds before quit closed still
			// reaches here, and sending the on-call to reconcile a book on a clean
			// deploy is how a page-on-any-increase counter stops being read.
			//
			// Everything else is counted. The venue undertook to pull this account's
			// resting orders, the queue would not take the command, and the orders
			// STAY in the book: owned by a session that no longer exists, still able
			// to trade, cancellable only by an operator or a restart. Same class of
			// harm as a dropped publisher batch — not a delay, a loss — which is why
			// it gets its own counter rather than being folded in beside a throttle.
			if !errors.Is(err, matching.ErrShuttingDown) {
				sess.srv.countShedUnreported()
			}
		}
	}
}

// reportOpenOrders answers a Query with the venue's authoritative view of the
// account's live orders.
//
// The ordering here is the whole point. The book is read on the matching
// goroutine, then the publisher is drained, and only then is the report written.
// That sequence guarantees every event up to the instant of the read has already
// reached the client, so the report and the stream cannot contradict each other:
// everything after the reported Seq is a change to apply on top of it.
//
// Reading the book without draining first would let an execution that happened
// before the read arrive after the report, and the client would apply it twice.
func (sess *session) reportOpenOrders() {
	// Every book, in the venue's stable order. An account's open orders are its
	// open orders; splitting the answer per instrument would make a client
	// reconcile N replies against a count the wire gives it once.
	var orders []*types.Order
	for _, b := range sess.srv.books.all() {
		got, err := b.runner.OpenOrdersFor(sess.account)
		if err != nil {
			sess.reject("", orderentry.ReasonShuttingDown)
			return
		}
		orders = append(orders, got...)
	}
	sess.srv.pub.Wait()

	stream := sess.srv.reg.Stream(sess.account)
	// Draining the publisher put those events in the stream; this waits until they
	// have actually been queued for THIS connection. Without it the report could
	// overtake them and QueryEnd.Seq would assert a boundary the client had not
	// reached.
	reportSeq := stream.Seq()
	sess.waitForStream(reportSeq, 5*time.Second)
	for _, o := range orders {
		side := wire.SideBuy
		if o.Side == types.SideSell {
			side = wire.SideSell
		}
		b, encErr := wire.EncodeOpenOrder(nil, wire.OpenOrder{
			Version: wire.Version, ClOrdID: o.ClientOrderID,
			Price: o.Price, LeavesQty: o.RemainingQty, Side: side,
		})
		if encErr != nil {
			log.Printf("obgw: encode open order: %v", encErr)
			continue
		}
		sess.send(b)
	}

	// The terminator carries the count and the stream position. Without it a
	// client cannot tell "you have nothing open" from "the report was cut short",
	// which are opposite conclusions.
	b, encErr := wire.EncodeQueryEnd(nil, wire.QueryEnd{
		Version: wire.Version, Count: uint32(len(orders)), Seq: reportSeq,
	})
	if encErr != nil {
		return
	}
	sess.send(b)
}

// enqueueRefusalReason maps an enqueue failure onto the wire reason to send.
//
// One function rather than the same three-line switch at four call sites, because the
// three-line switch was only correct at one of them. Every site used to start from
// ReasonOverloaded and special-case a drain, so a cancel refused during a planned
// restart was reported to the client as OVERLOAD — harmless while nothing counted it,
// and the moment obgw_refused_total exists every clean deploy adds to the one series
// in this family that PAGES ON ANY INCREASE.
//
// ErrNoResolver is neither condition. It means a caller passed a nil id resolver,
// which is a bug in this process and not a statement about load, so it maps to
// "other" and never to overloaded. It cannot happen today — every closure is
// non-nil — and that is exactly why it has to be handled here rather than at
// whichever site somebody remembers.
func enqueueRefusalReason(err error) uint16 {
	switch {
	case errors.Is(err, matching.ErrShuttingDown):
		return orderentry.ReasonShuttingDown
	case errors.Is(err, matching.ErrNoResolver):
		return orderentry.ReasonOther
	default:
		return orderentry.ReasonOverloaded
	}
}

// reject reports that the command itself was refused, as distinct from an order
// the engine looked at and declined.
func (sess *session) reject(clOrdID string, reason uint16) {
	// Counted HERE, at the one funnel every client-visible refusal already passes
	// through, and counted BEFORE the encode.
	//
	// Here, because it makes the count complete by construction: a new ingress path
	// cannot forget to count, since it cannot refuse without calling this. Counting
	// at each `if err != nil` site instead is a list that goes stale the first time
	// somebody adds one — the same failure docs/JOURNAL-COMPLETENESS.md §4.2 spends a
	// section on for the command log.
	//
	// Before the encode, because a refusal the venue could not deliver — a failed
	// encode below, or a send that drops the connection because the client stopped
	// reading — is still a refusal, and the client is WORSE off for not hearing it,
	// not better. The metric counts the decision, not the successful delivery.
	//
	// Not the same population as orderbook_rejections_total, which counts what the
	// BOOK refused. They overlap where the gateway relays an engine error onto the
	// wire, neither is a total, and adding them is meaningless.
	sess.srv.countRefusal(reason)
	b, err := wire.EncodeCmdReject(nil, wire.CmdReject{Version: wire.Version, ClOrdID: clOrdID, Reason: reason})
	if err != nil {
		return
	}
	sess.send(b)
}

// send queues an outbound payload, dropping the connection if the client has
// stopped reading. A slow consumer must never be allowed to back up into the
// venue.
func (sess *session) send(payload []byte) {
	select {
	case sess.out <- payload:
	default:
		sess.close()
	}
}

// heartbeat sends a server heartbeat on an idle outbound path, so a client can
// distinguish "the venue has nothing for me" from "the venue is gone". Declaring
// the packet type and never sending it would leave clients unable to tell.
func (sess *session) heartbeat() {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		case <-t.C:
			select {
			case sess.out <- nil: // nil payload => heartbeat
			default:
				// Send queue full: the client is not reading, and the outbound
				// path will disconnect it. Do not add to the backlog.
			}
		}
	}
}

func (sess *session) writeLoop() {
	for {
		select {
		case <-sess.closed:
			_ = sess.conn.Close()
			return
		case b := <-sess.out:
			typ := wire.PacketSequencedData
			if b == nil {
				typ = wire.PacketServerHeartbt
			}
			_ = sess.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := wire.WritePacket(sess.conn, typ, b); err != nil {
				sess.close()
				return
			}
		}
	}
}

// followStream replays anything the client missed, then tails its stream.
//
// Polling rather than subscribing keeps Stream free of per-listener state; the
// interval bounds latency, and a real deployment would swap this for a condition
// variable. It is called out here rather than left for a reader to discover.
func (sess *session) followStream(stream *orderentry.Stream, from uint64, backlog []orderentry.Msg) {
	cursor := from
	sess.emitted.Store(from)
	for _, m := range backlog {
		sess.emit(m)
		cursor = m.Seq
		sess.emitted.Store(m.Seq)
	}
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		case <-tick.C:
			msgs, err := stream.Since(cursor)
			if err != nil {
				sess.close()
				return
			}
			for _, m := range msgs {
				sess.emit(m)
				cursor = m.Seq
				sess.emitted.Store(m.Seq)
			}
		}
	}
}

// emit encodes one outbound message and queues it.
func (sess *session) emit(m orderentry.Msg) {
	var (
		b   []byte
		err error
	)
	switch m.Kind {
	case orderentry.KindAccepted:
		side := wire.SideBuy
		if m.Side == types.SideSell {
			side = wire.SideSell
		}
		b, err = wire.EncodeAccepted(nil, wire.Accepted{
			Version: wire.Version, ClOrdID: m.ClOrdID,
			Price: m.Price, Quantity: m.Quantity, Side: side,
		})
	case orderentry.KindRejected:
		b, err = wire.EncodeRejected(nil, wire.Rejected{Version: wire.Version, ClOrdID: m.ClOrdID, Reason: m.Reason})
	case orderentry.KindExecuted:
		agg := wire.SideBuy
		if m.Aggressor == types.SideSell {
			agg = wire.SideSell
		}
		b, err = wire.EncodeExecuted(nil, wire.Executed{
			Version: wire.Version, ClOrdID: m.ClOrdID,
			Price: m.Price, Quantity: m.Quantity, LeavesQty: m.LeavesQty, Aggressor: agg,
			TradeID: m.TradeID,
		})
	case orderentry.KindBusted:
		b, err = wire.EncodeBusted(nil, wire.Busted{
			Version: wire.Version, ClOrdID: m.ClOrdID, TradeID: m.TradeID,
		})
	case orderentry.KindCanceled:
		b, err = wire.EncodeCanceled(nil, wire.Canceled{Version: wire.Version, ClOrdID: m.ClOrdID, Reason: m.Reason})
	case orderentry.KindReplaced:
		b, err = wire.EncodeReplaced(nil, wire.Replaced{Version: wire.Version, ClOrdID: m.ClOrdID, LeavesQty: m.LeavesQty})
	default:
		return
	}
	if err != nil {
		log.Printf("obgw: encode %d: %v", m.Kind, err)
		return
	}
	sess.send(b)
}
