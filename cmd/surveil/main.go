// Command surveil replays a scripted order-flow scenario — a genuine trader, a
// spoofer layering and pulling large orders, and a quote-stuffer — through the
// surveillance monitor and prints the alerts it raises.
//
//	go run ./cmd/surveil
package main

import (
	"fmt"
	"strings"

	"github.com/intrepidkarthi/orderbook/pkg/surveillance"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func main() {
	mon := surveillance.NewMonitor(
		surveillance.NewSpoofDetector(surveillance.SpoofConfig{MinSize: 50, MaxLifetime: 4}),
		surveillance.NewRateLimiter(surveillance.RateConfig{MaxOrders: 5, Window: 8}),
	)

	events := scenario()
	for _, e := range events {
		mon.Observe(e)
	}

	line := strings.Repeat("─", 64)
	fmt.Println(line)
	fmt.Println("  Surveillance replay: who gets flagged, and why")
	fmt.Println(line)
	fmt.Printf("  %d events replayed (genuine trades, a spoofer, a stuffer)\n", len(events))
	fmt.Println(line)
	alerts := mon.Alerts()
	if len(alerts) == 0 {
		fmt.Println("  no alerts")
	}
	for _, a := range alerts {
		fmt.Printf("  seq %-4d  %-10s  %-8s  %s\n", a.Seq, a.Kind, a.UserID, a.Detail)
	}
	fmt.Println(line)
	fmt.Println("  The genuine maker (fills, then cancels) is never flagged.")
	fmt.Println("  Big orders pulled unfilled = spoofing; order bursts = stuffing.")
	fmt.Println(line)
}

func scenario() []surveillance.Event {
	var ev []surveillance.Event
	seq := uint64(0)
	next := func() uint64 { seq++; return seq }

	placed := func(user string, id, size int64) {
		ev = append(ev, surveillance.Event{Kind: surveillance.OrderPlaced, Seq: next(),
			UserID: user, OrderID: id, Side: types.SideBuy, Quantity: size})
	}
	cancelled := func(user string, id int64) {
		ev = append(ev, surveillance.Event{Kind: surveillance.OrderCancelled, Seq: next(), UserID: user, OrderID: id})
	}
	trade := func(makerID, takerID, qty int64) {
		ev = append(ev, surveillance.Event{Kind: surveillance.Trade, Seq: next(),
			MakerOrderID: makerID, TakerOrderID: takerID, Quantity: qty})
	}

	// 1. A genuine market maker: posts, gets filled, then cancels the rest.
	placed("maker", 1, 80)
	trade(1, 99, 80)     // fully filled → legitimate
	cancelled("maker", 1) // already filled; no alert

	// 2. A spoofer layers three large bids and yanks them unfilled.
	placed("spoofer", 2, 100)
	placed("spoofer", 3, 120)
	placed("spoofer", 4, 90)
	cancelled("spoofer", 2)
	cancelled("spoofer", 3)
	cancelled("spoofer", 4)

	// 3. A quote-stuffer fires a burst of small orders.
	for i := range 7 {
		placed("stuffer", int64(100+i), 1)
	}

	return ev
}
