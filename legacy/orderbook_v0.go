//go:build ignore

// orderbook_v0.go — the original float64 prototype, frozen for posterity.
//
// This is the "before" that the rest of this repo improves upon: floats for
// money, map[float64] price keys, a no-op per-level sort, and matching that
// only crosses when a bid price exactly equals an ask price. It is excluded
// from the build by the //go:build ignore tag above and kept purely as a
// historical reference point.
//
// See docs/SPEC.md for the design that replaces it.
package main

import (
	"fmt"
	"sync"
	"time"
	"math/rand"
	"sort"
	"math"
)

// MarketDetails represents additional details for each market
type MarketDetails struct {
	ContractSymbol  string  // Symbol of the contract
	Precision       int     // Precision for the market
	ContractName    string  // Name of the contract
	Price           float64 // Current price of the contract
	AskDelta        float64 // Ask delta for the market
	BidDelta        float64 // Bid delta for the market
	ConversionRate  float64 // Conversion rate for the market
	CurrencySymbol  string  // Symbol of the currency
}

// Limit represents the limit details of an order
type Limit struct {
	Price float64 // Price at which the limit is set
}

// Order represents a single order
type Order struct {
	ID        uint64  // The ID concept is only for external APIs
	UserID    uint64  // UserID to identify who puts the order
	Amount    float64 // Amount of our crypto
	Bid       bool    // Is this a sell or buy Order
	Limit     *Limit  // To keep track of what limit this order is set in
	Timestamp int64   // Use in64 because we will use Unix nano for Timestamp
}



// MarketOrderBook represents the order book for a single market
type MarketOrderBook struct {
	Details MarketDetails    
	bids     map[float64]OrderSlice // TreeMap for buy orders
	asks     map[float64]OrderSlice // TreeMap for sell orders
}

// OrderBook represents the order book for different markets
type OrderBook struct {
	MarketOrderBooks map[string]*MarketOrderBook // Map of market identifier to order book
	mutex            sync.RWMutex                // Mutex for thread-safe access
}



// OrderSlice is a slice of Order pointers
type OrderSlice []*Order

// Implementing the sort.Interface for OrderSlice
func (os OrderSlice) Len() int           { return len(os) }
func (os OrderSlice) Less(i, j int) bool { return os[i].Limit.Price < os[j].Limit.Price }
func (os OrderSlice) Swap(i, j int)      { os[i], os[j] = os[j], os[i] }

// NewOrderBook initializes a new order book
func NewOrderBook() *OrderBook {
	return &OrderBook{
		MarketOrderBooks: make(map[string]*MarketOrderBook),
	}
}

// NewMarketOrderBook initializes a new market order book
func NewMarketOrderBook(details MarketDetails) *MarketOrderBook {
    return &MarketOrderBook{
        Details: details,
        bids:    make(map[float64]OrderSlice),
        asks:    make(map[float64]OrderSlice),
    }
}



// AddOrder adds an order to the order book
func (ob *OrderBook) AddOrder(marketID string, order Order, details MarketDetails) {
	ob.mutex.Lock()
	defer ob.mutex.Unlock()

	marketOrderBook, ok := ob.MarketOrderBooks[marketID]
    if !ok {
        // If the market doesn't exist, create a new MarketOrderBook
        marketOrderBook = NewMarketOrderBook(details)
        ob.MarketOrderBooks[marketID] = marketOrderBook
    }

	var orders map[float64]OrderSlice

    if order.Bid {
        orders = marketOrderBook.bids
    } else {
        orders = marketOrderBook.asks
    }

    if _, ok := orders[order.Limit.Price]; !ok {
        orders[order.Limit.Price] = make(OrderSlice, 0)
    }

    orders[order.Limit.Price] = append(orders[order.Limit.Price], &order)

    // Sort the orders at the given price level
    sort.Slice(orders[order.Limit.Price], func(i, j int) bool {
        if order.Bid {
            return orders[order.Limit.Price][i].Limit.Price > orders[order.Limit.Price][j].Limit.Price
        }
        return orders[order.Limit.Price][i].Limit.Price < orders[order.Limit.Price][j].Limit.Price
    })
}



// CancelOrder cancels an order from the order book
func (ob *OrderBook) CancelOrder(marketID string, orderID uint64, bid bool) {
    ob.mutex.Lock()
    defer ob.mutex.Unlock()

    marketOrderBook, ok := ob.MarketOrderBooks[marketID]
    if !ok {
        fmt.Printf("Market %s not found in the order book.\n", marketID)
        return
    }

    // Get the orders corresponding to the specified side (bid/ask)
    var orders map[float64]OrderSlice
    if bid {
        orders = marketOrderBook.bids
    } else {
        orders = marketOrderBook.asks
    }

    // Iterate through the orders to find and remove the order with the specified ID
    for price, orderSlice := range orders {
        for i, order := range orderSlice {
            if order.ID == orderID {
                // Remove the order from the order slice
                orders[price] = append(orderSlice[:i], orderSlice[i+1:]...)
                // If the order slice becomes empty after removal, delete the price level
                if len(orders[price]) == 0 {
                    delete(orders, price)
                }
                fmt.Printf("Order %d canceled.\n", orderID)
                return
            }
        }
    }

    // If the order is not found, print a message
    fmt.Printf("Order %d not found.\n", orderID)
}







// MeasureAddPerformance measures the performance of adding random orders to the order book
func (ob *OrderBook) MeasureAddPerformance(marketID string, numOrders int, details MarketDetails) time.Duration {
	start := time.Now()

	for i := 0; i < numOrders; i++ {
		order := generateRandomOrder(i, details)
		ob.AddOrder(marketID, order, details)
	}

	elapsed := time.Since(start)
	return elapsed
}

// generateRandomOrder generates a random order with given market details
func generateRandomOrder(i int, details MarketDetails) Order {
	rand.Seed(time.Now().UnixNano())

	//price := rand.Float64() * details.Price
	price:= float64(1000 + rand.Intn(10 - 1))
	amount := rand.Float64() * float64(rand.Intn(50 - 1)) // Assuming maximum amount is 50
	bid := rand.Intn(2) == 0
	timestamp := time.Now().UnixNano()

	order := Order{
		ID:        uint64(i+1), // Random ID for demonstration purpose
		UserID:    uint64(i+1001), // Random UserID for demonstration purpose
		Amount:    amount,
		Bid:       bid,
		Limit:     &Limit{Price: price},
		Timestamp: timestamp,
	}

	return order
}


// RandFloatWithPrecision generates a random float with the specified precision
func RandFloatWithPrecision(min, max float64, precision int) float64 {
	rand.Seed(time.Now().UnixNano())
	value := min + rand.Float64()*(max-min)
	precisionFactor := math.Pow(10, float64(precision))
	return math.Round(value*precisionFactor) / precisionFactor
}




// getBestBidPrice returns the highest bid price across all markets in the order book
func (ob *OrderBook) getBestBidPrice() float64 {
	var bestBidPrice float64
	ob.mutex.RLock()
	defer ob.mutex.RUnlock()

	for _, marketOrderBook := range ob.MarketOrderBooks {
		for price := range marketOrderBook.bids {
			if bestBidPrice == 0 || price > bestBidPrice {
				bestBidPrice = price
			}
		}
	}
	return bestBidPrice
}




// getBestAskPrice returns the lowest ask price across all markets in the order book
func (ob *OrderBook) getBestAskPrice() float64 {
    var bestAskPrice float64
    ob.mutex.RLock()
    defer ob.mutex.RUnlock()

    // Initialize bestAskPrice to a non-zero value to ensure correct comparison
    // with the first encountered ask price
    bestAskPriceSet := false

    // Iterate over each MarketOrderBook in the OrderBook
    for _, marketOrderBook := range ob.MarketOrderBooks {
        // Iterate over each ask price in the current MarketOrderBook
        for price := range marketOrderBook.asks {
            // Update bestAskPrice if it's uninitialized or the current price is lower
            if !bestAskPriceSet || price < bestAskPrice {
                bestAskPrice = price
                bestAskPriceSet = true
            }
        }
    }
    return bestAskPrice
}







// PrintOrderBook prints the order book for a specific market
func (ob *OrderBook) PrintOrderBook(marketID string) {
    ob.mutex.RLock()
    defer ob.mutex.RUnlock()
	fmt.Print("\033[H\033[2J") // Clear console
    if marketOrderBook, ok := ob.MarketOrderBooks[marketID]; ok {
        
        fmt.Printf("Order Book for Market %s:\n", marketID)
		fmt.Println("---------------------------------------------------------")
		fmt.Println("---------------------------------------------------------")
        fmt.Println("Bids: ", "Total No of BIDS: ", len(marketOrderBook.bids))
		fmt.Println("---------------------------------------------------------")
        printOrders(marketOrderBook.bids)
		fmt.Println("---------------------------------------------------------")
        fmt.Println("Asks:", "Total No of ASKS: ", len(marketOrderBook.asks))
		fmt.Println("---------------------------------------------------------")
        printOrders(marketOrderBook.asks)
		fmt.Println("---------------------------------------------------------")

        fmt.Println("getBestBidPrice: ", ob.getBestBidPrice())
        //fmt.Println("Current timestamp: ", time.Now())
        fmt.Println("getBestAskPrice: ", ob.getBestAskPrice())
		ob.MatchOrders(marketID)
		//precision := 2 // Configurable precision
		//randomNumber := RandFloatWithPrecision(0, 100, precision)
		//fmt.Printf("Random number with %d decimal places: %.2f\n", precision, randomNumber)


    } else {
        fmt.Printf("Market %s not found in the order book.\n", marketID)
    }
}



// PrintOrders prints the orders for a given side (bids/asks) of a market
func printOrders(orderMap map[float64]OrderSlice) {
    fmt.Printf("%-20s %-20s %-20s\n", "Price", "Amount", "Total Amount")
    for price, orders := range orderMap {
        totalAmount := 0.0
        for _, order := range orders {
            totalAmount += order.Amount
        }
        fmt.Printf("%-20.2f %-20.2f %-20.2f\n", price, orders[0].Amount, totalAmount)
    }
}


// MatchOrders matches buy orders with sell orders
func (ob *OrderBook) MatchOrders(marketID string) {
   // ob.mutex.Lock()
   // defer ob.mutex.Unlock()

    marketOrderBook, ok := ob.MarketOrderBooks[marketID]
    if !ok {
        fmt.Printf("Market %s not found in the order book.\n", marketID)
        return
    }

    // Iterate through bids and asks to find matching orders
    for buyPrice, buyOrders := range marketOrderBook.bids {
        sellOrders, ok := marketOrderBook.asks[buyPrice]
        if !ok {
            continue // No matching sell orders at this price
        }

        for len(buyOrders) > 0 && len(sellOrders) > 0 {
            buyOrder := buyOrders[0]
            sellOrder := sellOrders[0]

            // Calculate the amount to be traded
            tradeAmount := math.Min(buyOrder.Amount, sellOrder.Amount)

            // Print matching and partial fill information
			fmt.Println("---------------------------------------------------")
            fmt.Printf("Matching order price: %v\n", sellOrder.Limit.Price )
			fmt.Printf("Matching order Qty: %v\n",sellOrder.Amount)
			fmt.Println("---------------------------------------------------")
            fmt.Printf("Partial fill price: %v\n", buyOrder.Limit.Price)
			fmt.Printf("Matching order Qty: %v\n",buyOrder.Amount )

            // Update the order amounts
            buyOrder.Amount -= tradeAmount
            sellOrder.Amount -= tradeAmount

            // Remove filled orders from the order book if their amount becomes zero
            if buyOrder.Amount == 0 {
                buyOrders = buyOrders[1:]
            }
            if sellOrder.Amount == 0 {
                sellOrders = sellOrders[1:]
            }
        }

        // Update the order book after matching
        if len(buyOrders) == 0 {
            delete(marketOrderBook.bids, buyPrice)
        } else {
            marketOrderBook.bids[buyPrice] = buyOrders
        }
        if len(sellOrders) == 0 {
            delete(marketOrderBook.asks, buyPrice)
        } else {
            marketOrderBook.asks[buyPrice] = sellOrders
        }
    }
}

// SimulateRealTimeOrderBook simulates real-time updates to the order book
func (ob *OrderBook) SimulateRealTimeOrderBook() {

	marketID := "B_BTC_INR"
	details := MarketDetails{
		ContractSymbol: "BTC",
		Precision:      2,
		ContractName:   "Bitcoin",
		Price:          500.0, // Example price
		AskDelta:       0.1,
		BidDelta:       0.1,
		ConversionRate: 85.0,
		CurrencySymbol: "INR",
	}
	// Simulate order book updates every second
	for {
		//ob.UpdateOrderBook()
		
		numOrders := 2 // Number of random orders to add
	
		elapsed := ob.MeasureAddPerformance(marketID, numOrders, details)
		fmt.Printf("Time taken to add %d orders: %v\n", numOrders, elapsed)
		//MeasureCancelPerformance(ob, 10)
		ob.PrintOrderBook(marketID)
		time.Sleep(3 * time.Second)
		
	}

	// for {
	// 	ob.MatchOrders(marketID)
	// 	time.Sleep(2 * time.Second)
	// }


}

func main() {
	ob := NewOrderBook()


	// Simulate real-time order book updates
	go ob.SimulateRealTimeOrderBook()

	// Wait indefinitely
	select {}

}
