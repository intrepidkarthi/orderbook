package types

import "time"

// Trade is a single execution between a resting maker order and an incoming
// taker order. It always prints at the maker's (resting) price. Price is in
// integer ticks and Quantity in integer lots. ID/SequenceNum are monotonic
// int64 values assigned by the engine.
type Trade struct {
	ID           int64     `json:"id"`
	Symbol       string    `json:"symbol"`
	Price        int64     `json:"price"` // ticks
	Quantity     int64     `json:"quantity"`
	BuyOrderID   int64     `json:"buy_order_id"`
	SellOrderID  int64     `json:"sell_order_id"`
	BuyerUserID  string    `json:"buyer_user_id"`
	SellerUserID string    `json:"seller_user_id"`
	MakerOrderID int64     `json:"maker_order_id"`
	TakerOrderID int64     `json:"taker_order_id"`
	TakerSide    Side      `json:"taker_side"`
	CreatedAt    time.Time `json:"created_at"`
	SequenceNum  int64     `json:"sequence_num"`
}

// NewTradeValue builds a trade value (no heap allocation) from the matched
// buy/sell orders. takerSide identifies which side crossed the spread, from which
// maker/taker order ids are derived. ID/SequenceNum are left zero and assigned by
// the engine. This is the form the zero-alloc match path appends into a buffer.
func NewTradeValue(symbol string, price, quantity int64, buyOrder, sellOrder *Order, takerSide Side) Trade {
	t := Trade{
		Symbol:       symbol,
		Price:        price,
		Quantity:     quantity,
		BuyOrderID:   buyOrder.ID,
		SellOrderID:  sellOrder.ID,
		BuyerUserID:  buyOrder.UserID,
		SellerUserID: sellOrder.UserID,
		TakerSide:    takerSide,
		CreatedAt:    time.Now().UTC(),
	}

	if takerSide == SideBuy {
		t.TakerOrderID = buyOrder.ID
		t.MakerOrderID = sellOrder.ID
	} else {
		t.TakerOrderID = sellOrder.ID
		t.MakerOrderID = buyOrder.ID
	}
	return t
}

// NewTrade is the pointer form of NewTradeValue (one heap allocation), kept for
// callers that want a *Trade.
func NewTrade(symbol string, price, quantity int64, buyOrder, sellOrder *Order, takerSide Side) *Trade {
	t := NewTradeValue(symbol, price, quantity, buyOrder, sellOrder, takerSide)
	return &t
}

// NotionalValue is Price × Quantity of the trade in tick·lot units.
func (t *Trade) NotionalValue() int64 { return t.Price * t.Quantity }

// IsSelfTrade reports whether both sides belong to the same user.
func (t *Trade) IsSelfTrade() bool { return t.BuyerUserID == t.SellerUserID }
