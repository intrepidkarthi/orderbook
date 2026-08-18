package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

// The signals docs/LAG-AND-SHED.md adds, and the one sentence they exist for:
// every counter this venue had was a count of an outcome it PRODUCED, taken off the
// engine's event stream. Nothing counted work the venue refused before that stream
// existed, and nothing timed anything the venue waits on.
//
// So: what was refused (obgw_refused_total, obgw_login_refused_total,
// obgw_shed_unreported_total, obgw_snapshot_failures_total) and what is waited on
// (the WAL append and sync histograms, snapshot age and duration, recovery duration).
//
// Naming, which the exposition already split without writing it down: orderbook_ is
// a fact about the VENUE that anything with the path could compute — snapshot age is
// read off a file's mtime — and obgw_ is a fact only THIS PROCESS knows, because
// this process is what timed or counted it.
const (
	refusedMetric          = "obgw_refused_total"
	loginRefusedMetric     = "obgw_login_refused_total"
	shedUnreportedMetric   = "obgw_shed_unreported_total"
	walAppendLatencyMetric = "obgw_wal_append_latency_ns"
	walSyncLatencyMetric   = "obgw_wal_sync_latency_ns"
	snapshotAgeMetric      = "orderbook_snapshot_age_seconds"
	snapshotDurationMetric = "obgw_snapshot_duration_ns"
	snapshotFailuresMetric = "obgw_snapshot_failures_total"
	recoveryDurationMetric = "obgw_recovery_duration_ns"
	// obgw_ rather than orderbook_: it is a fact only this process knows, because
	// this process is what read the log and decided those records were insufficient.
	icebergReserveUnknownMetric = "obgw_recovery_iceberg_reserve_unknown_total"
)

// shedCancelOnDisconnect is the one label value obgw_shed_unreported_total has
// today. The label exists so the second such site lands as a series rather than as
// a new metric name.
const shedCancelOnDisconnect = "cancel_on_disconnect"

// reasonMetricNames maps the frozen wire reason vocabulary onto label values,
// indexed by the code itself so a refusal costs an array index rather than a hash.
//
// The vocabulary is the wire's (pkg/orderentry/reason.go), lowercased and snake
// cased, because the operator's question during an incident is "how many clients did
// we refuse and for what" and the answer has to be in the same words the clients are
// complaining in. A metric label an operator greps for is part of the interface —
// the same argument TestDrillTheCeilingRejectionNamesItself already makes for the
// engine's reason strings — so TestEveryReasonCodeHasAMetricName freezes it.
//
// ReasonNone has no name on purpose: it is the absence of a refusal, and reject is
// never called with it. A zero index is therefore a nil handle and counts nothing.
var reasonMetricNames = [...]string{
	orderentry.ReasonNone:            "",
	orderentry.ReasonOther:           "other",
	orderentry.ReasonUnknownOrder:    "unknown_order",
	orderentry.ReasonDuplicateClOrd:  "duplicate_clord",
	orderentry.ReasonTooSmall:        "too_small",
	orderentry.ReasonTooLarge:        "too_large",
	orderentry.ReasonPriceBand:       "price_band",
	orderentry.ReasonSelfTrade:       "self_trade",
	orderentry.ReasonPostOnlyCross:   "post_only_cross",
	orderentry.ReasonFOKCannotFill:   "fok_cannot_fill",
	orderentry.ReasonHalted:          "halted",
	orderentry.ReasonThrottled:       "throttled",
	orderentry.ReasonOverloaded:      "overloaded",
	orderentry.ReasonNotAuthorised:   "not_authorised",
	orderentry.ReasonMalformed:       "malformed",
	orderentry.ReasonShuttingDown:    "shutting_down",
	orderentry.ReasonInvalidQuantity: "invalid_quantity",
	orderentry.ReasonTooSoon:         "too_soon",
}

// refusedSeriesCount is the size of the resolved handle table: one slot per wire
// reason code, including the unused zero.
const refusedSeriesCount = len(reasonMetricNames)

const refusedHelp = "CmdReject MESSAGES the gateway decided to send on an ESTABLISHED session, by wire reason. " +
	"Refusals taken before a session exists speak a different vocabulary and are counted by obgw_login_refused_total. " +
	"Not the same population as orderbook_rejections_total, which counts what the BOOK refused in engine words; " +
	"never add the two. Counts messages rather than orders — equal today, because one client message yields at most one reject."

// loginRefusalNames maps the soup login-reject bytes onto label values.
//
// A separate metric from obgw_refused_total rather than three more series on it, and
// the reason is the one thing that makes a metric readable: the two families answer
// with different vocabularies. obgw_refused_total's label is a wire REASON CODE from
// pkg/orderentry, sent in a CmdReject to a client that has logged in. These are soup
// reject BYTES sent in a LoginRejected to a peer that has not. Mapping 'A' onto
// orderentry.ReasonNotAuthorised would put two populations under one label and make
// the count of "how many command refusals did we send" wrong by however many people
// mistyped a password.
//
// It exists because the alternative was worse than an absent metric. Before it, a
// failed login moved NOTHING — obgw_refused_total{reason="not_authorised"} sat at
// zero, permanently, while the venue refused a credential-stuffing run at whatever
// rate the attacker could open sockets. A zero on a page reads as evidence, and that
// one was evidence of the opposite of the truth. docs/LAG-AND-SHED.md §4.6.
var loginRefusalNames = map[byte]string{
	wire.RejectNotAuthorised: "not_authorised",
	wire.RejectNoSession:     "no_session",
	wire.RejectBadSequence:   "bad_sequence",
}

const loginRefusedHelp = "LoginRejected messages the gateway sent, by soup reject code, for peers that never got a session. " +
	"Disjoint from obgw_refused_total, which counts CmdRejects on established sessions; never add the two. " +
	"A pre-login peer that sends something other than a LoginRequest is dropped without a reply and is NOT counted here — " +
	"it gets no information about the venue, which includes not getting a series."

// registerRefusalCounters resolves one handle per reason at startup, so a refusal
// costs one atomic add on a pointer the gateway already holds.
//
// Every series is registered whether or not it ever moves. A reason that has never
// fired is exactly the reason an operator wants already graphed on the day it does,
// and the label set is bounded and frozen — seventeen values, no per-account
// dimension (docs/LAG-AND-SHED.md §10).
func (s *Server) registerRefusalCounters() {
	for code, name := range reasonMetricNames {
		if name == "" {
			continue
		}
		s.refused[code] = s.metrics.Counter(refusedMetric, refusedHelp,
			observability.Label{Name: "reason", Value: name})
	}
	s.loginRefused = make(map[byte]*observability.Counter, len(loginRefusalNames))
	for code, name := range loginRefusalNames {
		s.loginRefused[code] = s.metrics.Counter(loginRefusedMetric, loginRefusedHelp,
			observability.Label{Name: "reason", Value: name})
	}
	s.shedUnreported = s.metrics.Counter(shedUnreportedMetric,
		"Work the venue dropped and told NOBODY about, because there was nobody left to tell. "+
			"Not a delay, a loss: orders stay resting for a client that asked for them to be pulled. Alert on any increase.",
		observability.Label{Name: "op", Value: shedCancelOnDisconnect})
}

// countRefusal records one reject message the venue decided to send.
//
// Called from session.reject, before the encode, because a refusal the venue could
// not deliver is still a refusal and the client is worse off rather than better.
//
// Nil-safe on both counts: a Server assembled field-by-field — which the drills do,
// to present a wedged matcher without wedging a real venue — has no handles, and a
// metric must not be the reason a test cannot build one.
func (s *Server) countRefusal(reason uint16) {
	if s == nil || int(reason) >= len(s.refused) {
		return
	}
	if c := s.refused[reason]; c != nil {
		c.Add(1)
	}
}

// rejectLogin sends a LoginRejected and counts it, in that order and in one place,
// so the login path cannot refuse a peer without the page knowing.
//
// Same argument as session.reject: a funnel makes the count complete by construction,
// where an increment beside each write is a list that goes stale the first time
// somebody adds a reject code. The map lookup is affordable here in a way it is not
// on the command path — this runs once per connection attempt, on the handshake
// goroutine, against a socket write.
func (s *Server) rejectLogin(conn io.Writer, code byte) {
	if s != nil {
		if c := s.loginRefused[code]; c != nil {
			c.Add(1)
		}
	}
	_ = wire.WritePacket(conn, wire.PacketLoginRejected, []byte{code})
}

// countShedUnreported records a drop no client will ever hear about.
func (s *Server) countShedUnreported() {
	if s == nil || s.shedUnreported == nil {
		return
	}
	s.shedUnreported.Add(1)
}

// snapshotAgeSeconds reports how old this book's recovery base is, read from the
// FILE rather than from a process-local timer.
//
// Three reasons, and the second is the one a timer cannot be argued out of:
//
//   - It cannot claim a snapshot is fresh when the artefact on disk is old. A timer
//     records what the code believed; the mtime records what happened.
//   - It survives a restart. A venue that has just recovered reports the true age of
//     the base it recovered from, immediately, before its first checkpoint tick. A
//     timer seeded at process start reports zero — the freshest possible reading —
//     for a venue that may be recovering from a base two days old.
//   - matching.EngineSnapshot carries no timestamp and this does not add one: the
//     snapshot is inside the semantics digest's blast radius.
//
// NaN when the venue was never asked to checkpoint, for the reason the best-bid
// gauge is NaN: zero is a legal age, and a monitoring system cannot tell a missing
// snapshot from one written this instant. NaN also never satisfies a > comparison,
// so a venue with checkpointing deliberately off does not page — while a venue that
// WAS asked and never has counts from process start, and does.
//
// Negative when time is wrong somewhere, and deliberately not clamped: clamping would
// report the freshest possible snapshot at exactly the moment the clock is wrong, and a
// negative age here is the only clock signal this venue has (docs/LAG-AND-SHED.md §6.1).
// It is not offered as a substitute for one — this host's clock and the filesystem's
// clock are not the same clock, and on a network filesystem it is the server that
// stamps the mtime. docs/RUNBOOKS.md "A negative snapshot age" is the procedure for
// telling those two apart, because they need opposite responses.
func (s *Server) snapshotAgeSeconds(b *symbolBook) float64 {
	_, snapPath := s.cfg.paths(b.symbol)
	if snapPath == "" {
		return math.NaN()
	}
	info, err := os.Stat(snapPath)
	if err != nil {
		b.snapMTime.Store(0)
		// Configured and absent — no checkpoint has landed yet. Count from process
		// start, which is the honest age of "nothing", and which crosses the same
		// threshold at the same moment a stalled checkpoint loop would.
		return time.Since(s.startedAt).Seconds()
	}
	// The scrape refreshes what the readiness probe reads, so a venue being scraped
	// keeps the cache warm for free. Nothing depends on it: the checkpoint loop
	// refreshes it too, and the cache holds an mtime rather than an age.
	b.snapMTime.Store(info.ModTime().UnixNano())
	return time.Since(info.ModTime()).Seconds()
}

// refreshSnapshotMTime updates one book's cached snapshot mtime from the filesystem.
//
// Called from startup and from the checkpoint loop — both places that are already
// doing I/O and neither of which is a probe — so /readyz never has to.
func (s *Server) refreshSnapshotMTime(b *symbolBook) {
	if b == nil {
		return
	}
	_, snapPath := s.cfg.paths(b.symbol)
	if snapPath == "" {
		b.snapMTime.Store(0)
		return
	}
	info, err := os.Stat(snapPath)
	if err != nil {
		b.snapMTime.Store(0)
		return
	}
	b.snapMTime.Store(info.ModTime().UnixNano())
}

// cachedSnapshotAgeSeconds is snapshotAgeSeconds without the stat: the readiness
// path's reading, taken from whatever the last checkpoint tick or scrape saw.
//
// It agrees with the statting version on everything that decides an alert. NaN when
// unconfigured; process age when configured and never seen, which is what a venue
// that has just started reports; and time since the cached mtime otherwise — so a
// venue restarted onto a base backdated two hours reports 7200 seconds on its FIRST
// probe, because NewServer seeds the cache before Serve is called. A process-local
// timer would report zero there, which is the freshest possible reading at the moment
// the venue is at its most exposed.
func (s *Server) cachedSnapshotAgeSeconds(b *symbolBook) float64 {
	if _, snapPath := s.cfg.paths(b.symbol); snapPath == "" {
		return math.NaN()
	}
	mtime := b.snapMTime.Load()
	if mtime == 0 {
		return time.Since(s.startedAt).Seconds()
	}
	return time.Since(time.Unix(0, mtime)).Seconds()
}

// staleSnapshotFactor is how many checkpoint intervals a snapshot may fall behind
// before the venue calls itself degraded.
//
// Three, not one: a single missed tick is a write that overran its interval, which
// is a slow disk and not yet an incident. Three consecutive misses is not noise. The
// same multiple is the alert threshold in docs/RUNBOOKS.md, so the page and the
// probe agree about when the venue is in trouble rather than disagreeing by a
// constant nobody wrote down.
const staleSnapshotFactor = 3

// checkpointDegradation names the books whose recovery base has gone stale, or ""
// when none has.
//
// It is a CLAUSE on a 200, never a 503. A failed checkpoint costs recovery time
// LATER; a failed WAL sync means commands acknowledged NOW are not durable. Only one
// of those is a reason to stop trading and it is not this one — failing readiness
// here would take a book out of rotation while it still holds every position already
// in it, and push the venue toward exactly the restart whose cost the failed
// checkpoint has been inflating. See docs/LAG-AND-SHED.md §7.
func (s *Server) checkpointDegradation() string {
	// No interval, or no log to checkpoint against: checkpointLoop is gated on
	// s.durable() (server.go, Serve), so a venue started with -snapshot and no -wal
	// never checkpoints at all. Reporting it degraded forever, on a venue where
	// nothing is wrong and nothing could be, is how a degraded clause stops being
	// read. The same gate has to be here as on the loop, or the two disagree.
	if s.cfg.CheckpointEvery <= 0 || !s.durable() {
		return ""
	}
	limit := staleSnapshotFactor * s.cfg.CheckpointEvery.Seconds()
	var parts []string
	for _, b := range s.books.all() {
		// The CACHED age, so this readiness probe does no filesystem I/O. See
		// symbolBook.snapMTime.
		age := s.cachedSnapshotAgeSeconds(b)
		if math.IsNaN(age) || age <= limit {
			continue
		}
		var failures int64
		if b.snapFailures != nil {
			failures = b.snapFailures.Value()
		}
		parts = append(parts, fmt.Sprintf("%s checkpoint %.0fs old, %d failures", b.symbol, age, failures))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}
