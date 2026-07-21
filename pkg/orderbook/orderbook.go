// Package orderbook implements a central limit order book (CLOB) with
// price–time priority.
//
// Design (see docs/SPEC.md §6.2): each side keeps a map from price → *PriceLevel
// for O(1) level lookup, plus a price-sorted slice of prices. Each level holds
// its resting orders in an intrusive doubly-linked FIFO list, and an
// orderID → *node index gives O(1) cancellation of any order without scanning.
// Best bid/ask is O(1).
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

// node is an order's entry in a price level's intrusive FIFO list. It also holds
// a back-pointer to its level so cancellation is O(1) end to end.
type node struct {
	order *types.Order
	prev  *node
	next  *node
	level *PriceLevel
}

// PriceLevel is the FIFO queue of resting orders at one price.
type PriceLevel struct {
	Price    decimal.Decimal
	TotalQty decimal.Decimal
	head     *node // oldest order (front of the FIFO)
	tail     *node // newest order
	count    int
}

// push appends a node at the tail (time priority) and grows the aggregate.
func (pl *PriceLevel) push(n *node) {
	n.prev = pl.tail
	n.next = nil
	if pl.tail != nil {
		pl.tail.next = n
	} else {
		pl.head = n
	}
	pl.tail = n
	pl.count++
	pl.TotalQty = pl.TotalQty.Add(n.order.RemainingQty)
}

// unlink removes a node in O(1) and decrements the aggregate.
func (pl *PriceLevel) unlink(n *node) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		pl.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		pl.tail = n.prev
	}
	pl.count--
	pl.TotalQty = pl.TotalQty.Sub(n.order.RemainingQty)
	n.prev, n.next = nil, nil
}

func (pl *PriceLevel) isEmpty() bool { return pl.count == 0 }

// OrderBook is a single-symbol CLOB.
type OrderBook struct {
	mu             sync.RWMutex
	symbol         string
	bids           map[string]*PriceLevel // price string -> level
	asks           map[string]*PriceLevel
	nodes          map[string]*node  // orderID -> node, for O(1) cancel
	bidPrices      []decimal.Decimal // sorted descending (best first)
	askPrices      []decimal.Decimal // sorted ascending (best first)
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
		nodes:     make(map[string]*node),
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

	if len(ob.nodes) >= ob.maxOrders {
		return types.ErrOrderBookFull
	}
	if _, exists := ob.nodes[order.ID]; exists {
		return nil
	}

	ob.sequenceNum++
	order.SequenceNum = ob.sequenceNum

	priceStr := order.Price.String()
	var level *PriceLevel
	if order.Side == types.SideBuy {
		l, ok := ob.bids[priceStr]
		if !ok {
			l = &PriceLevel{Price: order.Price}
			ob.bids[priceStr] = l
			ob.insertBidPrice(order.Price)
		}
		level = l
	} else {
		l, ok := ob.asks[priceStr]
		if !ok {
			l = &PriceLevel{Price: order.Price}
			ob.asks[priceStr] = l
			ob.insertAskPrice(order.Price)
		}
		level = l
	}

	n := &node{order: order, level: level}
	level.push(n)
	ob.nodes[order.ID] = n
	return nil
}

// Remove deletes an order by id in O(1) (plus O(P) only when it empties its
// level, which cleans up the price slice), and returns the order.
func (ob *OrderBook) Remove(orderID string) (*types.Order, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	n, exists := ob.nodes[orderID]
	if !exists {
		return nil, types.ErrOrderNotFound
	}
	level := n.level
	level.unlink(n)
	delete(ob.nodes, orderID)

	if level.isEmpty() {
		priceStr := n.order.Price.String()
		if n.order.Side == types.SideBuy {
			delete(ob.bids, priceStr)
			ob.removeBidPrice(n.order.Price)
		} else {
			delete(ob.asks, priceStr)
			ob.removeAskPrice(n.order.Price)
		}
	}
	return n.order, nil
}

// Get returns a resting order by id.
func (ob *OrderBook) Get(orderID string) (*types.Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	n, ok := ob.nodes[orderID]
	if !ok {
		return nil, false
	}
	return n.order, true
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
// removing it, or nil.
func (ob *OrderBook) PeekBestBidOrder() *types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 {
		return nil
	}
	level := ob.bids[ob.bidPrices[0].String()]
	if level == nil || level.head == nil {
		return nil
	}
	return level.head.order
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
	if level == nil || level.head == nil {
		return nil
	}
	return level.head.order
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
	out := make([]*types.Order, 0, level.count)
	for n := level.head; n != nil; n = n.next {
		out = append(out, n.order)
	}
	return out
}

// UpdateOrderQuantity decrements the aggregate quantity at a resting order's
// level after that order is partially filled in place. O(1).
func (ob *OrderBook) UpdateOrderQuantity(orderID string, filledQty decimal.Decimal) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if n, ok := ob.nodes[orderID]; ok {
		n.level.TotalQty = n.level.TotalQty.Sub(filledQty)
	}
}

// RestoreOrderQuantity adds qty back to the aggregate total at a resting order's
// level, undoing a prior UpdateOrderQuantity (used by FOK reversal). O(1).
func (ob *OrderBook) RestoreOrderQuantity(orderID string, qty decimal.Decimal) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if n, ok := ob.nodes[orderID]; ok {
		n.level.TotalQty = n.level.TotalQty.Add(qty)
	}
}

// Count returns the number of resting orders.
func (ob *OrderBook) Count() int {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return len(ob.nodes)
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

// removeBidPrice deletes price from the descending-sorted bid price slice using
// binary search (O(log P) to locate) — not a linear scan, which on decimals
// would call the allocating Equal for every level.
func (ob *OrderBook) removeBidPrice(price decimal.Decimal) {
	low, high := 0, len(ob.bidPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.bidPrices[mid].GreaterThan(price) {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(ob.bidPrices) && ob.bidPrices[low].Equal(price) {
		ob.bidPrices = append(ob.bidPrices[:low], ob.bidPrices[low+1:]...)
	}
}

// removeAskPrice deletes price from the ascending-sorted ask price slice using
// binary search.
func (ob *OrderBook) removeAskPrice(price decimal.Decimal) {
	low, high := 0, len(ob.askPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.askPrices[mid].LessThan(price) {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(ob.askPrices) && ob.askPrices[low].Equal(price) {
		ob.askPrices = append(ob.askPrices[:low], ob.askPrices[low+1:]...)
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
			for n := levels[prices[i].String()].head; n != nil; n = n.next {
				out = append(out, L3Order{
					OrderID:     n.order.ID,
					UserID:      n.order.UserID,
					Price:       n.order.Price,
					Quantity:    n.order.RemainingQty,
					SequenceNum: n.order.SequenceNum,
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
