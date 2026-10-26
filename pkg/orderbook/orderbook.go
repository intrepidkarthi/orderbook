// Package orderbook implements a central limit order book (CLOB) with
// price–time priority.
//
// Design (see docs/SPEC.md §6.2 and docs/BOOK-LADDER.md): each side keeps its price
// levels in one slice sorted best-last, so the best level is read and emptied in O(1)
// with no map between a price and its level. Each level holds its resting orders in
// an intrusive doubly-linked FIFO list, and an orderID → *node index gives O(1)
// cancellation of any order without scanning.
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
	// user is the interned account id, stored here so a cancel never has to hash
	// the account string again. See userTable in index.go.
	user int32
	// contributed is how much this node currently adds to its level's TotalQty.
	//
	// It exists because the level total cannot be derived from order.RemainingQty at
	// removal time: the matching engine fills an order (dropping RemainingQty to 0)
	// and only then removes it, so an unlink that subtracted RemainingQty would
	// subtract nothing and leave the maker's original size in the level forever.
	// That was a real bug — depth over-reported after every full fill — and it was
	// caused by the level total being a discipline the caller had to maintain at
	// every removal site rather than a property the book kept for itself.
	contributed int64
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
	n.contributed = n.order.RemainingQty
	n.prev = pl.tail
	n.next = nil
	if pl.tail != nil {
		pl.tail.next = n
	} else {
		pl.head = n
	}
	pl.tail = n
	pl.count++
	pl.TotalQty += n.contributed
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
	// What this node actually contributed, NOT order.RemainingQty: a filled order is
	// removed with RemainingQty already at zero.
	pl.TotalQty -= n.contributed
	n.contributed = 0
	n.prev, n.next = nil, nil
}

func (pl *PriceLevel) isEmpty() bool { return pl.count == 0 }

// Count is how many orders the level believes it holds.
//
// It is the MAINTAINED count, incremented by push and decremented by unlink, not a
// walk of the list — which is the whole reason it is exported. Comparing it against
// an independent count of the orders actually there is what catches the two drifting
// apart; deriving it from the list would compare a number with itself.
func (pl *PriceLevel) Count() int { return pl.count }

// OrderBook is a single-symbol CLOB.
type OrderBook struct {
	mu             sync.RWMutex
	symbol         string
	bids           ladder      // best (highest) last
	asks           ladder      // best (lowest) last
	nodes          *orderIndex // orderID -> node, for O(1) cancel
	perUser        *userTable  // resting-order count per interned account
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
	nd.order, nd.level, nd.prev, nd.next, nd.user, nd.contributed = nil, nil, nil, nil, 0, 0
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
	// IndexHint is the order index's initial size; it grows past it as needed.
	// 0 => MaxOrders, which is right for one book sized to its cap and wrong for
	// thousands of books that each need a high cap and mostly hold few orders.
	IndexHint int
	// Prealloc fills the book's pools for this many resting orders at
	// construction: nodes and index entries in two contiguous slabs, and the index
	// sized so it does not grow before then. A pool, not a cap: past it the book
	// allocates as usual, and MaxOrders still caps it. 0 allocates nothing ahead.
	Prealloc int
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
	if config.IndexHint <= 0 {
		config.IndexHint = config.MaxOrders
	}
	if need := config.Prealloc*100/maxLoad + 1; config.Prealloc > 0 && config.IndexHint < need {
		config.IndexHint = need
	}
	ob := &OrderBook{
		symbol:    config.Symbol,
		bids:      ladder{sign: 1},
		asks:      ladder{sign: -1},
		nodes:     newOrderIndex(config.IndexHint),
		perUser:   newUserTable(),
		maxOrders: config.MaxOrders,
		clock:     config.Clock,
	}
	ob.prefill(config.Prealloc)
	return ob
}

// prefill puts n nodes, n index entries and up to 1,024 price levels on the free
// lists, each kind from one allocation, and sizes the level maps and price slices
// for those levels.
func (ob *OrderBook) prefill(n int) {
	if n <= 0 {
		return
	}
	nodes := make([]node, n)
	ob.nodePool = make([]*node, n, n+n/4)
	for i := range nodes {
		ob.nodePool[i] = &nodes[i]
	}
	ob.nodes.prefill(n)
	levels := make([]PriceLevel, min(n, 1024))
	ob.levelPool = make([]*PriceLevel, len(levels))
	for i := range levels {
		ob.levelPool[i] = &levels[i]
	}
	// The ladders, sized for the same levels, so the first thousand prices do not
	// grow them either.
	ob.bids.levels = make([]*PriceLevel, 0, len(levels))
	ob.asks.levels = make([]*PriceLevel, 0, len(levels))
	ob.bids.keys = make([]int64, 0, len(levels))
	ob.asks.keys = make([]int64, 0, len(levels))
}

// Symbol returns the book's instrument symbol.
func (ob *OrderBook) Symbol() string { return ob.symbol }

// side is the ladder for one side of the book.
func (ob *OrderBook) side(s types.Side) *ladder {
	if s == types.SideBuy {
		return &ob.bids
	}
	return &ob.asks
}

// Add inserts a resting order. An order that arrives without an id (ID==0) is
// assigned a monotonic one. Duplicate ids are ignored; a full book returns
// ErrOrderBookFull.
func (ob *OrderBook) Add(order *types.Order) error {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	if order.ID != 0 {
		if _, exists := ob.nodes.get(order.ID); exists {
			return nil
		}
	}

	if ob.nodes.len() >= ob.maxOrders {
		return types.ErrOrderBookFull
	}

	if order.ID == 0 {
		ob.orderSeq++
		order.ID = ob.orderSeq
	}
	if _, exists := ob.nodes.get(order.ID); exists {
		return nil
	}
	ob.sequenceNum++

	lad := ob.side(order.Side)
	var level *PriceLevel
	if i, ok := lad.search(order.Price); ok {
		level = lad.levels[i]
	} else {
		level = ob.getLevel(order.Price)
		lad.insertAt(i, level)
	}

	n := ob.getNode(order, level)
	level.push(n)
	n.user = ob.perUser.intern(order.UserID)
	ob.nodes.put(order.ID, n)
	ob.perUser.incr(n.user)
	return nil
}

// Remove deletes an order by id in O(1) (plus O(P) only when it empties its
// level, which cleans up the price slice), and returns the order.
func (ob *OrderBook) Remove(orderID int64) (*types.Order, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	n, exists := ob.nodes.get(orderID)
	if !exists {
		return nil, types.ErrOrderNotFound
	}
	order := n.order
	level := n.level
	level.unlink(n)
	ob.nodes.del(orderID)
	ob.perUser.decr(n.user)
	ob.putNode(n)

	if level.isEmpty() {
		ob.side(order.Side).remove(level)
		ob.putLevel(level)
	}
	return order, nil
}

// Get returns a resting order by id.
func (ob *OrderBook) Get(orderID int64) (*types.Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	n, ok := ob.nodes.get(orderID)
	if !ok {
		return nil, false
	}
	return n.order, true
}

// FrontOrder returns the highest-priority resting order on a side — the head of the
// best price level — in O(1).
//
// It exists for the auction uncross, which repeatedly needs "the next order that
// should trade". Deriving that from Orders() would allocate and walk the whole book on
// every fill, turning an uncross of a large book into an O(book^2) operation at exactly
// the moment a venue is trying to open.
func (ob *OrderBook) FrontOrder(side types.Side) (*types.Order, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	lad := ob.side(side)
	for i := 0; i < lad.len(); i++ {
		if l := lad.at(i); l.head != nil {
			return l.head.order, true
		}
	}
	return nil, false
}

// BestBid returns the highest bid price (ticks) and its aggregate quantity (lots).
func (ob *OrderBook) BestBid() (price, qty int64, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	l := ob.bids.best()
	if l == nil {
		return 0, 0, false
	}
	return l.Price, l.TotalQty, true
}

// BestAsk returns the lowest ask price (ticks) and its aggregate quantity (lots).
func (ob *OrderBook) BestAsk() (price, qty int64, ok bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	l := ob.asks.best()
	if l == nil {
		return 0, 0, false
	}
	return l.Price, l.TotalQty, true
}

// PeekBestBidOrder returns the front (oldest) order at the best bid without
// removing it, or nil.
func (ob *OrderBook) PeekBestBidOrder() *types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	level := ob.bids.best()
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
	level := ob.asks.best()
	if level == nil || level.head == nil {
		return nil
	}
	return level.head.order
}

// Spread returns best ask − best bid (ticks), or ok=false if either side is empty.
func (ob *OrderBook) Spread() (int64, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	bid, ask := ob.bids.best(), ob.asks.best()
	if bid == nil || ask == nil {
		return 0, false
	}
	return ask.Price - bid.Price, true
}

// MidPrice returns (best bid + best ask) / 2 in ticks (floored), or ok=false if
// either side is empty.
func (ob *OrderBook) MidPrice() (int64, bool) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	bid, ask := ob.bids.best(), ob.asks.best()
	if bid == nil || ask == nil {
		return 0, false
	}
	return (bid.Price + ask.Price) / 2, true
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
	levels := make([]*PriceLevel, 0, max(0, min(depth, ob.bids.len())))
	for i := 0; i < ob.bids.len() && i < depth; i++ {
		l := ob.bids.at(i)
		// count travels with the copy: it is maintained state a caller can compare
		// against the orders it can see, and a copy that dropped it silently reported
		// every level as empty.
		levels = append(levels, &PriceLevel{Price: l.Price, TotalQty: l.TotalQty, count: l.count})
	}
	return levels
}

// GetAskLevels returns up to depth aggregated ask levels, best first (copies).
func (ob *OrderBook) GetAskLevels(depth int) []*PriceLevel {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	levels := make([]*PriceLevel, 0, max(0, min(depth, ob.asks.len())))
	for i := 0; i < ob.asks.len() && i < depth; i++ {
		l := ob.asks.at(i)
		levels = append(levels, &PriceLevel{Price: l.Price, TotalQty: l.TotalQty, count: l.count})
	}
	return levels
}

// GetOrdersAtPrice returns a copy of the FIFO order queue at a given side/price
// (ticks).
func (ob *OrderBook) GetOrdersAtPrice(side types.Side, price int64) []*types.Order {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	level := ob.side(side).get(price)
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
	if n, ok := ob.nodes.get(orderID); ok {
		n.level.TotalQty -= filledQty
		n.contributed -= filledQty
	}
}

// RestoreOrderQuantity adds qty back to the aggregate total at a resting order's
// level, undoing a prior UpdateOrderQuantity (used by FOK reversal). O(1).
func (ob *OrderBook) RestoreOrderQuantity(orderID int64, qty int64) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if n, ok := ob.nodes.get(orderID); ok {
		n.level.TotalQty += qty
		n.contributed += qty
	}
}

// Count returns the number of resting orders.
func (ob *OrderBook) Count() int {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.nodes.len()
}

// OrdersByUser returns how many resting orders belong to userID — the basis for a
// per-account open-order cap. Maintained in O(1) on Add/Remove, so it survives
// fills (which remove through Remove) and rebuilds correctly on snapshot restore
// (which re-Adds each order).
func (ob *OrderBook) OrdersByUser(userID string) int {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	return ob.perUser.countOf(userID)
}

// DepthWithin returns the total resting quantity (lots) across both sides at
// prices in the inclusive tick range [lo, hi] — how much real liquidity backs a
// price region. Used to reject a mark that no depth supports.
func (ob *OrderBook) DepthWithin(lo, hi int64) int64 {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	var total int64
	for _, lad := range [...]*ladder{&ob.bids, &ob.asks} {
		for _, lvl := range lad.levels {
			if lvl.Price >= lo && lvl.Price <= hi {
				total += lvl.TotalQty
			}
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
	out := make([]*types.Order, 0, ob.nodes.len())
	appendSide := func(lad *ladder) {
		for i := 0; i < lad.len(); i++ {
			for n := lad.at(i).head; n != nil; n = n.next {
				out = append(out, n.order)
			}
		}
	}
	appendSide(&ob.bids)
	appendSide(&ob.asks)
	return out
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
	appendSide := func(lad *ladder) []L3Order {
		out := make([]L3Order, 0, depth)
		for i := 0; i < lad.len() && i < depth; i++ {
			for n := lad.at(i).head; n != nil; n = n.next {
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
	s.Bids = appendSide(&ob.bids)
	s.Asks = appendSide(&ob.asks)
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
	for i := 0; i < ob.bids.len() && i < depth; i++ {
		l := ob.bids.at(i)
		s.Bids = append(s.Bids, SnapshotLevel{Price: l.Price, Quantity: l.TotalQty})
	}
	for i := 0; i < ob.asks.len() && i < depth; i++ {
		l := ob.asks.at(i)
		s.Asks = append(s.Asks, SnapshotLevel{Price: l.Price, Quantity: l.TotalQty})
	}
	return s
}
