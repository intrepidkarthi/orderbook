package orderentry

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Routing a bust is harder than routing a fill, and the reason is worth stating:
// by the time a bust arrives both orders have usually left the book, so `orders`
// has forgotten them. The Registry keeps a bounded memory of recent prints for
// exactly this. See docs/TRADE-BUST.md.

// bustVenue drives a real engine into a registry and returns both, plus the id of
// the one print it produced.
func bustVenue(t *testing.T) (*Registry, *matching.Engine, int64) {
	t.Helper()
	reg := NewRegistry("INC1", 128)
	cfg := matching.DefaultConfig("X")
	cfg.EventSink = matching.MultiSink{NewNameIndex(reg), sinkFunc(reg.Publish)}
	e := matching.NewEngine(cfg)

	mk := func(user, clOrdID string, side types.Side, qty int64) *types.Order {
		o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, 100, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		o.ClientOrderID = clOrdID
		return o
	}
	e.Process(mk("maker", "m-1", types.SideSell, 10))
	res := e.Process(mk("taker", "t-1", types.SideBuy, 4))
	if len(res.Trades) != 1 {
		t.Fatalf("setup: %d trades, want 1", len(res.Trades))
	}
	return reg, e, res.Trades[0].ID
}

type sinkFunc func([]matching.Event)

func (f sinkFunc) OnEvents(evs []matching.Event) { f(evs) }

// msgsOfKind drains an account's stream and returns the messages of one kind.
func msgsOfKind(t *testing.T, reg *Registry, account string, kind MsgKind) []Msg {
	t.Helper()
	all, err := reg.Stream(account).Since(0)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	var out []Msg
	for _, m := range all {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

// TestExecutedCarriesTheTradeID — the precondition for everything else. A fill
// reported without a name cannot be annulled later by any message.
func TestExecutedCarriesTheTradeID(t *testing.T) {
	reg, _, tradeID := bustVenue(t)

	for _, acct := range []string{"maker", "taker"} {
		fills := msgsOfKind(t, reg, acct, KindExecuted)
		if len(fills) != 1 {
			t.Fatalf("%s got %d fills, want 1", acct, len(fills))
		}
		if fills[0].TradeID != tradeID {
			t.Errorf("%s fill TradeID = %d, want %d", acct, fills[0].TradeID, tradeID)
		}
	}
}

// TestBustReachesBothCounterparties — and only them.
func TestBustReachesBothCounterparties(t *testing.T) {
	reg, e, tradeID := bustVenue(t)

	if err := e.Bust(tradeID, "erroneous order entry"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	for _, acct := range []string{"maker", "taker"} {
		busts := msgsOfKind(t, reg, acct, KindBusted)
		if len(busts) != 1 {
			t.Fatalf("%s got %d bust messages, want 1", acct, len(busts))
		}
		if busts[0].TradeID != tradeID {
			t.Errorf("%s bust names trade %d, want %d", acct, busts[0].TradeID, tradeID)
		}
		if busts[0].ClOrdID == "" {
			t.Errorf("%s bust carries no ClOrdID, so the client cannot tell which order filled", acct)
		}
	}
	// A third party hears nothing: a bust is a private message about a specific
	// fill. The public version of the same fact is on the market-data feed.
	if got := msgsOfKind(t, reg, "bystander", KindBusted); len(got) != 0 {
		t.Errorf("an uninvolved account received %d bust messages", len(got))
	}
}

// TestBustRoutesAfterTheOrdersAreGone is the case the naive implementation gets
// wrong. Both orders leave the book before the bust arrives — the taker's filled
// completely, the maker cancels the rest — so a router that looked them up in the
// live-order map would find nothing and tell nobody.
func TestBustRoutesAfterTheOrdersAreGone(t *testing.T) {
	reg, e, tradeID := bustVenue(t)

	id, ok := reg.OrderIDFor("maker", "m-1")
	if !ok {
		t.Fatal("setup: the maker's order cannot be named")
	}
	if _, err := e.Cancel(id, "maker"); err != nil {
		t.Fatalf("setup: Cancel: %v", err)
	}
	if err := e.Bust(tradeID, "erroneous"); err != nil {
		t.Fatalf("Bust: %v", err)
	}

	for _, acct := range []string{"maker", "taker"} {
		if got := msgsOfKind(t, reg, acct, KindBusted); len(got) != 1 {
			t.Errorf("%s got %d bust messages after its order left the book, want 1", acct, len(got))
		}
	}
}

// TestBustOlderThanTheFillMemoryIsCounted — the bound is real, so what happens at
// the bound has to be an operational fact rather than silence.
func TestBustOlderThanTheFillMemoryIsCounted(t *testing.T) {
	reg, e, tradeID := bustVenue(t)
	reg.SetFillMemory(1)

	// Print again, evicting the first trade from the memory.
	mk := func(user, clOrdID string, side types.Side, qty int64) *types.Order {
		o, err := types.NewOrder(user, "X", side, types.OrderTypeLimit, 100, qty, types.TIFGoodTillCancel)
		if err != nil {
			t.Fatalf("NewOrder: %v", err)
		}
		o.ClientOrderID = clOrdID
		return o
	}
	e.Process(mk("maker2", "m-2", types.SideSell, 5))
	if res := e.Process(mk("taker2", "t-2", types.SideBuy, 5)); len(res.Trades) != 1 {
		t.Fatalf("setup: second print did not trade")
	}

	before := reg.UnroutableBusts()
	if err := e.Bust(tradeID, "too late"); err != nil {
		t.Fatalf("Bust: %v", err)
	}
	if got := reg.UnroutableBusts(); got != before+1 {
		t.Errorf("UnroutableBusts = %d, want %d — an undeliverable bust vanished silently", got, before+1)
	}
	if got := msgsOfKind(t, reg, "maker", KindBusted); len(got) != 0 {
		t.Errorf("maker received %d bust messages for an evicted print", len(got))
	}
}
