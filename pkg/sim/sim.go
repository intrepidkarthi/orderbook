// Package sim is a deterministic exchange simulator. It drives synthetic agents
// against a real matching engine so strategies and signals can be studied with
// controllable order flow and known ground truth (docs/SPEC.md §9).
//
// Determinism is the point: a Config with a fixed Seed reproduces the same
// trades, book, and price path on every run, which is what makes simulated
// experiments and backtests trustworthy.
//
// Prices are integer ticks and quantities integer lots (int64), matching the
// engine's representation.
package sim

import (
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// View is the market state handed to an agent each step.
type View struct {
	Symbol   string
	Step     int
	Snapshot *orderbook.Snapshot
	Ref      int64 // reference price (ticks): mid, else last trade, else initial
	HasBook  bool  // true if any liquidity is resting
}

// Agent produces orders to submit given the current market view. Implementations
// must draw all randomness from the provided *rand.Rand so runs stay
// deterministic.
type Agent interface {
	Act(v View, rng *rand.Rand) []*types.Order
}

// Config parameterizes a simulation.
type Config struct {
	Symbol       string
	Steps        int
	Seed         int64
	InitialPrice int64 // ticks
	Depth        int   // snapshot depth exposed to agents
	Agents       []Agent
}

func (c *Config) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.Steps <= 0 {
		c.Steps = 1000
	}
	if c.InitialPrice == 0 {
		c.InitialPrice = 100
	}
	if c.Depth <= 0 {
		c.Depth = 10
	}
	if len(c.Agents) == 0 {
		c.Agents = []Agent{DefaultNoiseTrader("noise")}
	}
}

// Result is the outcome of a simulation.
type Result struct {
	Engine *matching.Engine
	Trades []*types.Trade
	Prices []int64 // reference price per step (ticks)
	Final  *orderbook.Snapshot
}

// Run executes the simulation and returns its result.
func Run(cfg Config) *Result {
	cfg.applyDefaults()
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))
	res := &Result{Engine: eng}

	ref := cfg.InitialPrice
	for step := 0; step < cfg.Steps; step++ {
		if mid, ok := eng.MidPrice(); ok {
			ref = mid
		} else if ltp := eng.LastTradePrice(); ltp > 0 {
			ref = ltp
		}

		view := View{
			Symbol:   cfg.Symbol,
			Step:     step,
			Snapshot: eng.Snapshot(cfg.Depth),
			Ref:      ref,
			HasBook:  eng.OrderCount() > 0,
		}

		for _, ag := range cfg.Agents {
			for _, o := range ag.Act(view, rng) {
				r := eng.Process(o)
				res.Trades = append(res.Trades, r.Trades...)
			}
		}
		res.Prices = append(res.Prices, ref)
	}

	res.Final = eng.Snapshot(cfg.Depth)
	return res
}
