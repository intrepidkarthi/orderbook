package study

import (
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// execUserID is the identity the executing trader submits under. It must differ
// from the noise population so self-trade prevention never cancels our orders.
const execUserID = "exec"

// ExecutionConfig parameterizes the block-versus-sliced execution study
// (docs/research-roadmap.md §2, experiment 4).
type ExecutionConfig struct {
	Symbol        string
	Seed          int64
	InitialPrice  int64 // ticks
	Warmup        int   // steps run to build a book before executing
	Quantity      int64 // total lots to execute
	Side          types.Side
	Slices        int // children in the sliced arm
	SliceGap      int // simulator steps between children (liquidity recovery)
	RecoverySteps int // steps after the execution window, before permanent impact
	Noise         *sim.NoiseTrader
}

func (c *ExecutionConfig) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.InitialPrice == 0 {
		c.InitialPrice = 1000
	}
	if c.Warmup <= 0 {
		c.Warmup = 400
	}
	if c.Quantity <= 0 {
		c.Quantity = 400
	}
	if c.Side == "" {
		c.Side = types.SideBuy
	}
	if c.Slices <= 0 {
		c.Slices = 10
	}
	if c.SliceGap <= 0 {
		c.SliceGap = 20
	}
	if c.RecoverySteps <= 0 {
		c.RecoverySteps = 200
	}
	if c.Noise == nil {
		nt := sim.DefaultNoiseTrader("noise")
		nt.MaxOffsetTicks = 40
		c.Noise = nt
	}
}

// window is the number of steps the execution spans. Both arms run it so that
// permanent impact is measured at the same point on the clock for each.
func (c *ExecutionConfig) window() int { return (c.Slices - 1) * c.SliceGap }

// ExecutionArm is one execution strategy's scorecard. All prices are in ticks;
// shortfall is in tick·lots.
type ExecutionArm struct {
	Name       string
	ArrivalMid float64 // mid immediately before the first child
	VWAP       float64 // volume-weighted average execution price
	Filled     int64   // lots actually executed
	Unfilled   int64   // lots the book could not absorb

	// Shortfall is the implementation shortfall against the arrival mid, signed
	// so that positive is always a cost: (VWAP − arrival)·filled for a buy, and
	// (arrival − VWAP)·filled for a sell.
	Shortfall float64

	// SlipPerLot is Shortfall/Filled — the comparable per-lot cost in ticks.
	SlipPerLot float64

	// RealizedImpact is the mid displacement at the end of the execution window,
	// PermanentImpact the displacement after a further recovery period. Both are
	// signed in the direction of the trade, so positive means the price moved
	// against the executor.
	RealizedImpact  float64
	PermanentImpact float64
}

// ExecutionResult holds both arms, run from an identical book and seed.
type ExecutionResult struct {
	Block  ExecutionArm
	Sliced ExecutionArm
}

// RunExecution executes the same quantity two ways from the same starting book —
// one block marketable order versus the quantity sliced into children released
// over time — and scores both.
//
// The arms are built by replaying an identical warmup from the same seed, so any
// difference between them is caused by the execution schedule and nothing else.
func RunExecution(cfg ExecutionConfig) ExecutionResult {
	cfg.applyDefaults()
	return ExecutionResult{
		Block:  runArm(cfg, 1),
		Sliced: runArm(cfg, cfg.Slices),
	}
}

// runArm executes cfg.Quantity spread over n children and returns its scorecard.
// n == 1 is the block arm.
func runArm(cfg ExecutionConfig, n int) ExecutionArm {
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))
	ref := cfg.InitialPrice

	step := func() {
		if mid, ok := eng.MidPrice(); ok {
			ref = mid
		} else if ltp := eng.LastTradePrice(); ltp > 0 {
			ref = ltp
		}
		view := sim.View{
			Symbol:   cfg.Symbol,
			Snapshot: eng.Snapshot(10),
			Ref:      ref,
			HasBook:  eng.OrderCount() > 0,
		}
		for _, o := range cfg.Noise.Act(view, rng) {
			eng.Process(o)
		}
	}

	for range cfg.Warmup {
		step()
	}

	arrival, _ := floatMid(eng)
	name := "block"
	if n > 1 {
		name = "sliced"
	}
	arm := ExecutionArm{Name: name, ArrivalMid: arrival}

	// Children are released at even intervals across the window. Integer division
	// leaves a remainder, which the final child carries so the full quantity is
	// always attempted.
	child := cfg.Quantity / int64(n)
	var sentQty, notional int64

	for i := range n {
		qty := child
		if i == n-1 {
			qty = cfg.Quantity - sentQty
		}
		sentQty += qty

		if qty > 0 {
			o, err := types.NewOrder(execUserID, cfg.Symbol, cfg.Side,
				types.OrderTypeMarket, 0, qty, types.TIFImmediateOrCancel)
			if err == nil {
				for _, tr := range eng.Process(o).Trades {
					arm.Filled += tr.Quantity
					notional += tr.Price * tr.Quantity
				}
			}
		}

		// Let the book breathe between children — this recovery is exactly what
		// slicing is buying.
		if i < n-1 {
			for range cfg.SliceGap {
				step()
			}
		}
	}

	arm.Unfilled = cfg.Quantity - arm.Filled
	if arm.Filled > 0 {
		arm.VWAP = float64(notional) / float64(arm.Filled)
		arm.Shortfall = signedCost(cfg.Side, arm.VWAP-arm.ArrivalMid) * float64(arm.Filled)
		arm.SlipPerLot = arm.Shortfall / float64(arm.Filled)
	}

	// Realized impact is read immediately after each arm's final child, which is
	// the peak displacement that arm caused. Measuring it at a common clock step
	// instead would flatter the block, whose pressure has had the whole window to
	// decay while the sliced arm has only just stopped pushing.
	if mid, ok := floatMid(eng); ok {
		arm.RealizedImpact = signedCost(cfg.Side, mid-arm.ArrivalMid)
	}

	// Permanent impact *is* measured on a common clock: the block arm idles through
	// the window it did not use, so both arms are scored the same elapsed distance
	// from arrival and the comparison is like for like.
	if n == 1 {
		for range cfg.window() {
			step()
		}
	}
	for range cfg.RecoverySteps {
		step()
	}
	if mid, ok := floatMid(eng); ok {
		arm.PermanentImpact = signedCost(cfg.Side, mid-arm.ArrivalMid)
	}

	return arm
}

// signedCost orients a raw price displacement so that positive always means the
// price moved against the executor: upward hurts a buyer, downward a seller.
func signedCost(side types.Side, delta float64) float64 {
	if side == types.SideSell {
		return -delta
	}
	return delta
}
