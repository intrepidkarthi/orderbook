// Package orderbook implements a central limit order book (CLOB) with
// price–time priority.
//
// Design (see docs/SPEC.md §6.2): each side keeps a map from price → *PriceLevel
// for O(1) level lookup, plus a price-sorted slice of prices. Each level holds
// its resting orders in an intrusive doubly-linked FIFO list, and an
// orderID → *node index gives O(1) cancellation of any order without scanning.
// Best bid/ask is O(1).
//
// Prices are integer ticks and quantities integer lots (int64) — exact and fast.
// The book is safe for concurrent use, but it is intended to be driven by a
// single writer (the matching engine); readers should prefer Snapshot.
package orderbook

import (
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// node is an order's entry in a price level's intrusive FIFO list. It also holds
// a back-pointer to its level so cancellation is O(1) end to end.
type node struct {
	order *types.Order
	prev  *node
	next  *node
	level *PriceLevel
}

// PriceLevel is the FIFO queue of resting orders at one price. Price is in ticks
// and TotalQty in lots.
type PriceLevel struct {
	Price    int64
	TotalQty int64
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
	pl.TotalQty += n.order.RemainingQty
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
	pl.TotalQty -= n.order.RemainingQty
	n.prev, n.next = nil, nil
}

func (pl *PriceLevel) isEmpty() bool { return pl.count == 0 }

// OrderBook is a single-symbol CLOB.
type OrderBook struct {
	mu             sync.RWMutex
	symbol         string
	bids           map[int64]*PriceLevel // price ticks -> level
	asks           map[int64]*PriceLevel
	nodes          map[int64]*node // orderID -> node, for O(1) cancel
	perUser        map[string]int  // resting-order count per user, for admission caps
	bidPrices      []int64         // sorted descending (best first)
	askPrices      []int64         // sorted ascending (best first)
	lastTradePrice int64
	lastTradeTime  time.Time
	sequenceNum    uint64 // book version, bumped on each add
	orderSeq       int64  // assigns ids to orders that arrive without one
	maxOrders      int
	clock          func() time.Time

	// Free-lists: recycled nodes and price levels so steady-state add/cancel and
	// level churn allocate nothing on the heap. The book is single-writer (guarded
	// by mu), so plain slices beat sync.Pool here — no GC handoff, no contention.
	nodePool  []*node
	levelPool []*PriceLevel
}

// getNode returns a node from the free-list (or a fresh one), initialised for
// order at level.
func (ob *OrderBook) getNode(order *types.Order, level *PriceLevel) *node {
	if n := len(ob.nodePool); n > 0 {
		nd := ob.nodePool[n-1]
		ob.nodePool = ob.nodePool[:n-1]
		nd.order, nd.level, nd.prev, nd.next = order, level, nil, nil
		return nd
	}
	return &node{order: order, level: level}
}

// putNode clears and recycles a node.
func (ob *OrderBook) putNode(nd *node) {
	nd.order, nd.level, nd.prev, nd.next = nil, nil, nil, nil
	ob.nodePool = append(ob.nodePool, nd)
}

// getLevel returns a price level from the free-list (or a fresh one), reset to
// empty at price.
func (ob *OrderBook) getLevel(price int64) *PriceLevel {
	if n := len(ob.levelPool); n > 0 {
		l := ob.levelPool[n-1]
		ob.levelPool = ob.levelPool[:n-1]
		*l = PriceLevel{Price: price}
		return l
	}
	return &PriceLevel{Price: price}
}

// putLevel clears and recycles a price level.
func (ob *OrderBook) putLevel(l *PriceLevel) {
	*l = PriceLevel{}
	ob.levelPool = append(ob.levelPool, l)
}

// Config configures a new order book.
type Config struct {
	Symbol    string
	MaxOrders int // 0 => default of 100_000
	// Clock supplies the timestamps stamped on snapshots and the last-trade time.
	// nil => time.Now. Inject a deterministic clock to make snapshots byte-identical
	// under replay.
	Clock func() time.Time
}

// New builds an empty order book.
func New(config Config) *OrderBook {
	if config.MaxOrders == 0 {
		config.MaxOrders = 100_000
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &OrderBook{
		symbol:    config.Symbol,
		bids:      make(map[int64]*PriceLevel),
		asks:      make(map[int64]*PriceLevel),
		nodes:     make(map[int64]*node),
		perUser:   make(map[string]int),
		bidPrices: make([]int64, 0),
		askPrices: make([]int64, 0),
		maxOrders: config.MaxOrders,
		clock:     config.Clock,
	}
}

// Symbol returns the book's instrument symbol.
func (ob *OrderBook) Symbol() string { return ob.symbol }

// Add inserts a resting order. An order that arrives without an id (ID==0) is
// assigned a monotonic one. Duplicate ids are ignored; a full book returns
// ErrOrderBookFull.
func (ob *OrderBook) Add(order *types.Order) error {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	if len(ob.nodes) >= ob.maxOrders {
		return types.ErrOrderBookFull
	}

	ob.sequenceNum++
	if order.ID == 0 {
		ob.orderSeq++
		order.ID = ob.orderSeq
	}
	if _, exists := ob.nodes[order.ID]; exists {
		return nil
	}

	price := order.Price
	var level *PriceLevel
	if order.Side == types.SideBuy {
		l, ok := ob.bids[price]
		if !ok {
			l = ob.getLevel(price)
			ob.bids[price] = l
			ob.insertBidPrice(price)
		}
		level = l
	} else {
		l, ok := ob.asks[price]
		if !ok {
			l = ob.getLevel(price)
			ob.asks[price] = l
			ob.insertAskPrice(price)
		}
		level = l
	}

	n := ob.getNode(order, level)
	level.push(n)
	ob.nodes[order.ID] = n
	ob.perUser[order.UserID]++
	return nil
}

// Remove deletes an order by id in O(1) (plus O(P) only when it empties its
// level, which cleans up the price slice), and returns the order.
func (ob *OrderBook) Remove(orderID int64) (*types.Order, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	n, exists := ob.nodes[orderID]
	if !exists {
		return nil, types.ErrOrderNotFound
	}
	order := n.order
	level := n.level
	level.unlink(n)
	delete(ob.nodes, orderID)
	ob.putNode(n)
	if c := ob.perUser[order.UserID]; c <= 1 {
		delete(ob.perUser, order.UserID)
	} else {
		ob.perUser[order.UserID] = c - 1
	}

	if level.isEmpty() {
		price := order.Price
		if order.Side == types.SideBuy {
			delete(ob.bids, price)
			ob.removeBidPrice(price)
		} else {
			delete(ob.asks, price)
			ob.removeAskPrice(price)
		}
		ob.putLevel(level)
	}
	return order, nil
}

// Get returns a resting order by id.
func (ob *OrderBook) Get(orderID int64) (*types.Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	n, ok := ob.nodes[orderID]
	if !ok {
		return nil, false
	}
	return n.order, true
}

// BestBid returns the highest bid price (ticks) and its aggregate quantity (lots).
func (ob *OrderBook) BestBid() (price, qty int64, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 {
		return 0, 0, false
	}
	p := ob.bidPrices[0]
	return p, ob.bids[p].TotalQty, true
}

// BestAsk returns the lowest ask price (ticks) and its aggregate quantity (lots).
func (ob *OrderBook) BestAsk() (price, qty int64, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.askPrices) == 0 {
		return 0, 0, false
	}
	p := ob.askPrices[0]
	return p, ob.asks[p].TotalQty, true
}

// PeekBestBidOrder returns the front (oldest) order at the best bid without
// removing it, or nil.
func (ob *OrderBook) PeekBestBidOrder() *types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 {
		return nil
	}
	level := ob.bids[ob.bidPrices[0]]
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
	level := ob.asks[ob.askPrices[0]]
	if level == nil || level.head == nil {
		return nil
	}
	return level.head.order
}

// Spread returns best ask − best bid (ticks), or ok=false if either side is empty.
func (ob *OrderBook) Spread() (int64, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 || len(ob.askPrices) == 0 {
		return 0, false
	}
	return ob.askPrices[0] - ob.bidPrices[0], true
}

// MidPrice returns (best bid + best ask) / 2 in ticks (floored), or ok=false if
// either side is empty.
func (ob *OrderBook) MidPrice() (int64, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bidPrices) == 0 || len(ob.askPrices) == 0 {
		return 0, false
	}
	return (ob.bidPrices[0] + ob.askPrices[0]) / 2, true
}

// LastTradePrice returns the most recent execution price (ticks).
func (ob *OrderBook) LastTradePrice() int64 {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.lastTradePrice
}

// SetLastTradePrice records the most recent execution price (ticks) and time.
func (ob *OrderBook) SetLastTradePrice(price int64) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.lastTradePrice = price
	ob.lastTradeTime = ob.clock().UTC()
}

// GetBidLevels returns up to depth aggregated bid levels, best first (copies).
func (ob *OrderBook) GetBidLevels(depth int) []*PriceLevel {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	levels := make([]*PriceLevel, 0, depth)
	for i := 0; i < len(ob.bidPrices) && i < depth; i++ {
		l := ob.bids[ob.bidPrices[i]]
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
		l := ob.asks[ob.askPrices[i]]
		levels = append(levels, &PriceLevel{Price: l.Price, TotalQty: l.TotalQty})
	}
	return levels
}

// GetOrdersAtPrice returns a copy of the FIFO order queue at a given side/price
// (ticks).
func (ob *OrderBook) GetOrdersAtPrice(side types.Side, price int64) []*types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	var level *PriceLevel
	if side == types.SideBuy {
		level = ob.bids[price]
	} else {
		level = ob.asks[price]
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
func (ob *OrderBook) UpdateOrderQuantity(orderID int64, filledQty int64) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if n, ok := ob.nodes[orderID]; ok {
		n.level.TotalQty -= filledQty
	}
}

// RestoreOrderQuantity adds qty back to the aggregate total at a resting order's
// level, undoing a prior UpdateOrderQuantity (used by FOK reversal). O(1).
func (ob *OrderBook) RestoreOrderQuantity(orderID int64, qty int64) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if n, ok := ob.nodes[orderID]; ok {
		n.level.TotalQty += qty
	}
}

// Count returns the number of resting orders.
func (ob *OrderBook) Count() int {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return len(ob.nodes)
}

// OrdersByUser returns how many resting orders belong to userID — the basis for a
// per-account open-order cap. Maintained in O(1) on Add/Remove, so it survives
// fills (which remove through Remove) and rebuilds correctly on snapshot restore
// (which re-Adds each order).
func (ob *OrderBook) OrdersByUser(userID string) int {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.perUser[userID]
}

// DepthWithin returns the total resting quantity (lots) across both sides at
// prices in the inclusive tick range [lo, hi] — how much real liquidity backs a
// price region. Used to reject a mark that no depth supports.
func (ob *OrderBook) DepthWithin(lo, hi int64) int64 {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	var total int64
	for price, lvl := range ob.bids {
		if price >= lo && price <= hi {
			total += lvl.TotalQty
		}
	}
	for price, lvl := range ob.asks {
		if price >= lo && price <= hi {
			total += lvl.TotalQty
		}
	}
	return total
}

// Orders returns every resting order in price-then-time order (bids best-first,
// then asks best-first, FIFO within each level). Re-adding them to a fresh book
// in this order reproduces the exact structure — the basis for a book snapshot.
func (ob *OrderBook) Orders() []*types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	out := make([]*types.Order, 0, len(ob.nodes))
	appendSide := func(prices []int64, levels map[int64]*PriceLevel) {
		for _, p := range prices {
			for n := levels[p].head; n != nil; n = n.next {
				out = append(out, n.order)
			}
		}
	}
	appendSide(ob.bidPrices, ob.bids)
	appendSide(ob.askPrices, ob.asks)
	return out
}

// insertBidPrice inserts price into the descending-sorted bid price slice.
func (ob *OrderBook) insertBidPrice(price int64) {
	low, high := 0, len(ob.bidPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.bidPrices[mid] > price {
			low = mid + 1
		} else {
			high = mid
		}
	}
	ob.bidPrices = append(ob.bidPrices, 0)
	copy(ob.bidPrices[low+1:], ob.bidPrices[low:])
	ob.bidPrices[low] = price
}

// insertAskPrice inserts price into the ascending-sorted ask price slice.
func (ob *OrderBook) insertAskPrice(price int64) {
	low, high := 0, len(ob.askPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.askPrices[mid] < price {
			low = mid + 1
		} else {
			high = mid
		}
	}
	ob.askPrices = append(ob.askPrices, 0)
	copy(ob.askPrices[low+1:], ob.askPrices[low:])
	ob.askPrices[low] = price
}

// removeBidPrice deletes price from the descending-sorted bid price slice using
// binary search (O(log P) to locate).
func (ob *OrderBook) removeBidPrice(price int64) {
	low, high := 0, len(ob.bidPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.bidPrices[mid] > price {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(ob.bidPrices) && ob.bidPrices[low] == price {
		ob.bidPrices = append(ob.bidPrices[:low], ob.bidPrices[low+1:]...)
	}
}

// removeAskPrice deletes price from the ascending-sorted ask price slice using
// binary search.
func (ob *OrderBook) removeAskPrice(price int64) {
	low, high := 0, len(ob.askPrices)
	for low < high {
		mid := (low + high) / 2
		if ob.askPrices[mid] < price {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(ob.askPrices) && ob.askPrices[low] == price {
		ob.askPrices = append(ob.askPrices[:low], ob.askPrices[low+1:]...)
	}
}

// SnapshotLevel is one aggregated price level in a Snapshot (price in ticks,
// quantity in lots).
type SnapshotLevel struct {
	Price    int64 `json:"price"`
	Quantity int64 `json:"quantity"`
}

// Snapshot is a consistent, copyable view of the book to a given depth — the
// read model for signals, UIs, and the WASM demo.
type Snapshot struct {
	Symbol         string          `json:"symbol"`
	Bids           []SnapshotLevel `json:"bids"`
	Asks           []SnapshotLevel `json:"asks"`
	LastTradePrice int64           `json:"last_trade_price"`
	SequenceNum    uint64          `json:"sequence_num"`
	Timestamp      time.Time       `json:"timestamp"`
}

// L3Order is a single resting order in an L3 (market-by-order) snapshot.
type L3Order struct {
	OrderID     int64  `json:"order_id"`
	UserID      string `json:"user_id"`
	Price       int64  `json:"price"`
	Quantity    int64  `json:"quantity"` // remaining
	SequenceNum int64  `json:"sequence_num"`
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

	s := &SnapshotL3{Symbol: ob.symbol, SequenceNum: ob.sequenceNum, Timestamp: ob.clock().UTC()}
	appendSide := func(prices []int64, levels map[int64]*PriceLevel) []L3Order {
		out := make([]L3Order, 0, depth)
		for i := 0; i < len(prices) && i < depth; i++ {
			for n := levels[prices[i]].head; n != nil; n = n.next {
				out = append(out, L3Order{
					OrderID:     n.order.ID,
					UserID:      n.order.UserID,
					Price:       n.order.Price,
					Quantity:    n.order.RemainingQty,
					SequenceNum: n.order.ID,
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
		Timestamp:      ob.clock().UTC(),
	}
	for i := 0; i < len(ob.bidPrices) && i < depth; i++ {
		l := ob.bids[ob.bidPrices[i]]
		s.Bids = append(s.Bids, SnapshotLevel{Price: l.Price, Quantity: l.TotalQty})
	}
	for i := 0; i < len(ob.askPrices) && i < depth; i++ {
		l := ob.asks[ob.askPrices[i]]
		s.Asks = append(s.Asks, SnapshotLevel{Price: l.Price, Quantity: l.TotalQty})
	}
	return s
}
