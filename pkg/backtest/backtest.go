// Package backtest runs a market-making strategy against synthetic order flow in
// a real matching engine and reports its performance.
//
// Each step the maker cancels its resting quotes, asks its Quoter for a fresh
// bid/ask given the current mid, inventory, and time remaining, and posts them;
// then a background NoiseTrader supplies flow that may lift or hit those quotes.
// The harness tracks the maker's inventory and cash from every trade it takes
// part in (as maker or taker) and marks the book to compute PnL — the honest
// scorecard the research plan calls for (docs/research-roadmap.md §3): inventory
// path, PnL, and Sharpe, not just "it quotes".
package backtest

import (
	"math"
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
	"github.com/intrepidkarthi/orderbook/pkg/strategy"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// Quoter is a two-sided market-making strategy (e.g. strategy.AvellanedaStoikov).
type Quoter interface {
	Quote(mid, inventory, timeRemaining float64) strategy.Quote
}

// Config parameterizes a market-making backtest.
type Config struct {
	Symbol       string
	Steps        int
	Seed         int64
	InitialPrice decimal.Decimal
	Tick         decimal.Decimal // quote prices snap to this grid
	QuoteSize    decimal.Decimal // size posted on each side
	MMUserID     string
	Quoter       Quoter           // required
	Noise        *sim.NoiseTrader // background flow (default provided)
}

func (c *Config) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "SIM"
	}
	if c.Steps <= 0 {
		c.Steps = 3000
	}
	if c.InitialPrice.IsZero() {
		c.InitialPrice = decimal.NewFromInt(100)
	}
	if c.Tick.IsZero() {
		c.Tick = decimal.NewFromFloat(0.1)
	}
	if c.QuoteSize.IsZero() {
		c.QuoteSize = decimal.NewFromInt(2)
	}
	if c.MMUserID == "" {
		c.MMUserID = "mm"
	}
	if c.Noise == nil {
		c.Noise = sim.DefaultNoiseTrader("noise")
	}
}

// Result is the outcome and scorecard of a backtest.
type Result struct {
	Steps           int
	FinalInventory  decimal.Decimal
	FinalCash       decimal.Decimal
	FinalPnL        float64
	Fills           int             // number of maker fills
	Volume          decimal.Decimal // maker traded volume
	MaxAbsInventory decimal.Decimal
	Sharpe          float64 // of per-step PnL increments

	PnL           []float64 // mark-to-market PnL per step
	InventoryPath []float64
	MidPath       []float64
}

// Run executes the backtest and returns its scorecard.
func Run(cfg Config) *Result {
	cfg.applyDefaults()
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))

	inv := decimal.Zero
	cash := decimal.Zero
	fills := 0
	volume := decimal.Zero
	maxAbsInv := decimal.Zero
	var bidID, askID string

	res := &Result{Steps: cfg.Steps}

	for step := 0; step < cfg.Steps; step++ {
		mid := refPrice(eng, cfg.InitialPrice)
		timeRemaining := 1.0 - float64(step)/float64(cfg.Steps)

		// 1. Cancel last step's resting quotes (ignore if already filled).
		bidID = cancel(eng, bidID, cfg.MMUserID)
		askID = cancel(eng, askID, cfg.MMUserID)

		// 2. Ask the strategy for a fresh quote.
		q := cfg.Quoter.Quote(mid.InexactFloat64(), inv.InexactFloat64(), timeRemaining)

		// 3. Post the quotes (snapped to the tick grid; skip non-positive prices).
		bidPx := snap(q.Bid, cfg.Tick)
		askPx := snap(q.Ask, cfg.Tick)
		if bidPx.IsPositive() {
			bidID = post(eng, cfg, types.SideBuy, bidPx, &inv, &cash, &fills, &volume)
		}
		if askPx.IsPositive() && askPx.GreaterThan(bidPx) {
			askID = post(eng, cfg, types.SideSell, askPx, &inv, &cash, &fills, &volume)
		}

		// 4. Background noise flow (may hit the maker's resting quotes).
		view := sim.View{
			Symbol:   cfg.Symbol,
			Step:     step,
			Snapshot: eng.Snapshot(10),
			Ref:      mid,
			HasBook:  eng.OrderCount() > 0,
		}
		for _, o := range cfg.Noise.Act(view, rng) {
			r := eng.Process(o)
			account(r.Trades, cfg.MMUserID, &inv, &cash, &fills, &volume)
		}

		// 5. Mark to market and record.
		if inv.Abs().GreaterThan(maxAbsInv) {
			maxAbsInv = inv.Abs()
		}
		markMid := refPrice(eng, cfg.InitialPrice)
		pnl := cash.Add(inv.Mul(markMid)).InexactFloat64()
		res.PnL = append(res.PnL, pnl)
		res.InventoryPath = append(res.InventoryPath, inv.InexactFloat64())
		res.MidPath = append(res.MidPath, markMid.InexactFloat64())
	}

	res.FinalInventory = inv
	res.FinalCash = cash
	res.Fills = fills
	res.Volume = volume
	res.MaxAbsInventory = maxAbsInv
	if n := len(res.PnL); n > 0 {
		res.FinalPnL = res.PnL[n-1]
	}
	res.Sharpe = sharpe(res.PnL)
	return res
}

// post submits a maker limit order, accounts any immediate fills, and returns
// the resting order id ("" if it filled fully).
func post(eng *matching.Engine, cfg Config, side types.Side, price decimal.Decimal,
	inv, cash *decimal.Decimal, fills *int, volume *decimal.Decimal) string {
	o, err := types.NewOrder(cfg.MMUserID, cfg.Symbol, side, types.OrderTypeLimit,
		price, cfg.QuoteSize, types.TIFGoodTillCancel)
	if err != nil {
		return ""
	}
	r := eng.Process(o)
	account(r.Trades, cfg.MMUserID, inv, cash, fills, volume)
	if o.IsFilled() {
		return ""
	}
	return o.ID
}

// account folds any trades the maker took part in into inventory and cash.
func account(trades []*types.Trade, mm string, inv, cash *decimal.Decimal, fills *int, volume *decimal.Decimal) {
	for _, t := range trades {
		notional := t.Price.Mul(t.Quantity)
		if t.BuyerUserID == mm {
			*inv = inv.Add(t.Quantity)
			*cash = cash.Sub(notional)
			*fills++
			*volume = volume.Add(t.Quantity)
		}
		if t.SellerUserID == mm {
			*inv = inv.Sub(t.Quantity)
			*cash = cash.Add(notional)
			*fills++
			*volume = volume.Add(t.Quantity)
		}
	}
}

func cancel(eng *matching.Engine, id, user string) string {
	if id != "" {
		_, _ = eng.Cancel(id, user)
	}
	return ""
}

func refPrice(eng *matching.Engine, initial decimal.Decimal) decimal.Decimal {
	if mid, ok := eng.MidPrice(); ok {
		return mid
	}
	if ltp := eng.LastTradePrice(); ltp.IsPositive() {
		return ltp
	}
	return initial
}

// snap rounds a float price onto the tick grid.
func snap(px float64, tick decimal.Decimal) decimal.Decimal {
	if tick.IsZero() {
		return decimal.NewFromFloat(px)
	}
	return decimal.NewFromFloat(px).Div(tick).Round(0).Mul(tick)
}

// sharpe is mean/stddev of per-step PnL increments, scaled by sqrt(N).
func sharpe(pnl []float64) float64 {
	if len(pnl) < 3 {
		return 0
	}
	diffs := make([]float64, 0, len(pnl)-1)
	for i := 1; i < len(pnl); i++ {
		diffs = append(diffs, pnl[i]-pnl[i-1])
	}
	var sum float64
	for _, d := range diffs {
		sum += d
	}
	mean := sum / float64(len(diffs))
	var varSum float64
	for _, d := range diffs {
		varSum += (d - mean) * (d - mean)
	}
	std := math.Sqrt(varSum / float64(len(diffs)))
	if std == 0 {
		return 0
	}
	return mean / std * math.Sqrt(float64(len(diffs)))
}
