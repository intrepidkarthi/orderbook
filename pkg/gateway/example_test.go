package gateway_test

import (
	"fmt"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/gateway"
)

// ExampleRateGate shows the enforcing token-bucket admission limiter: a burst is
// admitted, then further orders in the same instant are throttled.
func ExampleRateGate() {
	g := gateway.NewRateGate(2, 2) // 2 orders/sec per account, burst 2
	now := time.Unix(0, 0).UTC()

	for i := 1; i <= 3; i++ {
		fmt.Printf("order %d admitted: %v\n", i, g.Allow("acct-1", now))
	}
	// A different account has its own bucket.
	fmt.Printf("other account:  %v\n", g.Allow("acct-2", now))

	// Output:
	// order 1 admitted: true
	// order 2 admitted: true
	// order 3 admitted: false
	// other account:  true
}
