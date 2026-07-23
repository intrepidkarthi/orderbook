package fix

import (
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

func TestFIXRoundTrip(t *testing.T) {

	input := Message{
		35: "D",
		11: "order123",
		55: "BTC-USD",
		54: "1",
		38: "10",
		44: "50000",
	}

	order, err := DecodeNewOrder(input)

	if err != nil {
		t.Fatal(err)
	}

	order.ID = 100

	encoder := &Encoder{}

	encoder.OnEvents([]matching.Event{
		{
			Seq:     1,
			Kind:    matching.EventAccepted,
			OrderID: order.ID,
			Order:   order,
		},
	})

	if len(encoder.Reports) != 1 {
		t.Fatal("expected one execution report")
	}

	output := EncodeExecutionReport(encoder.Reports[0])

	if output[35] != "8" {
		t.Fatal("expected execution report")
	}

	if output[11] != "order123" {
		t.Fatal("client order id mismatch")
	}
}
func TestDecodeCancel(t *testing.T) {

	msg := Message{
		35: "F",
		41: "order123",
	}

	cancel, err := DecodeCancel(msg)

	if err != nil {
		t.Fatal(err)
	}

	if cancel.ClientOrderID != "order123" {
		t.Fatal("wrong client order id")
	}
}
