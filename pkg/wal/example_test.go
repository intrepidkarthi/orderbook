package wal_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Example shows the write-ahead recovery contract: commands are logged before the
// engine applies them, and a fresh engine replaying the log rebuilds the same book.
func Example() {
	dir, _ := os.MkdirTemp("", "wal-example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "orders.wal")

	// Live: log write-ahead, then apply to the engine.
	w, _ := wal.Open(path)
	live := matching.NewEngine(matching.DefaultConfig("BTC-USD"))
	ask, _ := types.NewOrder("mm", "BTC-USD", types.SideSell, types.OrderTypeLimit, 100, 5, types.TIFGoodTillCancel)
	_, _ = w.AppendSubmit(ask)
	_ = w.Sync() // durability point (group-commit here)
	live.Process(ask)
	_ = w.Close()

	// Recover: replay the log into a fresh engine.
	entries, _ := wal.ReadAll(path)
	recovered := matching.NewEngine(matching.DefaultConfig("BTC-USD"))
	wal.Restore(recovered, entries)

	fmt.Printf("logged %d command(s); recovered book has %d resting order(s)\n",
		len(entries), recovered.OrderCount())
	// Output: logged 1 command(s); recovered book has 1 resting order(s)
}
