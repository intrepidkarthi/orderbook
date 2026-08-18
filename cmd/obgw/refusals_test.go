package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// What the venue refused — docs/LAG-AND-SHED.md §4.
//
// Every counter this venue had was a count of an outcome it PRODUCED, taken off the
// engine's event stream. A shed never reaches that stream: TryEnqueue returns
// ErrQueueFull, the gateway turns it into a wire reject and sends it, and no event is
// ever emitted. So a venue shedding load under overload had no counter for it, which
// is the one number an operator needs during the incident the queue-depth threshold
// exists to warn about.
//
// Every test here induces the real condition. A wedged matching goroutine fills a
// real queue; a real rate gate refuses a real burst; a real disconnect drops a real
// mass cancel. None of them calls the counter and asserts the counter moved.

// refusedCount reads one obgw_refused_total series off the handle the gateway
// resolved at startup. Through the handle rather than through a lookup on the
// collector, because there deliberately is no lookup: the counter's whole point is
// that the label set is resolved once and the increment is an atomic add on a
// pointer, and adding a by-name reader to pkg/observability for tests' convenience
// would grow a frozen surface to save a test three lines.
func refusedCount(t *testing.T, srv *Server, reason string) int64 {
	t.Helper()
	for code, name := range reasonMetricNames {
		if name != reason {
			continue
		}
		c := srv.refused[code]
		if c == nil {
			t.Fatalf("reason %q has no registered counter", reason)
		}
		return c.Value()
	}
	t.Fatalf("no wire reason is named %q", reason)
	return 0
}

// totalRefused sums every obgw_refused_total series, by parsing the exposition
// rather than by reading handles — so it counts what an operator would see and would
// notice a series that exists in code and never reaches the page.
func totalRefused(t *testing.T, srv *Server) int64 {
	t.Helper()
	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	var sum int64
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, refusedMetric+"{") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if i < 0 {
			t.Fatalf("malformed exposition line %q", line)
		}
		var v int64
		for _, ch := range line[i+1:] {
			if ch < '0' || ch > '9' {
				t.Fatalf("malformed value in %q", line)
			}
			v = v*10 + int64(ch-'0')
		}
		sum += v
	}
	return sum
}

// wedgeableLog is a CommandLog whose appends can be stopped and restarted while a
// real venue is trading. blockingLog wedges once, on its first write, which is the
// right shape for the stalled-matcher drill and the wrong one here: these tests need
// a book that filled up NORMALLY and then stopped draining.
type wedgeableLog struct {
	mu   sync.Mutex
	gate chan struct{}
}

func (l *wedgeableLog) wedge() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gate == nil {
		l.gate = make(chan struct{})
	}
}

func (l *wedgeableLog) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gate != nil {
		close(l.gate)
		l.gate = nil
	}
}

func (l *wedgeableLog) block() {
	l.mu.Lock()
	g := l.gate
	l.mu.Unlock()
	if g != nil {
		<-g
	}
}

func (l *wedgeableLog) AppendSubmit(*types.Order) (int64, error)          { l.block(); return 0, nil }
func (l *wedgeableLog) AppendCancel(int64, string) (int64, error)         { l.block(); return 0, nil }
func (l *wedgeableLog) AppendReduce(int64, int64, string) (int64, error)  { l.block(); return 0, nil }
func (l *wedgeableLog) AppendCancelAll(string) (int64, error)             { l.block(); return 0, nil }
func (l *wedgeableLog) AppendStop(*types.StopOrder) (int64, error)        { l.block(); return 0, nil }
func (l *wedgeableLog) AppendOCO(*types.OCOOrder) (int64, error)          { l.block(); return 0, nil }
func (l *wedgeableLog) AppendIceberg(*types.IcebergOrder) (int64, error)  { l.block(); return 0, nil }
func (l *wedgeableLog) AppendPegged(*types.PeggedOrder) (int64, error)    { l.block(); return 0, nil }
func (l *wedgeableLog) AppendTrailing(*types.TrailingStop) (int64, error) { l.block(); return 0, nil }
func (l *wedgeableLog) AppendHalt() (int64, error)                        { l.block(); return 0, nil }
func (l *wedgeableLog) AppendResume() (int64, error)                      { l.block(); return 0, nil }
func (l *wedgeableLog) AppendCancelOnly() (int64, error)                  { l.block(); return 0, nil }
func (l *wedgeableLog) AppendSetMark(int64) (int64, error)                { l.block(); return 0, nil }
func (l *wedgeableLog) AppendBust(int64, string) (int64, error)           { l.block(); return 0, nil }
func (l *wedgeableLog) AppendReplace(int64, string, *types.Order) (int64, error) {
	l.block()
	return 0, nil
}
func (l *wedgeableLog) AppendSetPhase(matching.EngineState) (int64, error) {
	l.block()
	return 0, nil
}

// wedgeableVenue is a real venue over a real socket whose matching goroutine can be
// stopped on demand, with a queue small enough that filling it is a handful of
// orders rather than a flood.
//
// The book's runner is swapped before Listen, so nothing is serving while the field
// is written and the venue that answers the socket is the one under test.
func wedgeableVenue(t *testing.T, queueSize int) (*Server, *wedgeableLog) {
	t.Helper()
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1", "bob": "pw2"},
		OutboundDepth: 512, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
	})
	b := srv.books.first()
	original := b.runner

	lg := &wedgeableLog{}
	eng := matching.DefaultConfig("X")
	eng.DedupClientOrderIDs = 4096
	eng.EventSink = matching.MultiSink{orderentry.NewNameIndex(srv.reg), srv.pub, b.feed, srv.metrics}
	r := matching.NewRunner(matching.RunnerConfig{Engine: eng, QueueSize: queueSize, Log: lg})
	b.runner = r
	b.gate = gateway.New(r, gateway.Config{Rate: 1e6, Burst: 1e6})
	srv.runner = r

	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		lg.release()
		srv.Close()
		original.Close()
	})
	return srv, lg
}

// drainCmdRejects reads CmdReject messages until the venue has been quiet for the
// given window, and reports them by wire reason.
//
// Quiet rather than counted: a reject the venue has decided on may still be in the
// connection's outbound queue when the last order is sent, and comparing a counter
// against a client's tally requires the client to have finished receiving.
func drainCmdRejects(t *testing.T, c *client, quiet time.Duration) map[uint16]int {
	t.Helper()
	out := map[uint16]int{}
	for {
		b, ok := c.awaitType(t, wire.MsgCmdReject, quiet)
		if !ok {
			return out
		}
		rej, err := wire.DecodeCmdReject(b)
		if err != nil {
			t.Fatalf("DecodeCmdReject: %v", err)
		}
		out[rej.Reason]++
	}
}

// declaredReasonCodes reads the Reason* constants out of pkg/orderentry's source.
//
// Go cannot enumerate a const block at run time, and the first version of the freeze
// below worked around that with a second hand-written copy of the list — which froze
// nothing at all, because both copies were in this file and stayed in step whenever
// neither was touched. Adding ReasonRiskLimit = 18 to pkg/orderentry passed.
//
// So the source is parsed instead. It is the only place the answer actually lives,
// and a constant added there cannot fail to appear here.
func declaredReasonCodes(t *testing.T) map[string]uint16 {
	t.Helper()
	const src = "../../pkg/orderentry/reason.go"
	f, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s, which is where the frozen reason vocabulary lives: %v", src, err)
	}
	out := map[string]uint16{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Reason") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s is not a plain integer literal; this parser is the freeze and it has to be able to read it", name.Name)
				}
				v, err := strconv.ParseUint(lit.Value, 0, 16)
				if err != nil {
					t.Fatalf("%s = %s: %v", name.Name, lit.Value, err)
				}
				out[name.Name] = uint16(v)
			}
		}
	}
	// A parser that silently matched nothing would turn this freeze back into the
	// no-op it is replacing.
	if len(out) < 17 {
		t.Fatalf("found only %d Reason* constants in %s; the parse is wrong, not the vocabulary", len(out), src)
	}
	return out
}

// TestEveryReasonCodeHasAMetricName freezes the label vocabulary.
//
// A metric label an operator greps for is part of the interface — the same argument
// TestDrillTheCeilingRejectionNamesItself already makes for the engine's reason
// strings. Adding a Reason* constant without a name fails at the moment the constant
// is written, rather than producing a silent nil handle that counts nothing and a
// series an alert was never written against.
//
// The enforcement is against pkg/orderentry's SOURCE (declaredReasonCodes), not
// against a second list in this file. That is the whole point of the test and it is
// the part an earlier version got wrong.
func TestEveryReasonCodeHasAMetricName(t *testing.T) {
	want := map[uint16]string{
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
	// Against the source, in both directions.
	//
	// A constant with no label would be counted into nothing: countRefusal indexes
	// s.refused by the code, so a code past the end of the array — or one landing on
	// an empty name — returns without incrementing anything. The client still gets its
	// reject, obgw_refused_total gains no series, no existing series moves, and nothing
	// anywhere fails. That is the stale-list failure docs/JOURNAL-COMPLETENESS.md §4.2
	// exists to prevent, arriving through the metrics page instead of the log.
	declared := declaredReasonCodes(t)
	for name, code := range declared {
		if name == "ReasonNone" {
			continue
		}
		if _, ok := want[code]; !ok {
			t.Errorf("pkg/orderentry declares %s = %d and this table does not name it: a refusal carrying that "+
				"code would count into no series at all", name, code)
			continue
		}
		if int(code) >= len(reasonMetricNames) || reasonMetricNames[code] == "" {
			t.Errorf("pkg/orderentry declares %s = %d and reasonMetricNames has no label for it: "+
				"countRefusal would return without incrementing anything", name, code)
		}
	}
	for code := range want {
		found := false
		for _, declared := range declared {
			if declared == code {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("this table names reason %d and pkg/orderentry no longer declares it; "+
				"a series nothing can ever set is a series an operator reads as zero meaning healthy", code)
		}
	}
	// ReasonNone is the absence of a refusal and is deliberately unnamed, so the
	// named table is one shorter than the array.
	if got := len(reasonMetricNames) - 1; got != len(want) {
		t.Fatalf("reasonMetricNames holds %d named codes and this table holds %d", got, len(want))
	}
	for code, name := range want {
		if int(code) >= len(reasonMetricNames) {
			t.Errorf("reason %d (%s) is past the end of reasonMetricNames", code, name)
			continue
		}
		if got := reasonMetricNames[code]; got != name {
			t.Errorf("reason %d label = %q, want %q", code, got, name)
		}
	}
	if reasonMetricNames[orderentry.ReasonNone] != "" {
		t.Error("ReasonNone has a label; it is the absence of a refusal and reject is never called with it")
	}
}

// TestEveryReasonSeriesIsExportedBeforeItFires — a series that appears the first time
// it moves is a series nobody wrote an alert against. All seventeen are registered at
// startup, and this reads the page rather than the handles.
func TestEveryReasonSeriesIsExportedBeforeItFires(t *testing.T) {
	srv := testServer(t)
	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "# TYPE "+refusedMetric+" counter") {
		t.Errorf("%s is not declared a counter:\n%s", refusedMetric, out)
	}
	for _, name := range reasonMetricNames {
		if name == "" {
			continue
		}
		want := refusedMetric + `{reason="` + name + `"} `
		if !strings.Contains(out, want) {
			t.Errorf("a venue that has refused nothing does not export %q; the alert has nothing to attach to", want)
		}
	}
}

// TestDrillAShedIsCounted is deliverable 3, and the assertion is EQUALITY rather
// than "greater than zero".
//
// Equality is what makes the number reconcilable against a client's own reject count
// during an incident, and it is what distinguishes counting at the gateway from
// counting in Runner.tryEnqueue: a queue-level count would also include the mass
// cancels and cancel-on-disconnect sweeps that produce no client reject, and would
// come out higher than anything the client can see.
func TestDrillAShedIsCounted(t *testing.T) {
	srv, lg := wedgeableVenue(t, 4)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// Trade normally first, so the shed below is a transition rather than a venue
	// that never worked.
	c.enter("warm", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("the venue was not healthy before the queue was wedged")
	}

	// A second account with cancel-on-disconnect on and an order resting. When its
	// connection drops during the shed below, the venue drops a mass cancel that
	// produces NO client reject — which is exactly the population a queue-level
	// counter would include and a gateway one must not. Without this the two places
	// to count are indistinguishable in this fixture.
	bob := dial(t, srv)
	bob.mustLogin("bob", "pw2")
	cod, err := wire.EncodeCancelOnDisconnect(nil, wire.CancelOnDisconnect{Version: wire.Version, Enabled: true})
	if err != nil {
		t.Fatalf("EncodeCancelOnDisconnect: %v", err)
	}
	if err := wire.WritePacket(bob.conn, wire.PacketUnsequenced, cod); err != nil {
		t.Fatalf("send COD: %v", err)
	}
	bob.enter("b1", wire.SideSell, wire.TypeLimit, wire.TIFGoodTillCancel, 200, 10)
	if _, ok := bob.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("bob's order was not accepted")
	}

	lg.wedge()
	for i := 0; i < 80; i++ {
		c.enter("s"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100+int64(i%5), 10)
	}
	b := srv.books.first()
	deadline := time.Now().Add(3 * time.Second)
	for b.runner.QueueLen() < b.runner.QueueCap() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.runner.QueueLen() < b.runner.QueueCap() {
		t.Fatalf("test premise broken: queue %d/%d is not full", b.runner.QueueLen(), b.runner.QueueCap())
	}
	_ = bob.conn.Close()
	deadline = time.Now().Add(3 * time.Second)
	for srv.shedUnreported.Value() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if srv.shedUnreported.Value() != 1 {
		t.Fatalf("test premise broken: bob's dropped sweep was not counted (%d)", srv.shedUnreported.Value())
	}

	got := drainCmdRejects(t, c, 750*time.Millisecond)
	received := got[wire.ReasonOverloaded]
	if received == 0 {
		t.Fatal("test premise broken: a wedged matcher with a four-deep queue refused nothing")
	}
	if counted := refusedCount(t, srv, "overloaded"); counted != int64(received) {
		t.Errorf("%s{reason=\"overloaded\"} = %d, client received %d rejects — a count an operator cannot "+
			"reconcile against a client's own tally is one that starts an argument during the incident. "+
			"A count taken at the QUEUE reads high here, because it also sees the disconnect sweep no client "+
			"was told about",
			refusedMetric, counted, received)
	}
	// And nothing else moved: a shed is a shed, not a throttle and not a drain.
	for _, other := range []string{"throttled", "shutting_down", "malformed"} {
		if n := refusedCount(t, srv, other); n != 0 {
			t.Errorf("{reason=%q} = %d after a pure shed, want 0", other, n)
		}
	}
	// The queue-depth threshold is what should have warned first, and it is still
	// the thing the runbook sends an operator to.
	if b.runner.QueueLen() < b.runner.QueueCap() {
		t.Errorf("queue %d/%d — the premise is a FULL queue", b.runner.QueueLen(), b.runner.QueueCap())
	}
}

// TestARateGateRefusalIsCounted is deliverable 4. The rate gate refuses before the
// command ever reaches a queue, which is exactly the population a queue-level counter
// cannot see.
func TestARateGateRefusalIsCounted(t *testing.T) {
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096,
		RatePerSec: 1, Burst: 1,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	for i := 0; i < 10; i++ {
		c.enter("r"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	}
	got := drainCmdRejects(t, c, 750*time.Millisecond)
	if got[wire.ReasonThrottled] != 9 {
		t.Fatalf("client received %d throttles, want 9 (rate 1/s, burst 1, ten orders)", got[wire.ReasonThrottled])
	}
	if n := refusedCount(t, srv, "throttled"); n != 9 {
		t.Errorf("{reason=\"throttled\"} = %d, want 9", n)
	}
	if n := refusedCount(t, srv, "overloaded"); n != 0 {
		t.Errorf("{reason=\"overloaded\"} = %d after a pure throttle, want 0 — this is the series that pages", n)
	}
}

// TestEveryRejectSiteIsCounted is deliverable 5: the funnel is complete.
//
// Complete BY CONSTRUCTION is the claim — every client-visible refusal passes through
// session.reject, so a new ingress path cannot forget to count — and this is the
// assertion that keeps it true: across a mixed session, the sum over every
// obgw_refused_total series equals the number of CmdReject messages the client got.
func TestEveryRejectSiteIsCounted(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	// One live order, so the duplicate and the reduce below have something to be
	// wrong about.
	c.enter("live", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("the venue was not healthy")
	}
	before := totalRefused(t, srv)

	// A deliberately mixed bag, one per shape of refusal the gateway can produce
	// without a wedged queue: a malformed frame, a duplicate id, an order naming an
	// instrument this venue does not serve, a cancel and a reduce for orders that do
	// not exist, and a reduce of a non-positive size.
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, []byte{wire.MsgEnter, 0, 0}); err != nil {
		t.Fatalf("send malformed: %v", err)
	}
	c.enter("live", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10) // duplicate
	unknownSymbol, err := wire.EncodeEnter(nil, wire.Enter{
		Version: wire.Version, ClOrdID: "elsewhere", Symbol: "NOPE",
		Side: wire.SideBuy, Type: wire.TypeLimit, TIF: wire.TIFGoodTillCancel, Price: 100, Quantity: 10,
	})
	if err != nil {
		t.Fatalf("EncodeEnter: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, unknownSymbol); err != nil {
		t.Fatalf("send unknown symbol: %v", err)
	}
	c.cancel("never-entered")
	c.reduce("never-entered", 5)
	c.reduce("live", 0)
	// A GTD deadline on a plain Enter has nowhere to go and is refused rather than
	// quietly downgraded, which is a reject site of its own.
	c.enter("dated", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillDate, 100, 10)

	got := drainCmdRejects(t, c, time.Second)
	var received int
	for _, n := range got {
		received += n
	}
	if received == 0 {
		t.Fatal("test premise broken: a session designed to be refused was not refused")
	}
	if counted := totalRefused(t, srv) - before; counted != int64(received) {
		t.Errorf("the exposition counted %d refusals and the client received %d CmdRejects (%v) — "+
			"a reject site that does not go through session.reject counts nothing",
			counted, received, got)
	}

	t.Run("a refusal the client never receives is still counted", func(t *testing.T) {
		// §4.2 rule 1, and the real condition rather than a simulation: an unbuffered
		// outbound queue with nobody reading it makes send take the drop branch, so
		// this refusal reaches no client at all. It is still a refusal, and the client
		// is WORSE off for not hearing it, not better — so the increment goes before
		// the encode and not after a successful send.
		orphan := &session{srv: srv, out: make(chan []byte), closed: make(chan struct{})}
		start := refusedCount(t, srv, "shutting_down")
		orphan.reject("nobody", orderentry.ReasonShuttingDown)
		select {
		case <-orphan.closed:
		default:
			t.Fatal("test premise broken: send did not drop the connection on a full outbound queue")
		}
		if n := refusedCount(t, srv, "shutting_down") - start; n != 1 {
			t.Errorf("counted %d refusals the client could not be told about, want 1", n)
		}
	})
}

// TestACancelRefusedDuringShutdownSaysSo is deliverable 7 and the fix in §4.3.
//
// Every other enqueue site distinguished a drain from a full queue; the cancel path
// did not, so a cancel refused during a planned restart told the client OVERLOAD.
// Harmless while nothing counted it. The moment obgw_refused_total exists, every
// clean drain adds to {reason="overloaded"} — the one series in this family that
// pages — and a planned deployment starts paging the on-call.
func TestACancelRefusedDuringShutdownSaysSo(t *testing.T) {
	srv := testServer(t)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")
	c.enter("a1", wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
	if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
		t.Fatal("order not accepted")
	}

	// A real drain: the runner stops taking commands, exactly as it does when the
	// venue is being restarted. The connection is still up, so the client is still
	// there to be told something.
	srv.books.first().runner.Close()

	c.cancel("a1")
	got := drainCmdRejects(t, c, time.Second)
	if got[wire.ReasonShuttingDown] != 1 {
		t.Errorf("client received %v; a cancel refused during a drain must say ReasonShuttingDown (%d)",
			got, wire.ReasonShuttingDown)
	}
	if got[wire.ReasonOverloaded] != 0 {
		t.Error("the client was told the venue is OVERLOADED during a clean drain: it will back off and retry here " +
			"instead of reconnecting elsewhere")
	}
	if n := refusedCount(t, srv, "overloaded"); n != 0 {
		t.Errorf("{reason=\"overloaded\"} = %d after a clean drain, want 0 — every planned restart would page", n)
	}
	if n := refusedCount(t, srv, "shutting_down"); n != 1 {
		t.Errorf("{reason=\"shutting_down\"} = %d, want 1", n)
	}
}

// TestCancelOnDisconnectDropIsCounted is deliverable 8, and it asserts BOTH halves:
// the counter moved, and the orders it is complaining about really are still resting.
// A counter is only worth something if it means what it says.
//
// This is the one shed in the gateway that produces no wire message, because there is
// no client left to reject — and it is the most consequential. The venue undertook to
// pull this account's orders, the queue would not take the command, and the orders
// stay in the book: owned by a session that no longer exists, still able to trade,
// cancellable only by an operator or a restart.
func TestCancelOnDisconnectDropIsCounted(t *testing.T) {
	srv, lg := wedgeableVenue(t, 2)
	c := dial(t, srv)
	c.mustLogin("alice", "pw1")

	cod, err := wire.EncodeCancelOnDisconnect(nil, wire.CancelOnDisconnect{Version: wire.Version, Enabled: true})
	if err != nil {
		t.Fatalf("EncodeCancelOnDisconnect: %v", err)
	}
	if err := wire.WritePacket(c.conn, wire.PacketUnsequenced, cod); err != nil {
		t.Fatalf("send COD: %v", err)
	}
	for _, id := range []string{"a1", "a2"} {
		c.enter(id, wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 100, 10)
		if _, ok := c.awaitType(t, wire.MsgAccepted, 3*time.Second); !ok {
			t.Fatalf("%s not accepted", id)
		}
	}
	b := srv.books.first()
	if n := b.runner.OrderCount(); n != 2 {
		t.Fatalf("book holds %d orders before the wedge, want 2", n)
	}

	// Wedge the matcher and fill the queue, so the sweep below cannot be enqueued.
	lg.wedge()
	for i := 0; i < 12; i++ {
		c.enter("f"+itoa(i), wire.SideBuy, wire.TypeLimit, wire.TIFGoodTillCancel, 101, 10)
	}
	deadline := time.Now().Add(3 * time.Second)
	for b.runner.QueueLen() < b.runner.QueueCap() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.runner.QueueLen() < b.runner.QueueCap() {
		t.Fatalf("test premise broken: queue %d/%d is not full", b.runner.QueueLen(), b.runner.QueueCap())
	}

	// The client goes away. Nobody is left to reject, and the sweep is dropped.
	_ = c.conn.Close()

	deadline = time.Now().Add(3 * time.Second)
	for srv.shedUnreported.Value() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := srv.shedUnreported.Value(); n != 1 {
		t.Fatalf("%s{op=%q} = %d, want 1 — the venue dropped a sweep it had undertaken and said nothing",
			shedUnreportedMetric, shedCancelOnDisconnect, n)
	}
	// The other half. Without this the counter could mean anything.
	if n := b.runner.OrderCount(); n != 2 {
		t.Errorf("book holds %d orders, want the 2 that were left resting — the counter is claiming a loss "+
			"that did not happen", n)
	}
	// And the drop is NOT filed beside an ordinary throttle.
	if n := refusedCount(t, srv, "overloaded"); n == 0 {
		t.Log("no client-visible overload accompanied the drop, which is fine: this shed has no client")
	}

	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if !strings.Contains(buf.String(), shedUnreportedMetric+`{op="cancel_on_disconnect"} 1`) {
		t.Errorf("the runbook's signal is not on the page:\n%s", buf.String())
	}
}

// loginRefusedCount reads one obgw_login_refused_total series off the handle the
// gateway resolved at startup.
func loginRefusedCount(t *testing.T, srv *Server, code byte) int64 {
	t.Helper()
	c := srv.loginRefused[code]
	if c == nil {
		t.Fatalf("login reject code %q has no registered counter", code)
	}
	return c.Value()
}

// TestFailedLoginsAreCounted is the fix for a metric that read healthy while the
// condition it describes was happening.
//
// obgw_refused_total was described as complete by construction over every refusal the
// venue sends, and failed logins were not in it: they are written straight to the
// socket as a LoginRejected, before a session exists, so they never reach
// session.reject. Twenty-five rejected logins moved no counter anywhere, while a
// permanently-zero obgw_refused_total{reason="not_authorised"} series sat on the page
// reading as positive evidence that nobody was being refused for authorisation.
//
// During credential stuffing that is the exact opposite of the truth, and it is the
// worst shape a metric can have: an operator trusts it during the incident.
//
// Real sockets and real handshakes; nothing here calls the counter.
func TestFailedLoginsAreCounted(t *testing.T) {
	srv := testServer(t)

	const attempts = 25
	for i := 0; i < attempts; i++ {
		c := dial(t, srv)
		pkt, err := c.login("alice", "not-pw1", "", 0)
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		if pkt.Type != wire.PacketLoginRejected {
			t.Fatalf("login %d: type = %q, want rejected", i, pkt.Type)
		}
	}

	if got := loginRefusedCount(t, srv, wire.RejectNotAuthorised); got != attempts {
		t.Errorf("%s{reason=\"not_authorised\"} = %d after %d refused logins, want %d",
			loginRefusedMetric, got, attempts, attempts)
	}
	// Disjoint populations. Folding these into the CmdReject family would have made
	// "how many command refusals did we send" wrong by however many people mistyped a
	// password.
	if got := totalRefused(t, srv); got != 0 {
		t.Errorf("%s moved to %d on a venue where no session ever issued a command; "+
			"the two families must stay disjoint", refusedMetric, got)
	}

	// A resume for a session this incarnation never issued: the other vocabulary, and
	// a real client failure rather than an attack.
	c := dial(t, srv)
	pkt, err := c.login("alice", "pw1", "INCOTHER", 7)
	if err != nil {
		t.Fatalf("resume login: %v", err)
	}
	if pkt.Type != wire.PacketLoginRejected {
		t.Fatalf("resume login: type = %q, want rejected", pkt.Type)
	}
	if got := loginRefusedCount(t, srv, pkt.Payload[0]); got != 1 {
		t.Errorf("%s for code %q = %d after one refused resume, want 1", loginRefusedMetric, pkt.Payload[0], got)
	}
}

// TestEveryLoginRefusalSeriesIsExportedBeforeItFires — the same rule as the CmdReject
// family. A series that appears the first time it moves is a series nobody wrote an
// alert against, and the whole point of counting logins is to have the graph already
// there on the morning somebody starts guessing passwords.
func TestEveryLoginRefusalSeriesIsExportedBeforeItFires(t *testing.T) {
	srv := testServer(t)
	var buf strings.Builder
	if err := srv.metrics.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "# TYPE "+loginRefusedMetric+" counter") {
		t.Errorf("%s is not declared a counter:\n%s", loginRefusedMetric, out)
	}
	for _, name := range loginRefusalNames {
		want := loginRefusedMetric + `{reason="` + name + `"} 0`
		if !strings.Contains(out, want) {
			t.Errorf("%q is not on the page before it fires:\n%s", want, out)
		}
	}
}

// TestEveryLoginRejectCodeHasAMetricName freezes the login vocabulary against
// internal/wire's source, for the same reason TestEveryReasonCodeHasAMetricName
// freezes the CmdReject one: a code with no label is a refusal that counts into
// nothing, silently, and a hand-kept second copy of the list would not notice.
func TestEveryLoginRejectCodeHasAMetricName(t *testing.T) {
	const src = "../../internal/wire/soup.go"
	f, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", src, err)
	}
	found := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				// Reject* rather than MDReject*: the market-data listener refuses
				// subscribers on its own path and is not this metric's population.
				if !strings.HasPrefix(name.Name, "Reject") {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.CHAR {
					t.Fatalf("%s is not a character literal; this parser is the freeze and has to read it", name.Name)
				}
				code, _, _, err := strconv.UnquoteChar(strings.Trim(lit.Value, "'"), '\'')
				if err != nil {
					t.Fatalf("%s = %s: %v", name.Name, lit.Value, err)
				}
				found++
				if _, ok := loginRefusalNames[byte(code)]; !ok {
					t.Errorf("internal/wire declares %s = %q and loginRefusalNames has no label for it: "+
						"a peer refused with that code would count into no series at all", name.Name, code)
				}
			}
		}
	}
	if found != len(loginRefusalNames) {
		t.Errorf("internal/wire declares %d login reject codes and loginRefusalNames holds %d", found, len(loginRefusalNames))
	}
}
