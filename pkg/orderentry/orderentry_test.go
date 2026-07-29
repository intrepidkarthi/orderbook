package orderentry

import (
	"errors"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// harness wires an engine to a registry through a publisher, the same way the
// server does, so these tests exercise the real path rather than a stub.
type harness struct {
	eng *matching.Engine
	reg *Registry
	pub *Publisher
}

func newHarness(t *testing.T, ring int) *harness {
	t.Helper()
	reg := NewRegistry("INC0000001", ring)
	pub := NewPublisher(reg, 1<<12)
	go pub.Pump()

	cfg := matching.DefaultConfig("X")
	cfg.EventSink = pub
	h := &harness{eng: matching.NewEngine(cfg), reg: reg, pub: pub}
	t.Cleanup(func() { pub.Close() })
	return h
}

func (h *harness) submit(t *testing.T, user, clOrdID string, side types.Side, price, qty int64) *types.Order {
	t.Helper()
	o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ClientOrderID = clOrdID
	h.eng.Process(o)
	return o
}

func (h *harness) msgs(t *testing.T, account string) []Msg {
	t.Helper()
	h.pub.Wait()
	got, err := h.reg.Stream(account).Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	return got
}

func kinds(ms []Msg) []MsgKind {
	out := make([]MsgKind, len(ms))
	for i, m := range ms {
		out[i] = m.Kind
	}
	return out
}

// TestAcceptedReachesOwner is the simplest path end to end.
func TestAcceptedReachesOwner(t *testing.T) {
	h := newHarness(t, 256)
	h.submit(t, "alice", "a1", types.SideBuy, 100, 5)

	got := h.msgs(t, "alice")
	if len(got) != 1 || got[0].Kind != KindAccepted {
		t.Fatalf("alice got %v, want one Accepted", kinds(got))
	}
	if got[0].ClOrdID != "a1" || got[0].Price != 100 || got[0].Quantity != 5 {
		t.Errorf("message = %+v", got[0])
	}
	if got[0].Seq != 1 {
		t.Errorf("first sequence = %d, want 1", got[0].Seq)
	}
}

// TestBothSidesHearAboutAFill is the property the whole package exists for: the
// resting maker learns of a fill it did not initiate.
func TestBothSidesHearAboutAFill(t *testing.T) {
	h := newHarness(t, 256)
	h.submit(t, "maker", "m1", types.SideBuy, 100, 10)
	h.submit(t, "taker", "t1", types.SideSell, 100, 4)

	maker := h.msgs(t, "maker")
	if len(maker) != 2 || maker[1].Kind != KindExecuted {
		t.Fatalf("maker got %v, want Accepted then Executed", kinds(maker))
	}
	if maker[1].ClOrdID != "m1" {
		t.Errorf("maker's fill carries ClOrdID %q, want m1", maker[1].ClOrdID)
	}
	if maker[1].Quantity != 4 {
		t.Errorf("maker filled %d, want 4", maker[1].Quantity)
	}
	if maker[1].LeavesQty != 6 {
		t.Errorf("maker LeavesQty = %d, want 6", maker[1].LeavesQty)
	}

	taker := h.msgs(t, "taker")
	if len(taker) < 2 {
		t.Fatalf("taker got %v, want at least Accepted and Executed", kinds(taker))
	}
	var sawExec bool
	for _, m := range taker {
		if m.Kind == KindExecuted {
			sawExec = true
			if m.ClOrdID != "t1" {
				t.Errorf("taker's fill carries ClOrdID %q, want t1", m.ClOrdID)
			}
			if m.LeavesQty != 0 {
				t.Errorf("taker LeavesQty = %d, want 0 (fully filled)", m.LeavesQty)
			}
		}
	}
	if !sawExec {
		t.Error("taker never heard about its own fill")
	}
}

// TestFillWhileDisconnectedIsRetained is the reason Stream outlives Session. The
// maker has no connection at all here — there is no session in this test — and
// the fill must still be waiting when it comes back.
func TestFillWhileDisconnectedIsRetained(t *testing.T) {
	h := newHarness(t, 256)
	h.submit(t, "maker", "m1", types.SideBuy, 100, 10)

	// Maker "disconnects": it reads its cursor and goes away. Drain first, or the
	// cursor is captured before its own Accepted has been fanned out and the test
	// would be resuming from a point the client never actually reached.
	h.pub.Wait()
	seen := h.reg.Stream("maker").Seq()

	h.submit(t, "taker", "t1", types.SideSell, 100, 10)
	h.pub.Wait()

	missed, err := h.reg.Resume(h.reg.Incarnation(), "maker", seen)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(missed) == 0 {
		t.Fatal("maker reconnected and was told nothing; its fill was lost")
	}
	if missed[0].Kind != KindExecuted || missed[0].LeavesQty != 0 {
		t.Errorf("missed[0] = %+v, want a fully-filling Executed", missed[0])
	}
}

// TestResumeRejectsAnotherIncarnation — a restarted venue must refuse a stale
// cursor rather than serve different content under numbers the client thinks it
// already has.
func TestResumeRejectsAnotherIncarnation(t *testing.T) {
	h := newHarness(t, 256)
	h.submit(t, "alice", "a1", types.SideBuy, 100, 1)
	h.pub.Wait()

	if _, err := h.reg.Resume("INC-SOMETHING-ELSE", "alice", 0); !errors.Is(err, ErrNoSuchStream) {
		t.Errorf("resume against a foreign incarnation: err = %v, want ErrNoSuchStream", err)
	}
}

// TestResumeRefusesEvictedSequence — a bounded ring must say "you are behind"
// rather than hand back a partial history that looks complete.
func TestResumeRefusesEvictedSequence(t *testing.T) {
	h := newHarness(t, 4) // tiny ring
	for i := 0; i < 20; i++ {
		h.submit(t, "alice", string(rune('a'+i)), types.SideBuy, int64(100-i), 1)
	}
	h.pub.Wait()

	if _, err := h.reg.Resume(h.reg.Incarnation(), "alice", 1); !errors.Is(err, ErrSequenceEvicted) {
		t.Errorf("resume from an evicted point: err = %v, want ErrSequenceEvicted", err)
	}
	// A cursor still inside the ring is served.
	s := h.reg.Stream("alice")
	if _, err := h.reg.Resume(h.reg.Incarnation(), "alice", s.Seq()-1); err != nil {
		t.Errorf("resume from inside the ring: %v", err)
	}
}

// TestResumeAheadOfVenueIsRefused — a client claiming messages the venue never
// sent is out of step and must be told.
func TestResumeAheadOfVenueIsRefused(t *testing.T) {
	h := newHarness(t, 64)
	h.submit(t, "alice", "a1", types.SideBuy, 100, 1)
	h.pub.Wait()

	if _, err := h.reg.Resume(h.reg.Incarnation(), "alice", 9999); !errors.Is(err, ErrSequenceEvicted) {
		t.Errorf("cursor ahead of the venue: err = %v, want ErrSequenceEvicted", err)
	}
}

// TestSequencesAreDenseAndPerAccount — a client detects loss by its own gap, so
// its numbering must not skip because another account traded.
func TestSequencesAreDenseAndPerAccount(t *testing.T) {
	h := newHarness(t, 256)
	h.submit(t, "alice", "a1", types.SideBuy, 100, 1)
	h.submit(t, "bob", "b1", types.SideBuy, 99, 1)
	h.submit(t, "alice", "a2", types.SideBuy, 98, 1)
	h.submit(t, "bob", "b2", types.SideBuy, 97, 1)

	for _, acct := range []string{"alice", "bob"} {
		got := h.msgs(t, acct)
		for i, m := range got {
			if m.Seq != uint64(i+1) {
				t.Errorf("%s message %d has Seq %d, want %d — the sequence is not dense", acct, i, m.Seq, i+1)
			}
		}
	}
}

// TestRejectionCarriesAReason — a client must be able to branch on why.
func TestRejectionCarriesAReason(t *testing.T) {
	reg := NewRegistry("INC1", 64)
	pub := NewPublisher(reg, 1024)
	go pub.Pump()
	defer pub.Close()

	cfg := matching.DefaultConfig("X")
	cfg.MaxOrderQty = 10
	cfg.EventSink = pub
	eng := matching.NewEngine(cfg)

	o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, 100, 500, types.TIFGoodTillCancel)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	o.ClientOrderID = "big"
	eng.Process(o)
	pub.Wait()

	got, err := reg.Stream("alice").Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 1 || got[0].Kind != KindRejected {
		t.Fatalf("got %v, want one Rejected", kinds(got))
	}
	if got[0].Reason != ReasonTooLarge {
		t.Errorf("reason = %d, want ReasonTooLarge (%d)", got[0].Reason, ReasonTooLarge)
	}
}

// TestPublisherNeverBlocksTheMatcher — the queue is bounded and OnEvents drops
// rather than waiting, because waiting would stop the venue.
func TestPublisherNeverBlocksTheMatcher(t *testing.T) {
	reg := NewRegistry("INC1", 8)
	pub := NewPublisher(reg, 4) // deliberately tiny; no pump running at all
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = pub
	eng := matching.NewEngine(cfg)

	// With no pump draining, this must still return promptly rather than deadlock.
	for i := 0; i < 500; i++ {
		o, err := types.NewOrder("alice", "X", types.SideBuy, types.OrderTypeLimit, int64(100+i%50), 1, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		eng.Process(o)
	}
	if pub.Dropped() == 0 {
		t.Error("nothing was dropped with a 4-deep queue and no pump; the bound is not being enforced")
	}
}
