package sim

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/shopspring/decimal"
)

func baseConfig(seed int64) Config {
	return Config{
		Symbol:       "SIM",
		Steps:        2000,
		Seed:         seed,
		InitialPrice: decimal.NewFromInt(100),
	}
}

func TestRun_Deterministic(t *testing.T) {
	a := Run(baseConfig(42))
	b := Run(baseConfig(42))

	// Compare the matching *outcome* (ValueDigest), not the per-run UUID order
	// ids, which are non-deterministic by design.
	if da, db := marketdata.ValueDigest(a.Trades), marketdata.ValueDigest(b.Trades); da != db {
		t.Errorf("same seed produced different trades:\n a=%s\n b=%s", da, db)
	}
	if len(a.Prices) != len(b.Prices) {
		t.Fatalf("price path lengths differ: %d vs %d", len(a.Prices), len(b.Prices))
	}
	for i := range a.Prices {
		if !a.Prices[i].Equal(b.Prices[i]) {
			t.Fatalf("price path diverges at step %d: %s vs %s", i, a.Prices[i], b.Prices[i])
		}
	}
}

func TestRun_ProducesTradesAndValidBook(t *testing.T) {
	r := Run(baseConfig(7))

	if len(r.Trades) == 0 {
		t.Error("expected the simulation to produce trades")
	}
	if len(r.Prices) != 2000 {
		t.Errorf("price path len = %d, want 2000", len(r.Prices))
	}
	// The resting book must never be crossed (best bid < best ask).
	if len(r.Final.Bids) > 0 && len(r.Final.Asks) > 0 {
		if !r.Final.Bids[0].Price.LessThan(r.Final.Asks[0].Price) {
			t.Errorf("final book crossed: best bid %s !< best ask %s",
				r.Final.Bids[0].Price, r.Final.Asks[0].Price)
		}
	}
}

func TestRun_DifferentSeedDiffers(t *testing.T) {
	a := Run(baseConfig(1))
	b := Run(baseConfig(2))
	if marketdata.ValueDigest(a.Trades) == marketdata.ValueDigest(b.Trades) {
		t.Error("different seeds should produce different trade streams")
	}
}

func TestRun_CustomAgentsAndSteps(t *testing.T) {
	cfg := Config{
		Symbol:       "SIM",
		Steps:        50,
		Seed:         3,
		InitialPrice: decimal.NewFromInt(50),
		Agents: []Agent{
			DefaultNoiseTrader("mm-a"),
			DefaultNoiseTrader("mm-b"),
		},
	}
	r := Run(cfg)
	if len(r.Prices) != 50 {
		t.Errorf("price path len = %d, want 50", len(r.Prices))
	}
	if r.Engine == nil || r.Final == nil {
		t.Error("result should carry engine and final snapshot")
	}
}
