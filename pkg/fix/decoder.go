package fix

import (
	"fmt"
	"strconv"

	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func DecodeNewOrder(m Message) (*types.Order, error) {
	if m[35] != "D" {
		return nil, fmt.Errorf("not NewOrderSingle")
	}

	var side types.Side

	switch m[54] {
	case "1":
		side = types.SideBuy
	case "2":
		side = types.SideSell
	default:
		return nil, fmt.Errorf("invalid side")
	}

	o, err := types.NewOrder(
		"",
		m[55],
		side,
		types.OrderTypeLimit,
		parseInt(m[44]),
		parseInt(m[38]),
		types.TIFGoodTillCancel,
	)

	if err != nil {
		return nil, err
	}

	o.ClientOrderID = m[11]

	return o, nil
}

func parseInt(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}

	return n
}

type CancelRequest struct {
	ClientOrderID string
}

func DecodeCancel(m Message) (*CancelRequest, error) {
	if m[35] != "F" {
		return nil, fmt.Errorf("not OrderCancelRequest")
	}

	return &CancelRequest{
		ClientOrderID: m[41],
	}, nil
}
