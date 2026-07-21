// Command l2capture polls a public exchange's live level-2 order book and
// computes order-flow imbalance (OFI) and book imbalance in real time using
// pkg/signals — the same code the simulator study uses, now on real market data.
//
// It closes the loop on the research question (docs/research-roadmap.md §1): does
// OFI track price *contemporaneously* on live data too? At the end it regresses
// mid-price change on OFI over the captured window and prints the R².
//
//	go run ./cmd/l2capture                 # BTC-USD, 20 polls, 1s apart
//	go run ./cmd/l2capture -polls 60 -product ETH-USD
//
// Data: Coinbase public REST (no key). Educational; not trading infrastructure.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/signals"
	"github.com/shopspring/decimal"
)

func main() {
	product := flag.String("product", "BTC-USD", "Coinbase product id")
	polls := flag.Int("polls", 20, "number of snapshots to capture")
	interval := flag.Duration("interval", time.Second, "delay between polls")
	depth := flag.Int("depth", 20, "levels per side to keep")
	flag.Parse()

	line := strings.Repeat("─", 74)
	fmt.Println(line)
	fmt.Printf("  Live L2 capture · %s · %d polls @ %s (Coinbase)\n", *product, *polls, *interval)
	fmt.Println(line)
	fmt.Printf("  %-12s %-12s %-9s %-8s %-9s %-10s\n", "best bid", "best ask", "spread", "imbal", "OFI", "cum OFI")

	ofiAcc := signals.NewOFI()
	var prev *orderbook.Snapshot
	var ofiSeries, dMidSeries []float64
	var lastMid float64

	for i := 0; i < *polls; i++ {
		snap, err := fetchBook(*product, *depth)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fetch error:", err)
			time.Sleep(*interval)
			continue
		}

		imb := signals.BestImbalance(snap)
		ofiStep := ofiAcc.Observe(snap)
		mid := midOf(snap)

		bb, ba := "—", "—"
		if len(snap.Bids) > 0 {
			bb = snap.Bids[0].Price.StringFixed(2)
		}
		if len(snap.Asks) > 0 {
			ba = snap.Asks[0].Price.StringFixed(2)
		}
		spread := "—"
		if len(snap.Bids) > 0 && len(snap.Asks) > 0 {
			spread = snap.Asks[0].Price.Sub(snap.Bids[0].Price).StringFixed(2)
		}
		fmt.Printf("  %-12s %-12s %-9s %-8.2f %-9.2f %-10.2f\n",
			bb, ba, spread, imb, ofiStep, ofiAcc.Cumulative())

		if prev != nil {
			ofiSeries = append(ofiSeries, signals.OFIStep(prev, snap).InexactFloat64())
			dMidSeries = append(dMidSeries, mid-lastMid)
		}
		prev, lastMid = snap, mid

		if i < *polls-1 {
			time.Sleep(*interval)
		}
	}

	fmt.Println(line)
	if len(ofiSeries) >= 3 {
		slope, _, r2 := signals.LinReg(ofiSeries, dMidSeries)
		fmt.Printf("  Contemporaneous fit (Δmid on OFI, n=%d): R² = %.3f, slope = %.5f\n",
			len(ofiSeries), r2, slope)
		fmt.Println("  A positive slope means OFI leans the way price moves in the same window.")
		fmt.Println("  This is a *contemporaneous* read on live data — not a forecast.")
	} else {
		fmt.Println("  (need more polls for a regression)")
	}
	fmt.Println(line)
}

// midOf returns the mid price of a snapshot as a float (0 if one-sided).
func midOf(s *orderbook.Snapshot) float64 {
	if len(s.Bids) == 0 || len(s.Asks) == 0 {
		return 0
	}
	return s.Bids[0].Price.Add(s.Asks[0].Price).Div(decimal.NewFromInt(2)).InexactFloat64()
}

// fetchBook pulls a level-2 book from Coinbase and converts it to a Snapshot.
func fetchBook(product string, depth int) (*orderbook.Snapshot, error) {
	url := fmt.Sprintf("https://api.exchange.coinbase.com/products/%s/book?level=2", product)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "orderbook-l2capture (educational)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw struct {
		Bids [][]any `json:"bids"`
		Asks [][]any `json:"asks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	snap := &orderbook.Snapshot{Symbol: product, Timestamp: time.Now().UTC()}
	snap.Bids = toLevels(raw.Bids, depth)
	snap.Asks = toLevels(raw.Asks, depth)
	return snap, nil
}

// toLevels converts Coinbase [price, size, num_orders] rows to snapshot levels.
func toLevels(rows [][]any, depth int) []orderbook.SnapshotLevel {
	out := make([]orderbook.SnapshotLevel, 0, depth)
	for i, r := range rows {
		if i >= depth || len(r) < 2 {
			break
		}
		price, ok1 := r[0].(string)
		size, ok2 := r[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		p, err1 := decimal.NewFromString(price)
		q, err2 := decimal.NewFromString(size)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, orderbook.SnapshotLevel{Price: p, Quantity: q})
	}
	return out
}
