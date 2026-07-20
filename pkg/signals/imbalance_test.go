package signals

import (
	"math"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// snap builds a snapshot from {price, qty} pairs (best-first, as the book emits).
func snap(bids, asks [][2]string) *orderbook.Snapshot {
	s := &orderbook.Snapshot{}
	for _, b := range bids {
		s.Bids = append(s.Bids, orderbook.SnapshotLevel{Price: dec(b[0]), Quantity: dec(b[1])})
	}
	for _, a := range asks {
		s.Asks = append(s.Asks, orderbook.SnapshotLevel{Price: dec(a[0]), Quantity: dec(a[1])})
	}
	return s
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBestImbalance_Symmetric(t *testing.T) {
	s := snap([][2]string{{"100", "5"}}, [][2]string{{"101", "5"}})
	if got := BestImbalance(s); !approx(got, 0) {
		t.Errorf("symmetric imbalance = %v, want 0", got)
	}
}

func TestBestImbalance_Signs(t *testing.T) {
	bidHeavy := snap([][2]string{{"100", "8"}}, [][2]string{{"101", "2"}})
	if got := BestImbalance(bidHeavy); !approx(got, 0.6) {
		t.Errorf("bid-heavy imbalance = %v, want 0.6", got)
	}
	askHeavy := snap([][2]string{{"100", "2"}}, [][2]string{{"101", "8"}})
	if got := BestImbalance(askHeavy); !approx(got, -0.6) {
		t.Errorf("ask-heavy imbalance = %v, want -0.6", got)
	}
}

func TestDepthImbalance_UsesTopN(t *testing.T) {
	// Best level is balanced; deeper levels are bid-heavy.
	s := snap(
		[][2]string{{"100", "1"}, {"99", "9"}},
		[][2]string{{"101", "1"}, {"102", "1"}},
	)
	if got := DepthImbalance(s, 1); !approx(got, 0) {
		t.Errorf("best-level imbalance = %v, want 0", got)
	}
	// Depth 2: (1+9 − 1−1)/(1+9+1+1) = 8/12 ≈ 0.6667
	if got := DepthImbalance(s, 2); !approx(got, 8.0/12.0) {
		t.Errorf("depth-2 imbalance = %v, want %v", got, 8.0/12.0)
	}
}

func TestImbalance_Degenerate(t *testing.T) {
	if got := DepthImbalance(nil, 3); got != 0 {
		t.Errorf("nil snapshot = %v, want 0", got)
	}
	empty := snap(nil, nil)
	if got := BestImbalance(empty); got != 0 {
		t.Errorf("empty book = %v, want 0", got)
	}
	// A one-sided book is a meaningful extreme, not "no signal": all bid → +1,
	// all ask → -1.
	if got := BestImbalance(snap([][2]string{{"100", "5"}}, nil)); !approx(got, 1) {
		t.Errorf("bids-only book = %v, want +1", got)
	}
	if got := BestImbalance(snap(nil, [][2]string{{"101", "5"}})); !approx(got, -1) {
		t.Errorf("asks-only book = %v, want -1", got)
	}
	if got := DepthImbalance(empty, 0); got != 0 {
		t.Errorf("zero levels = %v, want 0", got)
	}
}
