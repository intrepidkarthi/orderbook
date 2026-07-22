package backtest

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/strategy"
)

func asQuoter(t *testing.T) *strategy.AvellanedaStoikov {
	t.Helper()
	a, err := strategy.NewAvellanedaStoikov(strategy.ASParams{Gamma: 0.1, Kappa: 1.5, Sigma: 0.3})
	if err != nil {
		t.Fatalf("strategy: %v", err)
	}
	return a
}

func cfg(seed int64, q Quoter) Config {
	return Config{
		Symbol:       "SIM",
		Steps:        3000,
		Seed:         seed,
		InitialPrice: 100,
		Quoter:       q,
	}
}

func TestRun_MechanicsAndDeterminism(t *testing.T) {
	a := Run(cfg(1, asQuoter(t)))
	b := Run(cfg(1, asQuoter(t)))

	// Mechanics: the maker actually trades and every path is recorded per step.
	if a.Fills == 0 {
		t.Error("expected the market maker to get fills")
	}
	if a.Volume <= 0 {
		t.Error("expected positive traded volume")
	}
	for _, n := range []int{len(a.PnL), len(a.InventoryPath), len(a.MidPath)} {
		if n != a.Steps {
			t.Errorf("path length = %d, want Steps=%d", n, a.Steps)
		}
	}

	// Determinism: same seed ⇒ identical scorecard.
	if a.Fills != b.Fills || a.FinalPnL != b.FinalPnL || a.Sharpe != b.Sharpe {
		t.Errorf("non-deterministic: fills %d/%d pnl %.6f/%.6f sharpe %.6f/%.6f",
			a.Fills, b.Fills, a.FinalPnL, b.FinalPnL, a.Sharpe, b.Sharpe)
	}
	if a.FinalInventory != b.FinalInventory {
		t.Errorf("inventory differs: %d vs %d", a.FinalInventory, b.FinalInventory)
	}
}

func TestRun_InventoryStaysBounded(t *testing.T) {
	// The reservation-price skew steers inventory back toward flat, so the maker
	// should not accumulate a runaway position relative to its gross volume. The
	// bound is looser than with continuous prices: at mid≈100 on an integer tick
	// grid the sub-tick skew rounds away until inventory is sizeable, so control
	// kicks in later — still bounded, just coarser.
	r := Run(cfg(1, asQuoter(t)))
	if r.MaxAbsInventory >= 250 {
		t.Errorf("max abs inventory = %d, expected well-bounded (<250)", r.MaxAbsInventory)
	}
	// Round-tripping, not accumulating: |final inventory| << gross volume.
	if abs64(r.FinalInventory) > r.Volume {
		t.Errorf("final inventory %d exceeds volume %d (not round-tripping)",
			r.FinalInventory, r.Volume)
	}
}

// fixedQuoter is a trivial constant-spread maker used to prove the harness works
// with any Quoter, not just Avellaneda–Stoikov.
type fixedQuoter struct{ half float64 }

func (f fixedQuoter) Quote(mid, _, _ float64) strategy.Quote {
	return strategy.Quote{Bid: mid - f.half, Ask: mid + f.half, Reservation: mid, Spread: 2 * f.half}
}

func TestRun_QuoterAgnostic(t *testing.T) {
	r := Run(cfg(2, fixedQuoter{half: 2}))
	if r.Fills == 0 {
		t.Error("fixed-spread quoter should also get fills")
	}
	if len(r.PnL) != r.Steps {
		t.Errorf("pnl path = %d, want %d", len(r.PnL), r.Steps)
	}
}
