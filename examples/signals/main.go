// Signals example: compute book imbalance and order-flow imbalance (OFI) from
// two consecutive book snapshots. Prices are ticks and sizes lots (int64).
//
//	go run ./examples/signals
package main

import (
	"fmt"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/signals"
)

func lvl(price, qty int64) orderbook.SnapshotLevel {
	return orderbook.SnapshotLevel{Price: price, Quantity: qty}
}

func main() {
	prev := &orderbook.Snapshot{
		Bids: []orderbook.SnapshotLevel{lvl(100, 5)},
		Asks: []orderbook.SnapshotLevel{lvl(101, 5)},
	}
	// Bid size grows 5 → 8 (buy pressure); ask unchanged.
	cur := &orderbook.Snapshot{
		Bids: []orderbook.SnapshotLevel{lvl(100, 8)},
		Asks: []orderbook.SnapshotLevel{lvl(101, 5)},
	}

	fmt.Printf("best-level imbalance: %+.2f\n", signals.BestImbalance(cur))
	fmt.Printf("OFI (prev→cur):       %d\n", signals.OFIStep(prev, cur))
}
