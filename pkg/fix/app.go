package fix

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// ErrWrongType means a decoder was given a message of another MsgType.
var ErrWrongType = errors.New("fix: wrong MsgType")

// FieldError refuses a message for one field's value. A session layer answers it
// with a Reject (3) naming Tag.
type FieldError struct {
	Tag    int
	Reason string
}

func (e *FieldError) Error() string { return fmt.Sprintf("fix: tag %d: %s", e.Tag, e.Reason) }

func required(m Message, tag int) (string, error) {
	v, ok := m.Get(tag)
	if !ok || v == "" {
		return "", &FieldError{tag, "required field missing"}
	}
	return v, nil
}

// owner is Account (1), or SenderCompID (49) without one.
func owner(m Message) (string, error) {
	if v, ok := m.Get(1); ok && v != "" {
		return v, nil
	}
	return required(m, 49)
}

func side(m Message) (types.Side, error) {
	v, err := required(m, 54)
	if err != nil {
		return "", err
	}
	switch v {
	case "1":
		return types.SideBuy, nil
	case "2":
		return types.SideSell, nil
	}
	return "", &FieldError{54, fmt.Sprintf("side %q is not 1 (buy) or 2 (sell)", v)}
}

// onGrid converts a decimal string to a whole number of steps, refusing anything
// that is not exactly on the grid. Rounding would trade at a value nobody sent.
func onGrid(tag int, v string, step decimal.Decimal) (int64, error) {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return 0, &FieldError{tag, fmt.Sprintf("%q is not a decimal", v)}
	}
	n := d.Div(step)
	if !n.Equal(n.Truncate(0)) {
		return 0, &FieldError{tag, fmt.Sprintf("%s is not a multiple of %s", v, step)}
	}
	if !n.IsPositive() {
		return 0, &FieldError{tag, fmt.Sprintf("%s is not positive", v)}
	}
	return n.IntPart(), nil
}

// DecodeNewOrderSingle turns a NewOrderSingle (D) into an order for the
// instrument. The order's ClientOrderID is ClOrdID (11), its user is Account (1)
// or SenderCompID (49).
func DecodeNewOrderSingle(m Message, in types.Instrument) (*types.Order, error) {
	if t := m.Type(); t != "D" {
		return nil, fmt.Errorf("%w: %q, not NewOrderSingle (D)", ErrWrongType, t)
	}
	clOrdID, err := required(m, 11)
	if err != nil {
		return nil, err
	}
	user, err := owner(m)
	if err != nil {
		return nil, err
	}
	sym, err := required(m, 55)
	if err != nil {
		return nil, err
	}
	if sym != in.Symbol {
		return nil, &FieldError{55, fmt.Sprintf("symbol %q is not %q", sym, in.Symbol)}
	}
	s, err := side(m)
	if err != nil {
		return nil, err
	}
	qv, err := required(m, 38)
	if err != nil {
		return nil, err
	}
	qty, err := onGrid(38, qv, in.LotSize)
	if err != nil {
		return nil, err
	}
	ov, err := required(m, 40)
	if err != nil {
		return nil, err
	}
	var typ types.OrderType
	var price int64
	switch ov {
	case "1":
		typ = types.OrderTypeMarket
	case "2":
		typ = types.OrderTypeLimit
		pv, err := required(m, 44)
		if err != nil {
			return nil, err
		}
		if price, err = onGrid(44, pv, in.TickSize); err != nil {
			return nil, err
		}
	default:
		return nil, &FieldError{40, fmt.Sprintf("OrdType %q is not 1 (market) or 2 (limit)", ov)}
	}
	tif := types.TIFDay
	if v, ok := m.Get(59); ok {
		switch v {
		case "0":
		case "1":
			tif = types.TIFGoodTillCancel
		case "3":
			tif = types.TIFImmediateOrCancel
		case "4":
			tif = types.TIFFillOrKill
		default:
			return nil, &FieldError{59, fmt.Sprintf("TimeInForce %q is not 0, 1, 3 or 4", v)}
		}
	}
	o, err := types.NewOrder(user, in.Symbol, s, typ, price, qty, tif)
	if err != nil {
		return nil, err
	}
	o.ClientOrderID = clOrdID
	if v, ok := m.Get(18); ok && strings.ContainsRune(v, '6') {
		o.PostOnly = true
	}
	return o, nil
}

// CancelRequest is a decoded OrderCancelRequest (F).
type CancelRequest struct {
	ClOrdID     string // this request's id
	OrigClOrdID string // the order to cancel
	User        string
	Symbol      string
	Side        types.Side
}

// DecodeOrderCancelRequest decodes an OrderCancelRequest (F).
func DecodeOrderCancelRequest(m Message) (CancelRequest, error) {
	if t := m.Type(); t != "F" {
		return CancelRequest{}, fmt.Errorf("%w: %q, not OrderCancelRequest (F)", ErrWrongType, t)
	}
	var r CancelRequest
	var err error
	if r.ClOrdID, err = required(m, 11); err != nil {
		return r, err
	}
	if r.OrigClOrdID, err = required(m, 41); err != nil {
		return r, err
	}
	if r.User, err = owner(m); err != nil {
		return r, err
	}
	if r.Symbol, err = required(m, 55); err != nil {
		return r, err
	}
	if r.Side, err = side(m); err != nil {
		return r, err
	}
	return r, nil
}
