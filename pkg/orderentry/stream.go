// Package orderentry turns the engine's event stream into per-account outbound
// streams that a network session can serve, including to a client that was
// disconnected when the events happened.
//
// It is pure logic: no bytes and no sockets. Encoding lives in internal/wire and
// the server in cmd/obgw, so this package can be tested without either.
//
// # Why Stream outlives Session
//
// A Session is a connection: transient, and gone the moment a client drops. A
// Stream is an account's outbound sequence: durable for the life of the venue.
// Keeping them separate is what makes resume possible at all. If outbound events
// were owned by the connection, a maker whose resting order filled while its TCP
// connection was down would simply never learn about the fill — the single worst
// failure an order-entry system can have, because the client's position is now
// wrong and it does not know.
package orderentry

import (
	"errors"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

var (
	// ErrNoSuchStream is returned when a client asks to resume a stream this
	// venue incarnation does not have.
	ErrNoSuchStream = errors.New("orderentry: unknown stream")
	// ErrSequenceEvicted is returned when a client asks to resume from a point
	// that has already been dropped from the bounded buffer. It is deliberately
	// distinct from ErrNoSuchStream: the client must resynchronise out of band
	// rather than assume it is up to date.
	ErrSequenceEvicted = errors.New("orderentry: requested sequence no longer retained")
)

// MsgKind is the kind of an outbound message, mirroring the wire's message types
// without depending on them — this package must stay encoding-agnostic.
type MsgKind uint8

const (
	KindAccepted MsgKind = iota + 1
	KindRejected
	KindExecuted
	KindCanceled
	KindReplaced
)

// Msg is one outbound message on an account's stream. It is a value: everything
// it needs is copied out of the engine's event at publish time, because
// matching.Event holds pointers into engine-owned state that the matching
// goroutine keeps mutating after OnEvents returns.
type Msg struct {
	Seq       uint64 // dense, per-stream, starting at 1
	Kind      MsgKind
	ClOrdID   string
	Price     int64
	Quantity  int64
	LeavesQty int64
	Side      types.Side
	Aggressor types.Side
	Reason    uint16
}

// Stream is one account's outbound sequence, with a bounded ring of recent
// messages for resume.
//
// The ring is bounded on purpose: an unbounded buffer turns a client that never
// reconnects into a memory leak, which is a venue-wide failure caused by one
// participant. When it overflows the oldest messages are dropped and a resume
// request for them is refused explicitly, so the client learns it is behind
// rather than silently receiving a gap.
type Stream struct {
	mu      sync.Mutex
	account string
	seq     uint64
	ring    []Msg
	start   uint64 // sequence of ring[0]; 0 when empty
	max     int
}

func newStream(account string, max int) *Stream {
	if max <= 0 {
		max = 4096
	}
	return &Stream{account: account, ring: make([]Msg, 0, max), max: max}
}

// Account returns the owning account.
func (s *Stream) Account() string { return s.account }

// Seq returns the highest sequence assigned so far.
func (s *Stream) Seq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// append stamps and stores a message, evicting the oldest when full.
func (s *Stream) append(m Msg) Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	m.Seq = s.seq
	if len(s.ring) == 0 {
		s.start = m.Seq
	}
	s.ring = append(s.ring, m)
	if len(s.ring) > s.max {
		drop := len(s.ring) - s.max
		s.ring = append(s.ring[:0], s.ring[drop:]...)
		s.start += uint64(drop)
	}
	return m
}

// Since returns every retained message after seq, oldest first. Passing 0 returns
// everything still retained.
//
// A client resuming from a sequence older than what is retained gets
// ErrSequenceEvicted rather than a truncated slice: handing back a partial
// history that looks complete is how a client ends up with a wrong position and
// no way to detect it.
func (s *Stream) Since(seq uint64) ([]Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if seq > s.seq {
		// Ahead of the venue: the client is claiming messages that do not exist,
		// which means it is talking to a different incarnation.
		return nil, ErrSequenceEvicted
	}
	if len(s.ring) == 0 {
		return nil, nil
	}
	if seq+1 < s.start {
		return nil, ErrSequenceEvicted
	}
	idx := int(seq + 1 - s.start)
	if idx >= len(s.ring) {
		return nil, nil
	}
	out := make([]Msg, len(s.ring)-idx)
	copy(out, s.ring[idx:])
	return out, nil
}

// Registry owns every account's stream for one venue incarnation.
//
// Incarnation is the load-bearing idea: sequence numbers are only meaningful
// within one run of the venue. A restart mints a new incarnation id, so a client
// resuming with a stale cursor is refused rather than served different content
// under numbers it believes it already has — the failure that would otherwise be
// completely invisible to both sides.
type Registry struct {
	mu      sync.RWMutex
	streams map[string]*Stream
	orders  map[int64]*live // engine order id -> owner, client id, remaining

	// The naming index has its own lock, and that separation is the point.
	//
	// It is written on the MATCHING goroutine, by NameIndex, the instant the engine
	// accepts an order — while everything else in this type is maintained by the pump
	// goroutine, asynchronously, whenever it gets there. Sharing mu would put the
	// matcher behind the pump's delivery work, which is precisely what the pump exists
	// to avoid.
	//
	// It used to be written by the pump like everything else, and that was wrong in a
	// way no test caught and no benchmark could: a client's cancel resolves through
	// this map, so a pump running even a fraction of a second behind would tell a
	// client that its live resting order did not exist. The client, correctly, would
	// not ask again — and the order stayed in the book, uncancellable by anybody, for
	// the life of the venue. Measured at 12,843 orphaned orders in thirty seconds at
	// 10,000 messages a second; invisible below about 4,000. See docs/SOAK.md.
	nameMu sync.RWMutex
	// Nested rather than keyed on account+"\x00"+clOrdID, for two reasons.
	//
	// The composite key had to be built on every write, which is a string
	// concatenation — 16 B and one allocation per accepted order, on the matching
	// goroutine, in an engine whose hot path is measured against runtime.MemStats and
	// claims to allocate nothing. (Reads were already free: the compiler elides the
	// concatenation for a map lookup, but not for an assignment.)
	//
	// And the account scoping is the security boundary — without it one client could
	// name another's order by guessing an identifier as ordinary as "1" — so it is
	// better expressed as the shape of the data than as a convention about how a
	// string was built.
	byClOrd map[string]map[string]int64 // account -> client id -> engine order id

	incarnation string
	ringSize    int
}

// NewRegistry builds a registry for one venue incarnation. The incarnation id is
// supplied rather than generated so it can be derived from something meaningful
// and stay testable.
func NewRegistry(incarnation string, ringSize int) *Registry {
	return &Registry{
		streams:     map[string]*Stream{},
		orders:      map[int64]*live{},
		byClOrd:     map[string]map[string]int64{},
		incarnation: incarnation,
		ringSize:    ringSize,
	}
}

// Incarnation returns this venue run's identifier.
func (r *Registry) Incarnation() string { return r.incarnation }

// Stream returns the account's stream, creating it on first use.
func (r *Registry) Stream(account string) *Stream {
	r.mu.RLock()
	s, ok := r.streams[account]
	r.mu.RUnlock()
	if ok {
		return s
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok = r.streams[account]; ok {
		return s
	}
	s = newStream(account, r.ringSize)
	r.streams[account] = s
	return s
}

// Resume validates a client's cursor against this incarnation and returns what it
// missed.
func (r *Registry) Resume(incarnation, account string, afterSeq uint64) ([]Msg, error) {
	if incarnation != r.incarnation {
		return nil, ErrNoSuchStream
	}
	return r.Stream(account).Since(afterSeq)
}

// live tracks what the publisher needs to know about an order that the trade
// event itself does not carry: whose it is, the client's own id for it, and how
// much is left.
//
// This shadow table is only trustworthy because the engine's event stream is
// proven to reconstruct per-order state (TestEventStreamReconstructsBook). Before
// that proof it would have drifted permanently out of step — an iceberg reload or
// a self-trade-prevention decrement would have moved the book without telling
// anyone, and every later fill of that order would have been misattributed.
type live struct {
	account   string
	clOrdID   string
	remaining int64
}

// Publish fans one engine event batch out to the accounts it concerns. It is
// called from the pump goroutine, never from the matching goroutine.
func (r *Registry) Publish(evs []matching.Event) {
	r.mu.Lock()
	if r.orders == nil {
		r.orders = map[int64]*live{}
	}
	r.mu.Unlock()

	for i := range evs {
		e := &evs[i]
		switch e.Kind {
		case matching.EventAccepted:
			if e.Order == nil {
				continue
			}
			r.track(e.Order)
			r.deliver(e.Order.UserID, Msg{
				Kind: KindAccepted, ClOrdID: e.Order.ClientOrderID,
				Price: e.Order.Price, Quantity: e.Order.Quantity, Side: e.Order.Side,
			})

		case matching.EventRejected:
			if e.Order == nil {
				continue
			}
			r.deliver(e.Order.UserID, Msg{
				Kind: KindRejected, ClOrdID: e.Order.ClientOrderID, Reason: ReasonFor(e.Reason),
			})

		case matching.EventCanceled:
			if e.Order == nil {
				continue
			}
			r.deliver(e.Order.UserID, Msg{
				Kind: KindCanceled, ClOrdID: e.Order.ClientOrderID, Reason: ReasonFor(e.Reason),
			})
			r.forget(e.Order)

		case matching.EventReplaced:
			if e.Order == nil {
				continue
			}
			r.resize(e.Order.ID, e.Order.RemainingQty)
			r.deliver(e.Order.UserID, Msg{
				Kind: KindReplaced, ClOrdID: e.Order.ClientOrderID, LeavesQty: e.Order.RemainingQty,
			})

		case matching.EventTrade:
			r.publishTrade(e.Trade)
		}
	}
}

// publishTrade tells both sides of a fill. The maker learns of a fill it did not
// initiate, which is precisely the message a disconnected client must not lose.
func (r *Registry) publishTrade(t *types.Trade) {
	if t == nil {
		return
	}
	for _, id := range [2]int64{t.BuyOrderID, t.SellOrderID} {
		if id == 0 {
			continue
		}
		acct, clOrdID, leaves, ok := r.fill(id, t.Quantity)
		if !ok {
			// A trade against an order the publisher never saw accepted. With a
			// complete event stream this does not happen; if it ever does, the
			// stream has regressed and dropping the message silently would hide it.
			continue
		}
		r.deliver(acct, Msg{
			Kind: KindExecuted, ClOrdID: clOrdID,
			Price: t.Price, Quantity: t.Quantity, LeavesQty: leaves,
			Aggressor: t.TakerSide,
		})
	}
}

func (r *Registry) track(o *types.Order) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.orders == nil {
		r.orders = map[int64]*live{}
	}
	// An iceberg reload re-announces the same id; keep one entry and reset its
	// remaining to the new slice rather than accumulating.
	r.orders[o.ID] = &live{account: o.UserID, clOrdID: o.ClientOrderID, remaining: o.Quantity}
	// The name is NOT written here. Name owns it, and it has already run — on the
	// matching goroutine, before this event was ever queued.
}

// Name records that an order can be addressed by its client's own identifier.
//
// It is called from NameIndex on the matching goroutine and does nothing else, so
// the critical section is one map write. Everything expensive about publishing —
// encoding, fanning out, ring maintenance — stays on the pump where it belongs.
func (r *Registry) Name(o *types.Order) {
	if o == nil || o.ClientOrderID == "" {
		return
	}
	r.nameMu.Lock()
	r.nameLocked(o.UserID, o.ClientOrderID, o.ID)
	r.nameMu.Unlock()
}

// nameLocked records one name. Callers hold nameMu.
func (r *Registry) nameLocked(account, clOrdID string, id int64) {
	m := r.byClOrd[account]
	if m == nil {
		m = map[string]int64{}
		r.byClOrd[account] = m
	}
	m[clOrdID] = id
}

// Adopt seeds the registry from a recovered book, so orders that outlived a
// restart can still be named and still generate execution reports.
//
// Recovery rebuilt the book and nothing else. Without this the registry starts
// empty against a non-empty venue, and three things follow, in increasing order of
// severity: a client cannot cancel a recovered order, it cannot reduce one, and —
// worst — when one of those orders fills, `publishTrade` finds no entry for it and
// drops the execution report entirely. A maker whose resting order filled while the
// venue was down would never be told. That is the failure the whole stream-outlives-
// the-connection design exists to prevent, reintroduced through the back door.
//
// It delivers nothing to any stream. These orders were accepted in a previous
// incarnation and acknowledged there; re-announcing them now would replay history
// into a fresh sequence space, and a client cannot resume across incarnations
// anyway. Adoption restores the index, not the conversation.
//
// remaining is seeded from RemainingQty rather than Quantity, which is the whole
// subtlety: track() may use Quantity because at accept time nothing is filled, but
// a recovered order can be partially filled, and fill() decrements from this
// number. Seeding it with the original size would report a LeavesQty too high by
// exactly what was already filled before the restart.
//
// Call it once, after recovery and before serving.
func (r *Registry) Adopt(orders []*types.Order) {
	r.mu.Lock()
	if r.orders == nil {
		r.orders = map[int64]*live{}
	}
	for _, o := range orders {
		if o == nil {
			continue
		}
		r.orders[o.ID] = &live{account: o.UserID, clOrdID: o.ClientOrderID, remaining: o.RemainingQty}
	}
	r.mu.Unlock()

	r.nameMu.Lock()
	for _, o := range orders {
		if o != nil && o.ClientOrderID != "" {
			r.nameLocked(o.UserID, o.ClientOrderID, o.ID)
		}
	}
	r.nameMu.Unlock()
}

// OrderIDFor resolves a client's own order id to the engine's, within that
// client's account. It is how a cancel arriving over the wire names an order
// without the wire ever carrying an engine id or an account.
func (r *Registry) OrderIDFor(account, clOrdID string) (int64, bool) {
	r.nameMu.RLock()
	defer r.nameMu.RUnlock()
	id, ok := r.byClOrd[account][clOrdID]
	return id, ok
}

// Lock order: mu is never held while nameMu is taken, and nameMu is never held
// while mu is taken. The two maps are now owned by different goroutines — orders by
// the pump, byClOrd by the matcher — and every path that touches both does so in
// that sequence, one at a time.

// forget removes an order from the shadow table and drops its name.
//
// The identifier comes from the EVENT, not from the shadow table, and that is not a
// simplification — it is required. The two maps are no longer written together: naming
// is synchronous and cannot be missed, while the shadow table is maintained by the
// pump, whose queue discards its oldest batches under backpressure. So an order can be
// named and never tracked, and a forget that looked the identifier up in the shadow
// table would find nothing and leave the name behind for the life of the venue.
//
// A dropped batch is a worse problem than a leaked map entry — it is a lost execution
// report — and obgw_publisher_dropped_total exists so it is not a silent one. But a
// leak that only appears after another failure is still a leak, and this one arrived
// with the fix for something worse, which is the usual way.
func (r *Registry) forget(o *types.Order) {
	if o == nil {
		return
	}
	r.mu.Lock()
	delete(r.orders, o.ID)
	r.mu.Unlock()

	r.unname(o.UserID, o.ClientOrderID, o.ID)
}

// unname drops a name, but only if it still points at the order being forgotten.
//
// Forgetting runs on the pump and naming runs on the matcher, so by the time this
// arrives the client may already have reused the identifier for a new order.
// Deleting unconditionally would unname the live one on the strength of a dead
// one's cancellation — the same orphaning bug the split was made to fix, wearing a
// different hat.
func (r *Registry) unname(account, clOrdID string, id int64) {
	if clOrdID == "" {
		return
	}
	r.nameMu.Lock()
	if m := r.byClOrd[account]; m != nil {
		if cur, ok := m[clOrdID]; ok && cur == id {
			delete(m, clOrdID)
		}
		// The per-account map is left in place. An account that has traded once will
		// trade again, and the alternative is churning a map allocation every time a
		// participant's book empties — which for a market maker is constantly.
	}
	r.nameMu.Unlock()
}

func (r *Registry) resize(id, remaining int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.orders[id]; ok {
		l.remaining = remaining
	}
}

// fill decrements an order and reports what is left, forgetting it once it is
// exhausted so the table stays bounded by the live book rather than by history.
func (r *Registry) fill(id, qty int64) (account, clOrdID string, leaves int64, ok bool) {
	r.mu.Lock()
	l, ok := r.orders[id]
	if !ok {
		r.mu.Unlock()
		return "", "", 0, false
	}
	l.remaining -= qty
	if l.remaining < 0 {
		l.remaining = 0
	}
	account, clOrdID, leaves = l.account, l.clOrdID, l.remaining
	exhausted := l.remaining == 0
	if exhausted {
		delete(r.orders, id)
	}
	r.mu.Unlock()

	// Outside mu. This used to delete from byClOrd while holding mu, which was correct
	// when one lock covered both maps and became a concurrent map write the moment the
	// matcher started naming orders under its own. Neither the race detector nor 480
	// tests reached it; thirty seconds of soak crashed the process on it.
	//
	// One case this cannot close, stated rather than left to be discovered: an order
	// whose acceptance batch the pump dropped is named but not tracked, so the early
	// return above fires and its name survives a fill that exhausts it. Unlike a
	// cancel, a trade event carries no client identifier, so there is nothing to unname
	// it by. Closing it would mean tracking remaining quantity on the matching
	// goroutine, which is the shadow table again — a real cost on the hot path to
	// recover one map entry in a venue that has already lost an execution report.
	if exhausted {
		r.unname(account, clOrdID, id)
	}
	return account, clOrdID, leaves, true
}

func (r *Registry) deliver(account string, m Msg) {
	if account == "" {
		return
	}
	r.Stream(account).append(m)
}
