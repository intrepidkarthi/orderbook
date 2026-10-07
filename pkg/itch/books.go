package itch

import (
	"bytes"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Config selects what Books builds.
type Config struct {
	// Symbols, if non-empty, are the only stocks a book is built for. Messages for
	// any other stock are counted as skipped.
	Symbols []string
	// MaxOrders caps each book (0 means 1<<24). IndexHint is each book's initial
	// index size (0 means 1024), so thousands of books stay small until they fill.
	MaxOrders, IndexHint int
}

// Stats counts what Books has seen. Anomalies are counted, not fatal: a file cut
// mid-day refers to orders added before it starts.
type Stats struct {
	Messages     uint64
	ByType       [256]uint64
	NotHeld      uint64 // E, C, X, D or U naming an order no book holds
	OverExecuted uint64 // an execution or cancel larger than the order's remaining shares
	BookFull     uint64 // an add refused because its book was at MaxOrders
	Skipped      uint64 // messages for stocks not selected
}

// Books rebuilds one order book per stock from ITCH messages. Not safe for
// concurrent use; an ITCH stream is sequential.
type Books struct {
	cfg      Config
	selected map[string]bool
	byLocate [1 << 16]*orderbook.OrderBook
	skip     [1 << 16]bool
	bySymbol map[string]*orderbook.OrderBook
	free     []*types.Order
	Stats    Stats
}

// NewBooks returns an empty set of books.
func NewBooks(cfg Config) *Books {
	if cfg.MaxOrders == 0 {
		cfg.MaxOrders = 1 << 24
	}
	if cfg.IndexHint == 0 {
		cfg.IndexHint = 1024
	}
	b := &Books{cfg: cfg, bySymbol: map[string]*orderbook.OrderBook{}}
	if len(cfg.Symbols) > 0 {
		b.selected = map[string]bool{}
		for _, s := range cfg.Symbols {
			b.selected[s] = true
		}
	}
	return b
}

// Book returns the book for a stock, or nil if none was built.
func (b *Books) Book(symbol string) *orderbook.OrderBook { return b.bySymbol[symbol] }

// Symbols returns the stocks a book was built for, in no particular order.
func (b *Books) Symbols() []string {
	out := make([]string, 0, len(b.bySymbol))
	for s := range b.bySymbol {
		out = append(out, s)
	}
	return out
}

// Apply applies one message.
func (b *Books) Apply(m *Message) {
	b.Stats.Messages++
	b.Stats.ByType[m.Type]++
	if !Decoded(m.Type) {
		return
	}
	if b.skip[m.Locate] {
		b.Stats.Skipped++
		return
	}
	book := b.byLocate[m.Locate]
	switch m.Type {
	case 'A', 'F':
		if book == nil {
			if book = b.open(m); book == nil {
				b.Stats.Skipped++
				return
			}
		}
		side := types.SideBuy
		if m.Side == 'S' {
			side = types.SideSell
		}
		b.add(book, m.Ref, side, m.Price, m.Shares)
	case 'E', 'C', 'X':
		if book == nil {
			b.Stats.NotHeld++
			return
		}
		o, ok := book.Get(int64(m.Ref))
		if !ok {
			b.Stats.NotHeld++
			return
		}
		n := int64(m.Shares)
		if n >= o.RemainingQty {
			if n > o.RemainingQty {
				b.Stats.OverExecuted++
			}
			b.remove(book, m.Ref)
			return
		}
		if m.Type == 'X' {
			o.Quantity -= n
		} else {
			o.FilledQty += n
		}
		o.RemainingQty -= n
		book.UpdateOrderQuantity(o.ID, n)
	case 'D':
		if book == nil || !b.remove(book, m.Ref) {
			b.Stats.NotHeld++
		}
	case 'U':
		if book == nil {
			b.Stats.NotHeld++
			return
		}
		o, ok := book.Get(int64(m.Ref))
		if !ok {
			b.Stats.NotHeld++
			return
		}
		side := o.Side
		b.remove(book, m.Ref)
		// A replace loses priority: the new reference joins the back of its level.
		b.add(book, m.NewRef, side, m.Price, m.Shares)
	}
}

// open creates the book for an add's stock, or marks the locate skipped.
func (b *Books) open(m *Message) *orderbook.OrderBook {
	sym := string(bytes.TrimRight(m.Stock[:], " "))
	if b.selected != nil && !b.selected[sym] {
		b.skip[m.Locate] = true
		return nil
	}
	book := b.bySymbol[sym]
	if book == nil {
		book = orderbook.New(orderbook.Config{Symbol: sym, MaxOrders: b.cfg.MaxOrders, IndexHint: b.cfg.IndexHint})
		b.bySymbol[sym] = book
	}
	b.byLocate[m.Locate] = book
	return book
}

func (b *Books) add(book *orderbook.OrderBook, ref uint64, side types.Side, price, shares uint32) {
	var o *types.Order
	if n := len(b.free); n > 0 {
		o = b.free[n-1]
		b.free = b.free[:n-1]
		*o = types.Order{}
	} else {
		o = &types.Order{}
	}
	o.ID = int64(ref)
	o.Symbol = book.Symbol()
	o.Side = side
	o.Type = types.OrderTypeLimit
	o.Price = int64(price)
	o.Quantity = int64(shares)
	o.RemainingQty = int64(shares)
	o.Status = types.OrderStatusNew
	if err := book.Add(o); err != nil {
		b.Stats.BookFull++
		b.free = append(b.free, o)
	}
}

func (b *Books) remove(book *orderbook.OrderBook, ref uint64) bool {
	o, err := book.Remove(int64(ref))
	if err != nil {
		return false
	}
	b.free = append(b.free, o)
	return true
}
