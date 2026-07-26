package signals

import (
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// SignedFlow returns the net signed aggressor volume of a batch of trades,
// following Kyle (1985): buyer-initiated volume counts positive, seller-initiated
// negative.
//
//	y = Σ  +q  when the taker bought
//	       −q  when the taker sold
//
// The aggressor comes from Trade.TakerSide, which the matching engine assigns at
// execution time. Real market data does not publish it — it has to be inferred
// with the Lee-Ready / tick rule, and that inference is noisy (see
// docs/research-roadmap.md §0). Here it is exact, and so is y: quantities are
// integer lots.
func SignedFlow(trades []*types.Trade) int64 {
	var y int64
	for _, t := range trades {
		if t == nil {
			continue
		}
		if t.TakerSide == types.SideBuy {
			y += t.Quantity
		} else {
			y -= t.Quantity
		}
	}
	return y
}

// LambdaFit is a fitted Kyle price-impact relation. Lambda is only interpretable
// alongside R2 and N — a slope fitted through noise is still a slope.
type LambdaFit struct {
	Lambda    float64 // price impact per unit of signed volume (ticks per lot)
	Intercept float64 // fitted price change at zero net flow (ticks)
	R2        float64 // fraction of price-change variance explained by flow
	N         int     // paired intervals in the fit (0 when the input is unusable)
}

// EstimateLambda fits Kyle's price-impact model
//
//	ΔP = λ·y + c
//
// by ordinary least squares over paired per-interval signed order flow (y, in
// lots) and mid-price change (ΔP, in ticks). λ is the slope: the price impact of
// one lot of net aggressive volume, expressed in ticks.
//
// λ is a property of liquidity, not of the asset — a deep book absorbs flow with
// little price movement (small λ), a thin one is shoved around by the same volume
// (large λ). It returns a zero LambdaFit when the inputs are mismatched, shorter
// than two points, or carry no variance.
func EstimateLambda(flow, dPrice []float64) LambdaFit {
	if len(flow) < 2 || len(flow) != len(dPrice) {
		return LambdaFit{}
	}
	slope, intercept, r2 := LinReg(flow, dPrice)
	return LambdaFit{
		Lambda:    slope,
		Intercept: intercept,
		R2:        r2,
		N:         len(flow),
	}
}
