// Package strategy implements market-making strategies over the core engine.
//
// The first is the Avellaneda–Stoikov model (2008): given the market mid, your
// current inventory, and the time remaining in the session, it produces an
// inventory-skewed "reservation price" and an optimal bid/ask around it. The
// maker is not predicting direction — it is collecting spread while steering
// inventory back toward flat (docs/research-roadmap.md §3).
//
// Signals here are float64: this is the research layer, distinct from the exact
// decimals the core library uses for money.
package strategy

import (
	"errors"
	"math"
)

// ASParams are the Avellaneda–Stoikov parameters.
type ASParams struct {
	Gamma float64 // risk aversion (γ > 0): higher ⇒ more aggressive inventory skew
	Kappa float64 // order-arrival decay (k > 0): higher ⇒ fills are more spread-sensitive
	Sigma float64 // volatility (σ ≥ 0), per unit of the time scale used for timeRemaining
}

// Validate reports whether the parameters are usable.
func (p ASParams) Validate() error {
	if p.Gamma <= 0 {
		return errors.New("avellaneda-stoikov: gamma must be > 0")
	}
	if p.Kappa <= 0 {
		return errors.New("avellaneda-stoikov: kappa must be > 0")
	}
	if p.Sigma < 0 {
		return errors.New("avellaneda-stoikov: sigma must be >= 0")
	}
	return nil
}

// Quote is a two-sided market-making quote.
type Quote struct {
	Bid         float64
	Ask         float64
	Reservation float64 // inventory-adjusted fair value
	Spread      float64 // total bid↔ask distance
}

// AvellanedaStoikov produces quotes from the AS closed form.
type AvellanedaStoikov struct {
	p ASParams
}

// NewAvellanedaStoikov validates params and returns a quoter.
func NewAvellanedaStoikov(p ASParams) (*AvellanedaStoikov, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &AvellanedaStoikov{p: p}, nil
}

// Params returns the strategy's parameters.
func (a *AvellanedaStoikov) Params() ASParams { return a.p }

// Quote computes the reservation price and optimal bid/ask.
//
//	reservation = mid − inventory·γ·σ²·(T−t)
//	spread      = γ·σ²·(T−t) + (2/γ)·ln(1 + γ/k)
//	bid = reservation − spread/2 ,  ask = reservation + spread/2
//
// timeRemaining is (T−t) in the same time scale σ is expressed in (e.g. a
// fraction of the session in [0,1]). At timeRemaining = 0 the inventory term
// vanishes and the spread collapses to its order-flow floor (2/γ)·ln(1+γ/k).
func (a *AvellanedaStoikov) Quote(mid, inventory, timeRemaining float64) Quote {
	if timeRemaining < 0 {
		timeRemaining = 0
	}
	variance := a.p.Sigma * a.p.Sigma
	riskTerm := a.p.Gamma * variance * timeRemaining
	arrivalTerm := (2.0 / a.p.Gamma) * math.Log(1.0+a.p.Gamma/a.p.Kappa)

	reservation := mid - inventory*riskTerm
	spread := riskTerm + arrivalTerm

	return Quote{
		Bid:         reservation - spread/2.0,
		Ask:         reservation + spread/2.0,
		Reservation: reservation,
		Spread:      spread,
	}
}
