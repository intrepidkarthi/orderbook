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
//
// Prices are integer ticks and sizes integer lots (int64); cash is carried in
// tick·lot units. The strategy interface is float-based (mid, inventory), with
// the float↔tick conversion at the quoting boundary.
package backtest

import (
	"math"
	"math/rand"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/sim"
	"github.com/intrepidkarthi/orderbook/pkg/strategy"
	"github.com/intrepidkarthi/orderbook/pkg/types"
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
	InitialPrice int64 // ticks
	Tick         int64 // quote prices snap to this grid (ticks)
	QuoteSize    int64 // size posted on each side (lots)
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
	if c.InitialPrice == 0 {
		c.InitialPrice = 100
	}
	if c.Tick == 0 {
		c.Tick = 1
	}
	if c.QuoteSize == 0 {
		c.QuoteSize = 2
	}
	if c.MMUserID == "" {
		c.MMUserID = "mm"
	}
	if c.Noise == nil {
		c.Noise = sim.DefaultNoiseTrader("noise")
	}
}

// Result is the outcome and scorecard of a backtest. Inventory/cash/volume are
// integer lots / tick·lot units.
type Result struct {
	Steps           int
	FinalInventory  int64
	FinalCash       int64
	FinalPnL        float64
	Fills           int   // number of maker fills
	Volume          int64 // maker traded volume (lots)
	MaxAbsInventory int64

	Sharpe        float64   // of per-step PnL increments
	PnL           []float64 // mark-to-market PnL per step
	InventoryPath []float64
	MidPath       []float64
}

// Run executes the backtest and returns its scorecard.
func Run(cfg Config) *Result {
	cfg.applyDefaults()
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matching.NewEngine(matching.DefaultConfig(cfg.Symbol))

	var inv, cash, volume, maxAbsInv int64
	fills := 0
	var bidID, askID int64

	res := &Result{Steps: cfg.Steps}

	for step := 0; step < cfg.Steps; step++ {
		mid := refPrice(eng, cfg.InitialPrice)
		timeRemaining := 1.0 - float64(step)/float64(cfg.Steps)

		// 1. Cancel last step's resting quotes (ignore if already filled).
		bidID = cancel(eng, bidID, cfg.MMUserID)
		askID = cancel(eng, askID, cfg.MMUserID)

		// 2. Ask the strategy for a fresh quote.
		q := cfg.Quoter.Quote(float64(mid), float64(inv), timeRemaining)

		// 3. Post the quotes (snapped to the tick grid; skip non-positive prices).
		bidPx := snap(q.Bid, cfg.Tick)
		askPx := snap(q.Ask, cfg.Tick)
		if bidPx > 0 {
			bidID = post(eng, cfg, types.SideBuy, bidPx, &inv, &cash, &fills, &volume)
		}
		if askPx > 0 && askPx > bidPx {
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
		if abs64(inv) > maxAbsInv {
			maxAbsInv = abs64(inv)
		}
		markMid := refPrice(eng, cfg.InitialPrice)
		pnl := float64(cash) + float64(inv)*float64(markMid)
		res.PnL = append(res.PnL, pnl)
		res.InventoryPath = append(res.InventoryPath, float64(inv))
		res.MidPath = append(res.MidPath, float64(markMid))
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
// the resting order id (0 if it filled fully).
func post(eng *matching.Engine, cfg Config, side types.Side, price int64,
	inv, cash *int64, fills *int, volume *int64) int64 {
	o, err := types.NewOrder(cfg.MMUserID, cfg.Symbol, side, types.OrderTypeLimit,
		price, cfg.QuoteSize, types.TIFGoodTillCancel)
	if err != nil {
		return 0
	}
	r := eng.Process(o)
	account(r.Trades, cfg.MMUserID, inv, cash, fills, volume)
	if o.IsFilled() {
		return 0
	}
	return o.ID
}

// account folds any trades the maker took part in into inventory and cash.
func account(trades []*types.Trade, mm string, inv, cash *int64, fills *int, volume *int64) {
	for _, t := range trades {
		notional := t.Price * t.Quantity
		if t.BuyerUserID == mm {
			*inv += t.Quantity
			*cash -= notional
			*fills++
			*volume += t.Quantity
		}
		if t.SellerUserID == mm {
			*inv -= t.Quantity
			*cash += notional
			*fills++
			*volume += t.Quantity
		}
	}
}

func cancel(eng *matching.Engine, id int64, user string) int64 {
	if id != 0 {
		_, _ = eng.Cancel(id, user)
	}
	return 0
}

func refPrice(eng *matching.Engine, initial int64) int64 {
	if mid, ok := eng.MidPrice(); ok {
		return mid
	}
	if ltp := eng.LastTradePrice(); ltp > 0 {
		return ltp
	}
	return initial
}

// snap rounds a float price onto the integer tick grid.
func snap(px float64, tick int64) int64 {
	if tick <= 0 {
		tick = 1
	}
	return int64(math.Round(px/float64(tick))) * tick
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
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
