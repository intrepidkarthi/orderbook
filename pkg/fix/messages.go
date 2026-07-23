package fix

type Message map[int]string

func EncodeExecutionReport(r ExecutionReport) Message {

	return Message{
		35: "8",
		11: r.ClientOrderID,
		37: stringValue(r.OrderID),
		39: r.Status,
		14: stringValue(r.FilledQty),
		44: stringValue(r.Price),
	}
}
