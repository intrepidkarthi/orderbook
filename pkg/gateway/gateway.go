// Package gateway provides the pre-trade edge controls that belong in the layer
// *above* the pure matching core (docs/THREAT-MODEL.md §6): an enforcing
// per-account rate limit that rejects (not merely alerts), an asymmetric speed
// bump on liquidity-taking orders, and cancels that are never gated. The matching
// Engine/Runner stays a neutral, deterministic matcher; these controls need
// identity and wall-clock timing policy, which must not live in a bit-reproducible
// core. Pair a Gateway with a Runner at the network edge.
//
// The gateway is single-writer-friendly but not itself synchronised; front it
// with your own ingress goroutine (or one per connection) and pass a monotonic
// ingress timestamp to Submit.
package gateway

import (
	"errors"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// ErrThrottled is returned by Submit when an order is refused by the rate gate.
var ErrThrottled = errors.New("gateway: account rate limit exceeded")

// RateGate is a per-account token-bucket admission limiter — the enforcing
// counterpart to surveillance.RateLimiter, which only alerts. Each account
// refills at RatePerSec tokens/second up to Burst; a submit with no token is
// denied. It is the quote-stuffing / flood-DoS defence.
type RateGate struct {
	ratePerSec float64
	burst      float64
	buckets    map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateGate builds a rate gate allowing ratePerSec orders/second per account
// with a burst allowance.
func NewRateGate(ratePerSec, burst float64) *RateGate {
	return &RateGate{ratePerSec: ratePerSec, burst: burst, buckets: map[string]*bucket{}}
}

// Allow refills the account's bucket up to now and consumes a token, reporting
// whether the order is admitted.
func (g *RateGate) Allow(user string, now time.Time) bool {
	b := g.buckets[user]
	if b == nil {
		b = &bucket{tokens: g.burst, last: now}
		g.buckets[user] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * g.ratePerSec
	if b.tokens > g.burst {
		b.tokens = g.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Config configures a Gateway.
type Config struct {
	Rate      float64       // orders/sec per account for the rate gate (0 => no rate limiting)
	Burst     float64       // token-bucket burst (defaults to max(Rate, 1) when Rate > 0)
	SpeedBump time.Duration // delay applied to liquidity-taking orders (0 => none)
}

// Gateway wraps a matching.Runner with the edge controls.
type Gateway struct {
	runner *matching.Runner
	gate   *RateGate
	bump   time.Duration
	// OnBump, if set, is called for each liquidity-taking order with the time it
	// would be released after the speed bump. A real gateway holds the order until
	// then so makers can reprice; wire this to your scheduler.
	OnBump func(o *types.Order, releaseAt time.Time)
}

// New builds a gateway over runner.
func New(runner *matching.Runner, cfg Config) *Gateway {
	g := &Gateway{runner: runner, bump: cfg.SpeedBump}
	if cfg.Rate > 0 {
		burst := cfg.Burst
		if burst < 1 {
			burst = cfg.Rate
			if burst < 1 {
				burst = 1
			}
		}
		g.gate = NewRateGate(cfg.Rate, burst)
	}
	return g
}

// Submit runs an order through the rate gate and speed bump, then forwards it to
// the engine. now is the ingress timestamp. A throttled order is rejected here,
// before it reaches the matcher.
func (g *Gateway) Submit(o *types.Order, now time.Time) (*matching.MatchResult, error) {
	if g.gate != nil && !g.gate.Allow(o.UserID, now) {
		return nil, ErrThrottled
	}
	if g.bump > 0 && g.OnBump != nil && g.IsTaker(o) {
		g.OnBump(o, now.Add(g.bump))
	}
	return g.runner.Process(o), nil
}

// Cancel forwards a cancel. It is deliberately never rate-gated: shedding new
// liquidity while keeping cancels flowing is the point of the DoS design.
func (g *Gateway) Cancel(orderID int64, user string) (*types.Order, error) {
	return g.runner.Cancel(orderID, user)
}

// IsTaker reports whether an order would remove liquidity (cross the book) — the
// orders the speed bump targets.
func (g *Gateway) IsTaker(o *types.Order) bool {
	if o.Type == types.OrderTypeMarket {
		return true
	}
	if o.Side == types.SideBuy {
		ask, _, ok := g.runner.BestAsk()
		return ok && o.Price >= ask
	}
	bid, _, ok := g.runner.BestBid()
	return ok && o.Price <= bid
}
