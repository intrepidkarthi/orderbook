// Command gateway shows the defences that belong in the layer *above* the pure
// matching core, not inside it — the boundary docs/THREAT-MODEL.md §6 draws. The
// engine stays a neutral, deterministic matcher; this gateway wraps it with the
// pre-trade and audit controls a real venue runs at the edge:
//
//   - an enforcing admission gate: a per-account token-bucket rate limit that
//     *rejects* an over-quota order (not just alerts), while always letting
//     cancels through — the quote-stuffing / flood-DoS defence (THREAT-MODEL #2);
//   - an asymmetric speed bump on liquidity-*taking* orders, so makers can reprice
//     against the same information — the latency-arbitrage defence (IEX, #16);
//   - a CAT-style audit export off the engine's sequenced event stream — the
//     post-trade audit trail (Rule 613, #18).
//
// These are deliberately outside the core: they need identity, wall-clock timing
// policy, and I/O, none of which belong in a bit-reproducible matcher.
//
//	go run ./examples/gateway
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// ErrThrottled is returned when an order is refused by the admission gate.
var ErrThrottled = errors.New("gateway: account rate limit exceeded")

// --- Enforcing admission gate: a per-account token bucket ---

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

// rateGate rejects new orders from an account faster than ratePerSec (with a
// burst allowance). It is the *enforcing* counterpart to surveillance.RateLimiter,
// which only alerts.
type rateGate struct {
	ratePerSec float64
	burst      float64
	buckets    map[string]*bucket
}

func newRateGate(ratePerSec, burst float64) *rateGate {
	return &rateGate{ratePerSec: ratePerSec, burst: burst, buckets: map[string]*bucket{}}
}

func (g *rateGate) allow(user string, now time.Time) bool {
	b := g.buckets[user]
	if b == nil {
		b = &bucket{tokens: g.burst, lastRefill: now}
		g.buckets[user] = b
	}
	b.tokens += now.Sub(b.lastRefill).Seconds() * g.ratePerSec
	if b.tokens > g.burst {
		b.tokens = g.burst
	}
	b.lastRefill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// --- CAT-style audit sink: one immutable record per sequenced event ---

type auditSink struct{ w io.Writer }

func (a *auditSink) OnEvents(evs []matching.Event) {
	for _, e := range evs {
		fmt.Fprintf(a.w, "  AUDIT seq=%-3d %-9s order=%-2d user=%s\n", e.Seq, e.Kind, e.OrderID, e.UserID)
	}
}

// --- Gateway: wraps the pure Runner with the edge controls ---

type gateway struct {
	runner *matching.Runner
	gate   *rateGate
	bump   time.Duration
}

// Submit runs an order through the admission gate and speed bump, then forwards it
// to the engine. now is the ingress timestamp (a real gateway reads the clock).
func (gw *gateway) submit(o *types.Order, now time.Time) (*matching.MatchResult, error) {
	if !gw.gate.allow(o.UserID, now) {
		fmt.Printf("  GATE      reject %-6s (rate limit)\n", o.UserID)
		return nil, ErrThrottled
	}
	if gw.isTaker(o) {
		// A real gateway holds the taker until now+bump; here we report the delay
		// the maker would get to reprice within.
		fmt.Printf("  BUMP      taker  %-6s delayed %v\n", o.UserID, gw.bump)
	}
	return gw.runner.Process(o), nil
}

// cancel is never rate-gated — shedding new liquidity while keeping cancels
// flowing is the whole point of the backpressure/DoS design.
func (gw *gateway) cancel(orderID int64, user string) {
	_, _ = gw.runner.Cancel(orderID, user)
}

// isTaker reports whether an order would remove liquidity (cross the book).
func (gw *gateway) isTaker(o *types.Order) bool {
	if o.Type == types.OrderTypeMarket {
		return true
	}
	if o.Side == types.SideBuy {
		ask, _, ok := gw.runner.BestAsk()
		return ok && o.Price >= ask
	}
	bid, _, ok := gw.runner.BestBid()
	return ok && o.Price <= bid
}

func main() {
	eng := matching.Config{Symbol: "BTC-USD", EventSink: &auditSink{w: os.Stdout}}
	runner := matching.NewRunner(matching.RunnerConfig{Engine: eng})
	defer runner.Close()

	gw := &gateway{
		runner: runner,
		gate:   newRateGate(2, 3), // 2 orders/sec, burst 3
		bump:   350 * time.Microsecond,
	}

	now := time.Unix(0, 0).UTC()
	lim := func(user string, side types.Side, price, qty int64) *types.Order {
		o, _ := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		return o
	}

	fmt.Println("1) A maker rests an ask; a marketable buy is speed-bumped, then trades:")
	_, _ = gw.submit(lim("mm", types.SideSell, 100, 5), now)
	_, _ = gw.submit(lim("taker", types.SideBuy, 100, 2), now) // crosses → taker → bumped

	fmt.Println("\n2) One account floods orders; the gate throttles past its burst:")
	flood := "flooder"
	for i := range 5 {
		// All within the same instant, so only the burst (3) passes; the rest are
		// rejected at the gate before ever reaching the engine.
		_, err := gw.submit(lim(flood, types.SideBuy, int64(90+i), 1), now)
		if err != nil {
			continue
		}
	}

	fmt.Println("\n   ...but a cancel is never gated (keep-cancels-flowing):")
	gw.cancel(3, flood) // order 3 was the flooder's first accepted (resting) order
	fmt.Println("  OK        flooder cancel accepted despite the throttle")

	fmt.Println("\n3) Tokens refill over time; a second later the account can post again:")
	now = now.Add(time.Second)
	if _, err := gw.submit(lim(flood, types.SideBuy, 95, 1), now); err == nil {
		fmt.Println("  OK        flooder accepted after refill")
	}

	fmt.Println("\nThe audit trail above is the CAT-style export off the sequenced event stream.")
}
