package fix

import (
	"github.com/intrepidkarthi/orderbook/pkg/matching"
)

type ExecutionReport struct {
	Seq           int64
	OrderID       int64
	ClientOrderID string
	Status        string
	FilledQty     int64
	Price         int64
}

type Encoder struct {
	Reports []ExecutionReport
}

var _ matching.EventSink = (*Encoder)(nil)

func (e *Encoder) OnEvents(events []matching.Event) {
	for _, event := range events {

		report := ExecutionReport{
			Seq:     event.Seq,
			OrderID: event.OrderID,
		}

		switch event.Kind {

		case matching.EventAccepted:
			report.Status = "ACCEPTED"

			if event.Order != nil {
				report.ClientOrderID = event.Order.ClientOrderID
				report.Price = event.Order.Price
			}

		case matching.EventTrade:
			report.Status = "FILLED"

			if event.Trade != nil {
				report.FilledQty = event.Trade.Quantity
				report.Price = event.Trade.Price
			}

		case matching.EventCanceled:
			report.Status = "CANCELED"

			if event.Order != nil {
				report.ClientOrderID = event.Order.ClientOrderID
			}

		case matching.EventRejected:
			report.Status = "REJECTED"

			if event.Order != nil {
				report.ClientOrderID = event.Order.ClientOrderID
			}
		}

		e.Reports = append(e.Reports, report)
	}
}
