package study

import (
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

const (
	wallUserID      = "absorber"
	aggressorUserID = "aggressor"
)

// SqueezeConfig parameterizes the constructed absorption episode
// (docs/research-roadmap.md §4, experiment 3).
type SqueezeConfig struct {
	Symbol       string
	Seed         int64
	InitialPrice int64 // ticks
	Warmup       int   // steps run to build a book first
	WallQty      int64 // passive bid size placed to absorb
	SellPressure int64 // total aggressive sell lots sent into it
	Reversal     int64 // aggressive buy lots once the selling is done
	Noise        *sim.NoiseTrader
}

func (c *SqueezeConfig) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.InitialPrice == 0 {
		c.InitialPrice = 1000
	}
	if c.Warmup <= 0 {
		c.Warmup = 400
	}
	if c.WallQty <= 0 {
		c.WallQty = 800
	}
	if c.SellPressure <= 0 {
		c.SellPressure = 600
	}
	if c.Reversal <= 0 {
		c.Reversal = 300
	}
	if c.Noise == nil {
		nt := sim.DefaultNoiseTrader("noise")
		nt.MaxOffsetTicks = 40
		c.Noise = nt
	}
}

// SqueezeDemo is the constructed episode's scorecard, in ticks.
//
// The control arm is the whole point. "Price held while selling hit the book"
// means nothing on its own — it could simply have been a quiet market. Only the
// same pressure without the wall establishes what the wall prevented.
type SqueezeDemo struct {
	AbsorbedLots    int64   // lots the wall itself ate
	MoveWithWall    float64 // mid change under the sell pressure, wall present
	MoveWithoutWall float64 // mid change under identical pressure, no wall
	SqueezeMove     float64 // mid change when the flow reverses, wall present
}

// RunSqueezeDemo constructs an absorption episode deliberately: a large passive
// bid absorbs heavy aggressive selling, price holds, then the flow reverses and
// price travels.
//
// This shows the mechanism is real. It says nothing about whether the pattern is
// *tradable* — RunAbsorption asks that separately, and gives a much less
// exciting answer. Both belong in any honest account.
func RunSqueezeDemo(cfg SqueezeConfig) SqueezeDemo {
	cfg.applyDefaults()

	withWall, absorbed, squeeze := squeezeArm(cfg, true)
	withoutWall, _, _ := squeezeArm(cfg, false)

	return SqueezeDemo{
		AbsorbedLots:    absorbed,
		MoveWithWall:    withWall,
		MoveWithoutWall: withoutWall,
		SqueezeMove:     squeeze,
	}
}

// squeezeArm runs one arm and returns the price move under selling pressure, the
// lots the wall absorbed, and the move once the flow reverses.
func squeezeArm(cfg SqueezeConfig, placeWall bool) (moveUnderPressure float64, absorbed int64, squeeze float64) {
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

	// The wall sits one tick above the prevailing best bid so it is first in line
	// for incoming sells — an absorber that queues behind everyone else absorbs
	// nothing.
	if placeWall {
		if bid, _, ok := eng.BestBid(); ok {
			o, err := types.NewOrder(wallUserID, cfg.Symbol, types.SideBuy,
				types.OrderTypeLimit, bid+1, cfg.WallQty, types.TIFGoodTillCancel)
			if err == nil {
				eng.Process(o)
			}
		}
	}

	before, _ := floatMid(eng)

	if o, err := types.NewOrder(aggressorUserID, cfg.Symbol, types.SideSell,
		types.OrderTypeMarket, 0, cfg.SellPressure, types.TIFImmediateOrCancel); err == nil {
		for _, tr := range eng.Process(o).Trades {
			if tr.BuyerUserID == wallUserID {
				absorbed += tr.Quantity
			}
		}
	}

	afterPressure, _ := floatMid(eng)
	moveUnderPressure = afterPressure - before

	// The reversal leg: the selling is spent and aggressive buying arrives — the
	// trapped-seller story, made mechanical.
	if o, err := types.NewOrder(aggressorUserID, cfg.Symbol, types.SideBuy,
		types.OrderTypeMarket, 0, cfg.Reversal, types.TIFImmediateOrCancel); err == nil {
		eng.Process(o)
	}

	afterReversal, _ := floatMid(eng)
	squeeze = afterReversal - afterPressure

	return moveUnderPressure, absorbed, squeeze
}
