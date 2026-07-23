package fix

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

func TestNewOrderRoundTrip(t *testing.T) {

	msg := Message{
		35: "D",
		11: "order123",
		55: "BTC-USD",
		54: "1",
		38: "10",
		44: "50000",
	}

	order, err := DecodeNewOrder(msg)

	if err != nil {
		t.Fatal(err)
	}

	if order.ClientOrderID != "order123" {
		t.Fail()
	}

	if order.Symbol != "BTC-USD" {
		t.Fail()
	}

	if order.Side != "BUY" {
		t.Fail()
	}
}
func TestEncoderAccepted(t *testing.T) {
	encoder := &Encoder{}

	encoder.OnEvents([]matching.Event{
		{
			Seq:     1,
			Kind:    matching.EventAccepted,
			OrderID: 10,
		},
	})

	if len(encoder.Reports) != 1 {
		t.Fatal("expected one report")
	}

	if encoder.Reports[0].Status != "ACCEPTED" {
		t.Fatal("wrong status")
	}
}
