// Command gateway shows the pkg/gateway edge controls that belong in the layer
// *above* the pure matching core (docs/THREAT-MODEL.md §6): an enforcing
// per-account rate gate that rejects (not just alerts), an asymmetric speed bump
// on liquidity-taking orders, and a CAT-style audit export off the engine's
// sequenced event stream. The core stays a neutral, deterministic matcher.
//
//	go run ./examples/gateway
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// auditSink is an EventSink writing one immutable audit record per sequenced
// engine event — the Rule 613 (CAT) post-trade trail off the event spine.
type auditSink struct{ w io.Writer }

func (a *auditSink) OnEvents(evs []matching.Event) {
	for _, e := range evs {
		fmt.Fprintf(a.w, "  AUDIT seq=%-3d %-9s order=%-2d user=%s\n", e.Seq, e.Kind, e.OrderID, e.UserID)
	}
}

func main() {
	eng := matching.Config{Symbol: "BTC-USD", EventSink: &auditSink{w: os.Stdout}}
	runner := matching.NewRunner(matching.RunnerConfig{Engine: eng})
	defer runner.Close()

	gw := gateway.New(runner, gateway.Config{
		Rate:      2, // 2 orders/sec per account
		Burst:     3,
		SpeedBump: 350 * time.Microsecond,
	})
	gw.OnBump = func(o *types.Order, releaseAt time.Time) {
		fmt.Printf("  BUMP      taker  %-6s released at %v\n", o.UserID, releaseAt.UTC())
	}

	now := time.Unix(0, 0).UTC()
	lim := func(user string, side types.Side, price, qty int64) *types.Order {
		o, _ := types.NewOrder(user, "BTC-USD", side, types.OrderTypeLimit, price, qty, types.TIFGoodTillCancel)
		return o
	}

	fmt.Println("1) A maker rests an ask; a marketable buy is speed-bumped, then trades:")
	_, _ = gw.Submit(lim("mm", types.SideSell, 100, 5), now)
	_, _ = gw.Submit(lim("taker", types.SideBuy, 100, 2), now) // crosses → taker → bumped

	fmt.Println("\n2) One account floods orders; the gate throttles past its burst:")
	for i := range 5 {
		if _, err := gw.Submit(lim("flooder", types.SideBuy, int64(90+i), 1), now); err != nil {
			fmt.Printf("  GATE      reject flooder (%v)\n", err)
		}
	}

	fmt.Println("\n   ...but a cancel is never gated (keep-cancels-flowing):")
	if _, err := gw.Cancel(3, "flooder"); err == nil { // order 3 = flooder's first resting order
		fmt.Println("  OK        flooder cancel accepted despite the throttle")
	}

	fmt.Println("\n3) Tokens refill over time; a second later the account can post again:")
	now = now.Add(time.Second)
	if _, err := gw.Submit(lim("flooder", types.SideBuy, 95, 1), now); err == nil {
		fmt.Println("  OK        flooder accepted after refill")
	}

	fmt.Println("\nThe audit trail above is the CAT-style export off the sequenced event stream.")
}
