package study

import (
	"math"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// testSeeds is the fixed seed set the comparative claims are averaged over.
// Averaging matters: single seeds are noisy, and a test that happened to pick a
// favourable one would be measuring luck.
var testSeeds = []int64{1, 2, 3, 4, 5}

func execFor(seed int64) ExecutionResult {
	return RunExecution(ExecutionConfig{Seed: seed, Quantity: 400, Slices: 10, SliceGap: 20})
}

// Both arms must begin from a byte-identical book, or nothing downstream is a
// controlled comparison.
func TestRunExecution_ArmsShareStartingBook(t *testing.T) {
	for _, seed := range testSeeds {
		r := execFor(seed)
		if r.Block.ArrivalMid != r.Sliced.ArrivalMid {
			t.Errorf("seed %d: arrival mids differ (block %.2f, sliced %.2f) — "+
				"the arms did not start from the same book",
				seed, r.Block.ArrivalMid, r.Sliced.ArrivalMid)
		}
	}
}

func TestRunExecution_QuantityAccounting(t *testing.T) {
	const qty = 400
	for _, seed := range testSeeds {
		r := execFor(seed)
		for _, arm := range []ExecutionArm{r.Block, r.Sliced} {
			if arm.Filled+arm.Unfilled != qty {
				t.Errorf("seed %d %s: filled %d + unfilled %d != %d",
					seed, arm.Name, arm.Filled, arm.Unfilled, qty)
			}
			if arm.Filled < 0 || arm.Unfilled < 0 {
				t.Errorf("seed %d %s: negative quantities %+v", seed, arm.Name, arm)
			}
		}
	}
}

func TestRunExecution_Deterministic(t *testing.T) {
	a, b := execFor(2), execFor(2)
	if a != b {
		t.Errorf("non-deterministic:\n a = %+v\n b = %+v", a, b)
	}
}

// The headline result: releasing the same quantity gradually costs less per lot
// than dumping it in one marketable order.
func TestRunExecution_SlicingCostsLessPerLot(t *testing.T) {
	var block, sliced float64
	for _, seed := range testSeeds {
		r := execFor(seed)
		block += r.Block.SlipPerLot
		sliced += r.Sliced.SlipPerLot
	}
	n := float64(len(testSeeds))
	block, sliced = block/n, sliced/n

	if !(sliced < block) {
		t.Errorf("mean slip per lot: sliced %.3f, block %.3f — expected slicing to be cheaper",
			sliced, block)
	}
}

// A block order shoves the price further at the moment it lands. This is the
// temporary impact that slicing exists to avoid.
func TestRunExecution_BlockCausesLargerImmediateImpact(t *testing.T) {
	var block, sliced float64
	for _, seed := range testSeeds {
		r := execFor(seed)
		block += r.Block.RealizedImpact
		sliced += r.Sliced.RealizedImpact
	}
	n := float64(len(testSeeds))
	if block/n <= sliced/n {
		t.Errorf("mean realized impact: block %.3f, sliced %.3f — expected the block to move price more",
			block/n, sliced/n)
	}
}

// A single marketable order can exhaust the resting book; a schedule lets it
// replenish. Completion, not just price, is part of execution quality.
func TestRunExecution_BlockCanFailToComplete(t *testing.T) {
	var blockUnfilled, slicedUnfilled int64
	for _, seed := range testSeeds {
		r := execFor(seed)
		blockUnfilled += r.Block.Unfilled
		slicedUnfilled += r.Sliced.Unfilled
	}
	if blockUnfilled == 0 {
		t.Skip("no block shortfall at these seeds; nothing to compare")
	}
	if slicedUnfilled >= blockUnfilled {
		t.Errorf("sliced left %d lots unfilled vs block's %d — slicing should complete at least as often",
			slicedUnfilled, blockUnfilled)
	}
}

func TestRunExecution_ShortfallMatchesPerLot(t *testing.T) {
	for _, seed := range testSeeds {
		r := execFor(seed)
		for _, arm := range []ExecutionArm{r.Block, r.Sliced} {
			if arm.Filled == 0 {
				continue
			}
			want := arm.SlipPerLot * float64(arm.Filled)
			if math.Abs(arm.Shortfall-want) > 1e-6 {
				t.Errorf("seed %d %s: shortfall %.6f != slipPerLot·filled %.6f",
					seed, arm.Name, arm.Shortfall, want)
			}
		}
	}
}

// A buyer who pays above the arrival mid has a positive cost. The sign
// convention has to survive the sell side too, where paying "more" means
// receiving less.
func TestSignedCost_Orientation(t *testing.T) {
	if got := signedCost(types.SideBuy, 5); got != 5 {
		t.Errorf("buy paying 5 above arrival = %v, want +5 (a cost)", got)
	}
	if got := signedCost(types.SideBuy, -5); got != -5 {
		t.Errorf("buy paying 5 below arrival = %v, want -5 (a gain)", got)
	}
	if got := signedCost(types.SideSell, -5); got != 5 {
		t.Errorf("sell 5 below arrival = %v, want +5 (a cost)", got)
	}
	if got := signedCost(types.SideSell, 5); got != -5 {
		t.Errorf("sell 5 above arrival = %v, want -5 (a gain)", got)
	}
}

func TestRunExecution_SellSideIsCostedTheSameWay(t *testing.T) {
	r := RunExecution(ExecutionConfig{
		Seed: 1, Quantity: 400, Slices: 10, SliceGap: 20, Side: types.SideSell,
	})

	if r.Block.Filled == 0 || r.Sliced.Filled == 0 {
		t.Fatalf("sell side executed nothing: %+v", r)
	}
	// On the sell side the cost is (arrival − VWAP)·filled, the mirror of the buy
	// side. Asserting the identity directly catches a sign inversion that a
	// one-directional inequality could sit through unnoticed.
	for _, arm := range []ExecutionArm{r.Block, r.Sliced} {
		want := (arm.ArrivalMid - arm.VWAP) * float64(arm.Filled)
		if math.Abs(arm.Shortfall-want) > 1e-6 {
			t.Errorf("%s: shortfall %.6f, want (arrival %.2f − VWAP %.2f)·%d = %.6f",
				arm.Name, arm.Shortfall, arm.ArrivalMid, arm.VWAP, arm.Filled, want)
		}
	}

	// And selling into a book does push the price down, so the cost is real.
	if !(r.Block.Shortfall > 0) {
		t.Errorf("block sell shortfall = %.2f, want positive: hitting bids walks the price down",
			r.Block.Shortfall)
	}
}

func TestExecutionConfig_DefaultsAreUsable(t *testing.T) {
	r := RunExecution(ExecutionConfig{Seed: 1})
	if r.Block.Filled == 0 && r.Sliced.Filled == 0 {
		t.Error("zero-value config executed nothing")
	}
	if r.Block.Name != "block" || r.Sliced.Name != "sliced" {
		t.Errorf("arm names = %q/%q, want block/sliced", r.Block.Name, r.Sliced.Name)
	}
}
