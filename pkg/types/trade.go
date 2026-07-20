package types

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Trade is a single execution between a resting maker order and an incoming
// taker order. It always prints at the maker's (resting) price.
type Trade struct {
	ID           string          `json:"id"`
	Symbol       string          `json:"symbol"`
	Price        decimal.Decimal `json:"price"`
	Quantity     decimal.Decimal `json:"quantity"`
	BuyOrderID   string          `json:"buy_order_id"`
	SellOrderID  string          `json:"sell_order_id"`
	BuyerUserID  string          `json:"buyer_user_id"`
	SellerUserID string          `json:"seller_user_id"`
	MakerOrderID string          `json:"maker_order_id"`
	TakerOrderID string          `json:"taker_order_id"`
	TakerSide    Side            `json:"taker_side"`
	CreatedAt    time.Time       `json:"created_at"`
	SequenceNum  uint64          `json:"sequence_num"`
}

// NewTrade builds a trade from the matched buy/sell orders. takerSide identifies
// which side crossed the spread, from which maker/taker order ids are derived.
func NewTrade(symbol string, price, quantity decimal.Decimal, buyOrder, sellOrder *Order, takerSide Side) *Trade {
	t := &Trade{
		ID:           uuid.Must(uuid.NewV7()).String(),
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

// NotionalValue is Price × Quantity of the trade.
func (t *Trade) NotionalValue() decimal.Decimal { return t.Price.Mul(t.Quantity) }

// IsSelfTrade reports whether both sides belong to the same user.
func (t *Trade) IsSelfTrade() bool { return t.BuyerUserID == t.SellerUserID }
