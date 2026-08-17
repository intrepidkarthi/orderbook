package refmatch

// The command surface. One method per thing a client or an operator can ask the
// venue to do, each returning the verdict, the prints and the ordered events that
// command produced.

// Submit runs one order through the venue.
//
// The order of operations matters and is written out rather than inherited:
//
//  1. The order is NAMED FIRST. A sequence number is consumed before any admission
//     check, so an order refused for arriving into a halted venue, or for being
//     post-only across a crossed spread, still burns an id. Order ids therefore have
//     gaps, and the gaps are correct — a model that incremented only on acceptance
//     would diverge permanently at the first rejection.
//  2. The trading state gates it.
//  3. Post-only refuses anything that would take.
//  4. It matches.
//  5. Its type and time-in-force decide what happens to the remainder.
func (m *Model) Submit(in Order) Result {
	m.begin()
	m.orderSeq++
	o := &order{
		Resting: Resting{
			ID:        composeID(m.cfg.ShardIndex, m.orderSeq),
			User:      in.User,
			Side:      in.Side,
			Price:     in.Price,
			Qty:       in.Qty,
			Remaining: in.Qty,
			PostOnly:  in.PostOnly,
		},
		Type:       in.Type,
		TIF:        in.TIF,
		STPMode:    in.STPMode,
		TradeGroup: in.TradeGroup,
		Privileged: in.Privileged,
		Status:     StatusNew,
	}
	// A market order carries no price; it takes whatever the book offers.
	if o.Type == Market {
		o.Price = 0
	}
	status, reason := m.settle(o)
	return m.compose(o, status, reason)
}

// settle applies the state gate, post-only, the matching walk and the resting
// rules, returning the verdict the client is told.
func (m *Model) settle(o *order) (Status, Reject) {
	switch m.state {
	case Halted:
		o.Status = StatusRejected
		return StatusRejected, RejectTradingHalted
	case CancelOnlyState:
		o.Status = StatusRejected
		return StatusRejected, RejectNewOrdersHalted
	}

	if o.PostOnly && m.wouldCross(o) {
		o.Status = StatusRejected
		return StatusRejected, RejectPostOnlyWouldCross
	}

	start := len(m.trades)
	if m.cfg.ProRata {
		m.matchProRata(o)
	} else {
		m.matchFIFO(o)
	}

	// A market order never rests.
	if o.Type == Market {
		if o.Remaining != 0 {
			o.Status = StatusCancelled
			if len(m.trades) == start {
				return StatusCancelled, RejectMarketNoLiquidity
			}
			return StatusCancelled, RejectNone
		}
		return StatusFilled, RejectNone
	}

	switch o.TIF {
	case IOC:
		// Whatever could not fill immediately is cancelled; it never rests.
		if o.Remaining != 0 && o.Status != StatusCancelled {
			o.Status = StatusCancelled
		}
		return o.Status, RejectNone

	case FOK:
		// All or nothing. Note the test is on the remaining quantity, so an order
		// that self-trade-prevention DECREMENTED down to nothing counts as filled
		// and is reported filled, even though it printed less than it asked for.
		if o.Remaining != 0 {
			m.reverseFrom(start)
			m.trades = m.trades[:start]
			o.Status = StatusRejected
			return StatusRejected, RejectFOKCannotFill
		}
		return StatusFilled, RejectNone

	default: // GTC
		if o.active() && o.Remaining != 0 {
			// The book-size cap is a verdict, not an admission control: an order
			// that traded and then found no room is REJECTED, and its prints still
			// stand even though the events announcing them are dropped.
			if m.count() >= m.cfg.MaxOrders {
				o.Status = StatusRejected
				return StatusRejected, RejectOrderBookFull
			}
			m.rest(o)
		}
		return o.Status, RejectNone
	}
}

// compose builds the event batch for a submitted order around the events matching
// recorded, and returns the command's result.
//
// A rejection means nothing happened, so the events recorded during the walk are
// DROPPED — which is why a rejected fill-or-kill announces no executions even
// though it burned trade ids, and why a maker that self-trade-prevention removed
// mid-walk can leave the book with no event saying so.
func (m *Model) compose(o *order, status Status, reason Reject) Result {
	var evs []Event
	if status == StatusRejected {
		m.pending = nil
		evs = append(evs, Event{Kind: EvRejected, OrderID: o.ID, User: o.User, Reason: reason})
	} else {
		evs = append(evs, Event{Kind: EvAccepted, OrderID: o.ID, User: o.User})
	}
	evs = append(evs, m.pending...)
	m.pending = nil
	// An order that ends cancelled was announced as accepted and never rested — an
	// IOC or market remainder, or a taker self-trade-prevention cancelled. Without a
	// matching delete a consumer reconstructs it as live forever.
	if status == StatusCancelled {
		evs = append(evs, Event{Kind: EvCanceled, OrderID: o.ID, User: o.User})
	}
	return Result{Verdict: Verdict{Status: status, Reason: reason}, Trades: m.trades, Events: evs}
}

// Cancel removes a resting order if it belongs to userID and is still live.
//
// A cancel that finds nothing emits no event at all, which is what makes a tape
// deletion-closed: deleting the submit that created an order leaves the later
// cancel naming an id nothing issued, and both sides agree that is a plain
// "not found".
func (m *Model) Cancel(id int64, user string) Result {
	m.begin()
	if r := m.cancelOne(id, user); r != RejectNone {
		return Result{Verdict: Verdict{Status: StatusRejected, Reason: r}}
	}
	return Result{Verdict: Verdict{Status: StatusCancelled}, Events: m.flush()}
}

// cancelOne is the body of a cancel, shared with Replace.
func (m *Model) cancelOne(id int64, user string) Reject {
	o := m.find(id)
	if o == nil {
		return RejectOrderNotFound
	}
	// Same answer as a missing order: a probe must not be able to tell "not yours"
	// from "does not exist".
	if o.User != user {
		return RejectOrderNotFound
	}
	if !o.active() {
		return RejectOrderNotActive
	}
	o.Status = StatusCancelled
	m.remove(id)
	m.emit(EvCanceled, o)
	return RejectNone
}

// Reduce shrinks a resting order in place, KEEPING its queue position. newQty is
// the new total quantity, matching how the order was submitted.
//
// Keeping the place is the whole reason this verb exists — cancel-then-new sends
// the order to the back of its level — so the assertion that catches a reduce which
// re-queues is the ranked book comparison, not membership.
func (m *Model) Reduce(id, newQty int64, user string) Result {
	m.begin()
	o := m.find(id)
	if o == nil || o.User != user {
		return Result{Verdict: Verdict{Status: StatusRejected, Reason: RejectOrderNotFound}}
	}
	if !o.active() {
		return Result{Verdict: Verdict{Status: StatusRejected, Reason: RejectOrderNotActive}}
	}
	// A reduction only: an increase or a reprice forfeits priority and belongs in a
	// replace, so it is refused here rather than silently reinterpreted. An order
	// already filled beyond the new total cannot shrink to it either.
	if newQty <= 0 || newQty >= o.Qty || newQty <= o.Filled {
		return Result{Verdict: Verdict{Status: StatusRejected, Reason: RejectInvalidQuantity}}
	}
	o.Qty = newQty
	o.Remaining = newQty - o.Filled
	m.emit(EvReplaced, o)
	return Result{Verdict: Verdict{Status: StatusReduced}, Events: m.flush()}
}

// Replace is cancel-then-new as one command, so nothing can interleave between the
// two halves.
//
// The failure semantics are asymmetric on purpose. If the original cannot be
// cancelled the replacement is NOT submitted — a client replacing an order it no
// longer holds did not ask to open a new position — and, because the replacement
// never reaches the venue, it never consumes an id either. If the cancel succeeds
// and the replacement is then refused, the client holds neither, and both halves
// are on this command's event stream.
//
// Queue priority is forfeited: the replacement enters at the back.
func (m *Model) Replace(id int64, user string, in Order) Result {
	c := m.Cancel(id, user)
	if c.Verdict.Status == StatusRejected {
		return c
	}
	r := m.Submit(in)
	return Result{
		Verdict: r.Verdict,
		Trades:  r.Trades,
		Events:  append(append([]Event{}, c.Events...), r.Events...),
	}
}

// CancelAll pulls every resting order belonging to user, in book order.
func (m *Model) CancelAll(user string) Result {
	m.begin()
	// Snapshot first: the walk removes from the slices it is reading.
	var victims []*order
	for _, o := range m.bids {
		victims = append(victims, o)
	}
	for _, o := range m.asks {
		victims = append(victims, o)
	}
	for _, o := range victims {
		if o.User != user || !o.active() {
			continue
		}
		o.Status = StatusCancelled
		m.remove(o.ID)
		m.emit(EvCanceled, o)
	}
	return Result{Verdict: Verdict{Status: StatusCancelled}, Events: m.flush()}
}

// Halt refuses everything until a resume. Announcing the TRANSITION and not the
// call is deliberate: halting an already-halted venue describes nothing, and a
// consumer counting halts would see an event that did not happen.
func (m *Model) Halt() Result {
	m.begin()
	if m.state == Halted {
		return Result{}
	}
	m.state = Halted
	return Result{Events: []Event{{Kind: EvHalted}}}
}

// SetCancelOnly accepts cancels and refuses new liquidity. Its own event kind
// rather than a halt: a subscriber told the venue is halted when it is still
// accepting cancels would draw the wrong conclusion about whether it can get out.
func (m *Model) SetCancelOnly() Result {
	m.begin()
	if m.state == CancelOnlyState {
		return Result{}
	}
	m.state = CancelOnlyState
	return Result{Events: []Event{{Kind: EvCancelOnly}}}
}

// Resume returns the venue to normal trading.
func (m *Model) Resume() Result {
	m.begin()
	if m.state == Open {
		return Result{}
	}
	m.state = Open
	return Result{Events: []Event{{Kind: EvResumed}}}
}
