package sim

import (
	"math/rand"
	"strconv"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// NoiseTrader is an uninformed agent representing a *population* of participants:
// each step it may submit a random buy or sell, usually a passive limit near the
// reference price, occasionally a market order that crosses the book. It supplies
// the "uninformed flow" a market maker wants to trade against
// (docs/research-roadmap.md §3).
//
// Orders are issued under distinct trader identities (UserID-N for N in
// [0,Population)) so that aggressive orders cross the resting book instead of
// being cancelled by self-trade prevention. Occasional same-identity collisions
// are realistic and simply get STP-cancelled.
type NoiseTrader struct {
	UserID         string
	Population     int             // distinct trader identities (default 50)
	ActProb        float64         // probability of acting on a given step
	MarketFraction float64         // fraction of actions that are market orders
	MaxOffsetTicks int             // passive limits sit 0..N ticks off the ref
	Tick           decimal.Decimal // price increment
	MinSize        int             // order size drawn uniformly in [MinSize,MaxSize]
	MaxSize        int
}

// DefaultNoiseTrader returns a NoiseTrader with reasonable defaults.
func DefaultNoiseTrader(userID string) *NoiseTrader {
	return &NoiseTrader{
		UserID:         userID,
		Population:     50,
		ActProb:        0.9,
		MarketFraction: 0.3,
		MaxOffsetTicks: 5,
		Tick:           decimal.NewFromInt(1),
		MinSize:        1,
		MaxSize:        10,
	}
}

// Act implements Agent.
func (nt *NoiseTrader) Act(v View, rng *rand.Rand) []*types.Order {
	if rng.Float64() >= nt.ActProb {
		return nil
	}

	// A distinct trader identity from the population, so crossing orders are not
	// swallowed by self-trade prevention.
	user := nt.UserID + "-" + strconv.Itoa(rng.Intn(nt.population()))

	side := types.SideBuy
	if rng.Float64() < 0.5 {
		side = types.SideSell
	}
	size := decimal.NewFromInt(int64(nt.MinSize + rng.Intn(nt.maxMinusMin()+1)))

	// A market order (only worthwhile if there's liquidity to take).
	if v.HasBook && rng.Float64() < nt.MarketFraction {
		o, err := types.NewOrder(user, v.Symbol, side, types.OrderTypeMarket,
			decimal.Zero, size, types.TIFImmediateOrCancel)
		if err != nil {
			return nil
		}
		return []*types.Order{o}
	}

	// A passive limit sitting offset ticks away from the reference price.
	offset := nt.Tick.Mul(decimal.NewFromInt(int64(rng.Intn(nt.MaxOffsetTicks + 1))))
	var price decimal.Decimal
	if side == types.SideBuy {
		price = v.Ref.Sub(offset)
	} else {
		price = v.Ref.Add(offset)
	}
	if price.LessThanOrEqual(decimal.Zero) {
		price = nt.Tick
	}

	o, err := types.NewOrder(user, v.Symbol, side, types.OrderTypeLimit,
		price, size, types.TIFGoodTillCancel)
	if err != nil {
		return nil
	}
	return []*types.Order{o}
}

func (nt *NoiseTrader) maxMinusMin() int {
	if nt.MaxSize <= nt.MinSize {
		return 0
	}
	return nt.MaxSize - nt.MinSize
}

func (nt *NoiseTrader) population() int {
	if nt.Population <= 0 {
		return 50
	}
	return nt.Population
}
