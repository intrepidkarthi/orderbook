// Package orderbook implements a central limit order book (CLOB) with
// price–time priority.
//
// Design (see docs/SPEC.md §6.2): each side keeps a map from price → *PriceLevel
// for O(1) level lookup, plus a price-sorted slice of prices maintained with
// binary-search insertion. Best bid/ask is therefore O(1); inserting a brand new
// price level is O(log n) to find the slot plus O(n) to shift. Within a level,
// orders form a FIFO queue, which is what gives time priority.
//
// The book is safe for concurrent use, but it is intended to be driven by a
// single writer (the matching engine); readers should prefer Snapshot.
package orderbook

import (
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/shopspring/decimal"
)

// PriceLevel is the FIFO queue of resting orders at one price.
type PriceLevel struct {
	Price    decimal.Decimal
	Orders   []*types.Order
	TotalQty decimal.Decimal
}

// Add appends an order to the back of the level (time priority) and grows the
// aggregate quantity.
func (pl *PriceLevel) Add(order *types.Order) {
	pl.Orders = append(pl.Orders, order)
	pl.TotalQty = pl.TotalQty.Add(order.RemainingQty)
}

// Remove drops an order by id, decrementing the aggregate quantity. Returns
// whether the order was present.
func (pl *PriceLevel) Remove(orderID string) bool {
	for i, o := range pl.Orders {
		if o.ID == orderID {
			pl.TotalQty = pl.TotalQty.Sub(o.RemainingQty)
			pl.Orders = append(pl.Orders[:i], pl.Orders[i+1:]...)
			return true
		}
	}
	return false
}

// reduceTotal decrements the aggregate quantity of the level by filledQty (used
// when a resting order is partially filled in place).
func (pl *PriceLevel) reduceTotal(filledQty decimal.Decimal) {
	pl.TotalQty = pl.TotalQty.Sub(filledQty)
}

// IsEmpty reports whether the level holds no orders.
func (pl *PriceLevel) IsEmpty() bool { return len(pl.Orders) == 0 }

// OrderBook is a single-symbol CLOB.
type OrderBook struct {
	mu             sync.RWMutex
	symbol         string
	bids           map[string]*PriceLevel // price string -> level
	asks           map[string]*PriceLevel
	orders         map[string]*types.Order // orderID -> order
	bidPrices      []decimal.Decimal       // sorted descending (best first)
	askPrices      []decimal.Decimal       // sorted ascending (best first)
	lastTradePrice decimal.Decimal
	lastTradeTime  time.Time
	sequenceNum    uint64
	maxOrders      int
}

// Config configures a new order book.
type Config struct {
	Symbol    string
	MaxOrders int // 0 => default of 100_000
}

// New builds an empty order book.
func New(config Config) *OrderBook {
	if config.MaxOrders == 0 {
		config.MaxOrders = 100_000
	}
	return &OrderBook{
		symbol:    config.Symbol,
		bids:      make(map[string]*PriceLevel),
		asks:      make(map[string]*PriceLevel),
		orders:    make(map[string]*types.Order),
		bidPrices: make([]decimal.Decimal, 0),
		askPrices: make([]decimal.Decimal, 0),
		maxOrders: config.MaxOrders,
	}
}

// Symbol returns the book's instrument symbol.
func (ob *OrderBook) Symbol() string { return ob.symbol }

// Add inserts a resting order. Duplicate ids are ignored; a full book returns
// ErrOrderBookFull.
func (ob *OrderBook) Add(order *types.Order) error {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	if len(ob.orders) >= ob.maxOrders {
		return types.ErrOrderBookFull
	}
	if _, exists := ob.orders[order.ID]; exists {
		return nil
	}

	ob.sequenceNum++
	order.SequenceNum = ob.sequenceNum
	ob.orders[order.ID] = order

	priceStr := order.Price.String()
	if order.Side == types.SideBuy {
		level, ok := ob.bids[priceStr]
		if !ok {
			level = &PriceLevel{Price: order.Price}
			ob.bids[priceStr] = level
			ob.insertBidPrice(order.Price)
		}
		level.Add(order)
	} else {
		level, ok := ob.asks[priceStr]
		if !ok {
			level = &PriceLevel{Price: order.Price}
			ob.asks[priceStr] = level
			ob.insertAskPrice(order.Price)
		}
		level.Add(order)
	}
	return nil
}

// Remove deletes an order by id, cleaning up an emptied price level.
func (ob *OrderBook) Remove(orderID string) (*types.Order, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	order, exists := ob.orders[orderID]
	if !exists {
		return nil, types.ErrOrderNotFound
	}

	priceStr := order.Price.String()
	if order.Side == types.SideBuy {
		if level, ok := ob.bids[priceStr]; ok {
			level.Remove(orderID)
			if level.IsEmpty() {
				delete(ob.bids, priceStr)
				ob.removeBidPrice(order.Price)
			}
		}
	} else {
		if level, ok := ob.asks[priceStr]; ok {
			level.Remove(orderID)
			if level.IsEmpty() {
				delete(ob.asks, priceStr)
				ob.removeAskPrice(order.Price)
			}
		}
	}

	delete(ob.orders, orderID)
	return order, nil
}

// Get returns a resting order by id.
func (ob *OrderBook) Get(orderID string) (*types.Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	order, ok := ob.orders[orderID]
	return order, ok
}

// BestBid returns the highest bid price and its aggregate quantity.
func (ob *OrderBook) BestBid() (price, qty decimal.Decimal, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 {
		return decimal.Zero, decimal.Zero, false
	}
	p := ob.bidPrices[0]
	return p, ob.bids[p.String()].TotalQty, true
}

// BestAsk returns the lowest ask price and its aggregate quantity.
func (ob *OrderBook) BestAsk() (price, qty decimal.Decimal, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.askPrices) == 0 {
		return decimal.Zero, decimal.Zero, false
	}
	p := ob.askPrices[0]
	return p, ob.asks[p.String()].TotalQty, true
}

// PeekBestBidOrder returns the front (oldest) order at the best bid without
// removing it, or nil. Optimized for the matching hot path — callers inside the
// engine already hold the appropriate lock context.
func (ob *OrderBook) PeekBestBidOrder() *types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 {
		return nil
	}
	level := ob.bids[ob.bidPrices[0].String()]
	if level == nil || len(level.Orders) == 0 {
		return nil
	}
	return level.Orders[0]
}

// PeekBestAskOrder returns the front (oldest) order at the best ask without
// removing it, or nil.
func (ob *OrderBook) PeekBestAskOrder() *types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.askPrices) == 0 {
		return nil
	}
	level := ob.asks[ob.askPrices[0].String()]
	if level == nil || len(level.Orders) == 0 {
		return nil
	}
	return level.Orders[0]
}

// Spread returns best ask − best bid, or ok=false if either side is empty.
func (ob *OrderBook) Spread() (decimal.Decimal, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 || len(ob.askPrices) == 0 {
		return decimal.Zero, false
	}
	return ob.askPrices[0].Sub(ob.bidPrices[0]), true
}

// MidPrice returns (best bid + best ask) / 2, or ok=false if either side is empty.
func (ob *OrderBook) MidPrice() (decimal.Decimal, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 || len(ob.askPrices) == 0 {
		return decimal.Zero, false
	}
	return ob.bidPrices[0].Add(ob.askPrices[0]).Div(decimal.NewFromInt(2)), true
}

// LastTradePrice returns the most recent execution price.
func (ob *OrderBook) LastTradePrice() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.lastTradePrice
}

// SetLastTradePrice records the most recent execution price and time.
func (ob *OrderBook) SetLastTradePrice(price decimal.Decimal) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.lastTradePrice = price
	ob.lastTradeTime = time.Now().UTC()
}

// GetBidLevels returns up to depth aggregated bid levels, best first (copies).
func (ob *OrderBook) GetBidLevels(depth int) []*PriceLevel {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	levels := make([]*PriceLevel, 0, depth)
	for i := 0; i < len(ob.bidPrices) && i < depth; i++ {
		l := ob.bids[ob.bidPrices[i].String()]
		levels = append(levels, &PriceLevel{Price: l.Price, TotalQty: l.TotalQty})
	}
	return levels
}

// GetAskLevels returns up to depth aggregated ask levels, best first (copies).
func (ob *OrderBook) GetAskLevels(depth int) []*PriceLevel {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	levels := make([]*PriceLevel, 0, depth)
	for i := 0; i < len(ob.askPrices) && i < depth; i++ {
		l := ob.asks[ob.askPrices[i].String()]
		levels = append(levels, &PriceLevel{Price: l.Price, TotalQty: l.TotalQty})
	}
	return levels
}

// GetOrdersAtPrice returns a copy of the FIFO order queue at a given side/price.
func (ob *OrderBook) GetOrdersAtPrice(side types.Side, price decimal.Decimal) []*types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	var level *PriceLevel
	if side == types.SideBuy {
		level = ob.bids[price.String()]
	} else {
		level = ob.asks[price.String()]
	}
	if level == nil {
		return nil
	}
	out := make([]*types.Order, len(level.Orders))
	copy(out, level.Orders)
	return out
}

// UpdateOrderQuantity decrements the aggregate quantity at a resting order's
// level after that order is partially filled in place.
func (ob *OrderBook) UpdateOrderQuantity(orderID string, filledQty decimal.Decimal) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	order, ok := ob.orders[orderID]
	if !ok {
		return
	}
	priceStr := order.Price.String()
	if order.Side == types.SideBuy {
		if level, ok := ob.bids[priceStr]; ok {
			level.reduceTotal(filledQty)
		}
	} else {
		if level, ok := ob.asks[priceStr]; ok {
			level.reduceTotal(filledQty)
		}
	}
}

// RestoreOrderQuantity adds qty back to the aggregate total at a resting order's
// level, undoing a prior UpdateOrderQuantity. Used by the matching engine to
// unwind partial fills against a still-resting maker when a Fill-or-Kill order
// cannot be completed (see matching.Engine.reverseTrade).
func (ob *OrderBook) RestoreOrderQuantity(orderID string, qty decimal.Decimal) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	order, ok := ob.orders[orderID]
	if !ok {
		return
	}
	priceStr := order.Price.String()
	if order.Side == types.SideBuy {
		if level, ok := ob.bids[priceStr]; ok {
			level.TotalQty = level.TotalQty.Add(qty)
		}
	} else {
		if level, ok := ob.asks[priceStr]; ok {
			level.TotalQty = level.TotalQty.Add(qty)
		}
	}
}

// Count returns the number of resting orders.
func (ob *OrderBook) Count() int {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return len(ob.orders)
}

// insertBidPrice inserts price into the descending-sorted bid price slice.
func (ob *OrderBook) insertBidPrice(price decimal.Decimal) {
	low, high := 0, len(ob.bidPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.bidPrices[mid].GreaterThan(price) {
			low = mid + 1
		} else {
			high = mid
		}
	}
	ob.bidPrices = append(ob.bidPrices, decimal.Zero)
	copy(ob.bidPrices[low+1:], ob.bidPrices[low:])
	ob.bidPrices[low] = price
}

// insertAskPrice inserts price into the ascending-sorted ask price slice.
func (ob *OrderBook) insertAskPrice(price decimal.Decimal) {
	low, high := 0, len(ob.askPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.askPrices[mid].LessThan(price) {
			low = mid + 1
		} else {
			high = mid
		}
	}
	ob.askPrices = append(ob.askPrices, decimal.Zero)
	copy(ob.askPrices[low+1:], ob.askPrices[low:])
	ob.askPrices[low] = price
}

func (ob *OrderBook) removeBidPrice(price decimal.Decimal) {
	for i, p := range ob.bidPrices {
		if price.Equal(p) {
			ob.bidPrices = append(ob.bidPrices[:i], ob.bidPrices[i+1:]...)
			return
		}
	}
}

func (ob *OrderBook) removeAskPrice(price decimal.Decimal) {
	for i, p := range ob.askPrices {
		if price.Equal(p) {
			ob.askPrices = append(ob.askPrices[:i], ob.askPrices[i+1:]...)
			return
		}
	}
}

// SnapshotLevel is one aggregated price level in a Snapshot.
type SnapshotLevel struct {
	Price    decimal.Decimal `json:"price"`
	Quantity decimal.Decimal `json:"quantity"`
}

// Snapshot is a consistent, copyable view of the book to a given depth — the
// read model for signals, UIs, and the WASM demo.
type Snapshot struct {
	Symbol         string          `json:"symbol"`
	Bids           []SnapshotLevel `json:"bids"`
	Asks           []SnapshotLevel `json:"asks"`
	LastTradePrice decimal.Decimal `json:"last_trade_price"`
	SequenceNum    uint64          `json:"sequence_num"`
	Timestamp      time.Time       `json:"timestamp"`
}

// L3Order is a single resting order in an L3 (market-by-order) snapshot.
type L3Order struct {
	OrderID     string          `json:"order_id"`
	UserID      string          `json:"user_id"`
	Price       decimal.Decimal `json:"price"`
	Quantity    decimal.Decimal `json:"quantity"` // remaining
	SequenceNum uint64          `json:"sequence_num"`
}

// SnapshotL3 is a market-by-order (L3) view: every resting order individually,
// preserving price then time order — the fullest market-data granularity.
type SnapshotL3 struct {
	Symbol      string    `json:"symbol"`
	Bids        []L3Order `json:"bids"`
	Asks        []L3Order `json:"asks"`
	SequenceNum uint64    `json:"sequence_num"`
	Timestamp   time.Time `json:"timestamp"`
}

// SnapshotL3 returns an order-by-order view of the top depth price levels of each
// side (best price first, FIFO within a level).
func (ob *OrderBook) SnapshotL3(depth int) *SnapshotL3 {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	s := &SnapshotL3{Symbol: ob.symbol, SequenceNum: ob.sequenceNum, Timestamp: time.Now().UTC()}
	appendSide := func(prices []decimal.Decimal, levels map[string]*PriceLevel) []L3Order {
		out := make([]L3Order, 0, depth)
		for i := 0; i < len(prices) && i < depth; i++ {
			for _, o := range levels[prices[i].String()].Orders {
				out = append(out, L3Order{
					OrderID:     o.ID,
					UserID:      o.UserID,
					Price:       o.Price,
					Quantity:    o.RemainingQty,
					SequenceNum: o.SequenceNum,
				})
			}
		}
		return out
	}
	s.Bids = appendSide(ob.bidPrices, ob.bids)
	s.Asks = appendSide(ob.askPrices, ob.asks)
	return s
}

// Snapshot returns the top depth levels of each side.
func (ob *OrderBook) Snapshot(depth int) *Snapshot {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	s := &Snapshot{
		Symbol:         ob.symbol,
		Bids:           make([]SnapshotLevel, 0, depth),
		Asks:           make([]SnapshotLevel, 0, depth),
		LastTradePrice: ob.lastTradePrice,
		SequenceNum:    ob.sequenceNum,
		Timestamp:      time.Now().UTC(),
	}
	for i := 0; i < len(ob.bidPrices) && i < depth; i++ {
		l := ob.bids[ob.bidPrices[i].String()]
		s.Bids = append(s.Bids, SnapshotLevel{Price: l.Price, Quantity: l.TotalQty})
	}
	for i := 0; i < len(ob.askPrices) && i < depth; i++ {
		l := ob.asks[ob.askPrices[i].String()]
		s.Asks = append(s.Asks, SnapshotLevel{Price: l.Price, Quantity: l.TotalQty})
	}
	return s
}
