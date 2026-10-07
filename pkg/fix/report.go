package fix

import (
	"errors"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Out receives each outgoing message with the user it is addressed to, which a
// session layer turns into a TargetCompID and a connection.
type Out func(user string, msg []byte)

// ReporterConfig sets what goes in every outgoing header.
type ReporterConfig struct {
	SenderCompID string
	Instrument   types.Instrument
	// Clock stamps SendingTime (52) and TransactTime (60); nil means time.Now.
	Clock func() time.Time
}

// Reporter is a matching.EventSink that sends an ExecutionReport (8) for each
// order an event affects. It keeps each order's quantities itself, from the events
// in sequence, because an event's *Order shows the order at publication time.
type Reporter struct {
	cfg     ReporterConfig
	out     Out
	orders  map[int64]*orderState
	cancels map[int64]cancelRef // engine order id -> the cancel request in flight
	execID  int64
	seq     map[string]int64 // MsgSeqNum (34) per user, a stand-in until a session owns it
}

type orderState struct {
	clOrdID string
	user    string
	side    types.Side
	price   int64 // ticks; 0 for a market order
	qty     int64
	cum     int64
	value   decimal.Decimal // sum of price * qty over fills, in ticks * lots
}

type cancelRef struct{ clOrdID, origClOrdID string }

// NewReporter returns a reporter that sends through out.
func NewReporter(cfg ReporterConfig, out Out) *Reporter {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Reporter{cfg: cfg, out: out, orders: map[int64]*orderState{},
		cancels: map[int64]cancelRef{}, seq: map[string]int64{}}
}

// OnEvents implements matching.EventSink.
func (r *Reporter) OnEvents(evs []matching.Event) {
	for i := range evs {
		e := &evs[i]
		switch e.Kind {
		case matching.EventAccepted:
			o := e.Order
			st := &orderState{clOrdID: o.ClientOrderID, user: o.UserID, side: o.Side, qty: o.Quantity}
			if o.Type == types.OrderTypeLimit {
				st.price = o.Price
			}
			r.orders[e.OrderID] = st
			r.report(e.OrderID, st, "0", "0", nil, "")
		case matching.EventTrade:
			t := e.Trade
			for _, id := range []int64{t.MakerOrderID, t.TakerOrderID} {
				st := r.orders[id]
				if st == nil {
					continue
				}
				st.cum += t.Quantity
				st.value = st.value.Add(decimal.NewFromInt(t.Price).Mul(decimal.NewFromInt(t.Quantity)))
				status := "1"
				if st.cum == st.qty {
					status = "2"
				}
				r.report(id, st, "F", status, t, "")
				if status == "2" {
					delete(r.orders, id)
				}
			}
		case matching.EventCanceled:
			st := r.orders[e.OrderID]
			if st == nil {
				continue
			}
			text := ""
			if e.Reason != nil {
				text = e.Reason.Error()
			}
			r.report(e.OrderID, st, "4", "4", nil, text)
			delete(r.orders, e.OrderID)
			delete(r.cancels, e.OrderID)
		case matching.EventRejected:
			o := e.Order
			st := &orderState{clOrdID: o.ClientOrderID, user: o.UserID, side: o.Side, qty: o.Quantity, price: o.Price}
			r.reject(e.OrderID, st, e.Reason)
		}
	}
}

// reject sends an ExecutionReport rejecting an order the engine never held.
func (r *Reporter) reject(orderID int64, st *orderState, reason error) {
	b := r.header("8", st.user)
	b.Add(37, orderIDField(orderID)).Add(11, st.clOrdID).Add(17, r.nextExecID()).
		Add(150, "8").Add(39, "8")
	r.body(b, st)
	b.AddInt(151, 0).Add(14, "0").Add(6, "0")
	b.AddInt(103, int64(ordRejReason(reason)))
	if reason != nil {
		b.Add(58, reason.Error())
	}
	r.out(st.user, b.Bytes())
}

func (r *Reporter) report(orderID int64, st *orderState, execType, status string, t *types.Trade, text string) {
	b := r.header("8", st.user)
	b.AddInt(37, orderID)
	if c, ok := r.cancels[orderID]; ok && execType == "4" {
		b.Add(11, c.clOrdID).Add(41, c.origClOrdID)
	} else {
		b.Add(11, st.clOrdID)
	}
	b.Add(17, r.nextExecID()).Add(150, execType).Add(39, status)
	r.body(b, st)
	if t != nil {
		b.Add(31, r.px(t.Price)).Add(32, r.qty(t.Quantity))
	}
	leaves := st.qty - st.cum
	if execType == "4" {
		leaves = 0
	}
	b.Add(151, r.qty(leaves)).Add(14, r.qty(st.cum)).Add(6, r.avgPx(st))
	if text != "" {
		b.Add(58, text)
	}
	r.out(st.user, b.Bytes())
}

func (r *Reporter) header(msgType, user string) *Builder {
	r.seq[user]++
	return NewBuilder(msgType).Add(49, r.cfg.SenderCompID).Add(56, user).
		AddInt(34, r.seq[user]).Add(52, r.cfg.Clock().UTC().Format("20060102-15:04:05.000"))
}

func (r *Reporter) body(b *Builder, st *orderState) {
	side := "1"
	if st.side == types.SideSell {
		side = "2"
	}
	b.Add(55, r.cfg.Instrument.Symbol).Add(54, side).Add(38, r.qty(st.qty))
	if st.price != 0 {
		b.Add(44, r.px(st.price))
	}
}

func (r *Reporter) nextExecID() string {
	r.execID++
	return strconv.FormatInt(r.execID, 10)
}

func (r *Reporter) px(ticks int64) string { return r.cfg.Instrument.TicksToPrice(ticks).String() }
func (r *Reporter) qty(lots int64) string { return r.cfg.Instrument.LotsToQty(lots).String() }

func (r *Reporter) avgPx(st *orderState) string {
	if st.cum == 0 {
		return "0"
	}
	return st.value.Div(decimal.NewFromInt(st.cum)).Mul(r.cfg.Instrument.TickSize).Round(8).String()
}

// ordRejReason maps an engine refusal onto FIX OrdRejReason (103), through
// pkg/orderentry's mapping so the two protocols cannot drift apart.
func ordRejReason(err error) int {
	if errors.Is(err, types.ErrNoSessionClose) {
		return 11 // unsupported order characteristic: DAY without a session
	}
	switch orderentry.ReasonFor(err) {
	case orderentry.ReasonDuplicateClOrd:
		return 6
	case orderentry.ReasonTooLarge:
		return 3
	case orderentry.ReasonTooSmall, orderentry.ReasonInvalidQuantity:
		return 13
	case orderentry.ReasonHalted:
		return 2
	case orderentry.ReasonUnknownOrder:
		return 5
	}
	return 99
}

// Entry feeds decoded client messages to the engine. It keeps the ClOrdID to
// order-id map a cancel needs, and answers a cancel the engine refuses with an
// OrderCancelReject (9), because a refused cancel publishes no event.
type Entry struct {
	e   *matching.Engine
	rep *Reporter
	ids map[string]int64 // user + "\x00" + ClOrdID -> engine order id
	buf []types.Trade
}

// NewEntry joins an engine to the reporter attached as its EventSink.
func NewEntry(e *matching.Engine, rep *Reporter) *Entry {
	return &Entry{e: e, rep: rep, ids: map[string]int64{}}
}

// Handle parses one framed message and acts on it. It returns an error for a
// message it cannot act on (bad framing, a bad field, an unsupported MsgType),
// which a session layer answers with a Reject (3). Refusals by the engine are not
// errors: they are reports.
func (en *Entry) Handle(raw []byte) error {
	m, _, err := Parse(raw)
	if err != nil {
		return err
	}
	switch m.Type() {
	case "D":
		o, err := DecodeNewOrderSingle(m, en.rep.cfg.Instrument)
		if err != nil {
			return err
		}
		key := o.UserID + "\x00" + o.ClientOrderID
		if _, seen := en.ids[key]; seen {
			en.rep.reject(0, &orderState{clOrdID: o.ClientOrderID, user: o.UserID, side: o.Side, qty: o.Quantity, price: o.Price},
				types.ErrDuplicateClientOrderID)
			return nil
		}
		en.buf, _, _ = en.e.Match(o, en.buf[:0])
		if o.ID != 0 {
			en.ids[key] = o.ID
		}
		return nil
	case "F":
		c, err := DecodeOrderCancelRequest(m)
		if err != nil {
			return err
		}
		id, ok := en.ids[c.User+"\x00"+c.OrigClOrdID]
		if !ok {
			en.cancelReject(c, 0, "1", "unknown order")
			return nil
		}
		en.rep.cancels[id] = cancelRef{c.ClOrdID, c.OrigClOrdID}
		if _, err := en.e.Cancel(id, c.User); err != nil {
			delete(en.rep.cancels, id)
			en.cancelReject(c, id, "0", err.Error()) // known, but no longer working
		}
		return nil
	}
	return &FieldError{35, "MsgType " + strconv.Quote(m.Type()) + " is not D or F"}
}

func (en *Entry) cancelReject(c CancelRequest, orderID int64, reason, text string) {
	b := en.rep.header("9", c.User)
	b.Add(37, orderIDField(orderID)).Add(11, c.ClOrdID).Add(41, c.OrigClOrdID).Add(39, "8").
		Add(434, "1").Add(102, reason).Add(58, text)
	en.rep.out(c.User, b.Bytes())
}

// orderIDField is OrderID (37): the engine's id, or NONE for an order the engine
// never assigned one, as FIX requires the field either way.
func orderIDField(id int64) string {
	if id == 0 {
		return "NONE"
	}
	return strconv.FormatInt(id, 10)
}
