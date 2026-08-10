package marketdata

import (
	"errors"
	"sync"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Feed is the venue's public market-data stream: one sequenced broadcast of book
// deltas and trade prints, with snapshot-plus-delta recovery for a subscriber that
// joins late or falls behind.
//
// # Why this is not pkg/orderentry
//
// The two feeds look similar and are not. Order entry is private and per-account:
// every account has its own stream, and a client that falls behind must be given the
// messages it missed, because an execution report it never sees is a position it does
// not know it has. Market data is public and identical for everyone, and a subscriber
// that falls behind can simply be told the current state — nothing is owed to it
// personally.
//
// That difference is the whole design. There is one stream rather than a registry of
// them, and the answer to "I am too far behind" is a fresh snapshot instead of a
// refusal.
//
// # The guarantee, stated so it can be falsified
//
// For one venue incarnation, the sequence is dense and gap-free from 1, and:
//
//	Snapshot(at Seq S) + every Update with Seq > S == the engine's current book
//
// A subscriber can therefore start anywhere: take a snapshot, apply what follows,
// and be exactly right. TestSnapshotPlusDeltasEqualsTheBook asserts it after every
// command of a random tape.
//
// The subtlety that makes it true is that Snapshot captures the book *and* the
// sequence under one lock. Reading them separately is the same bug the order-entry
// side shipped in v0.12.0, where a report claimed to be consistent with a sequence
// the subscriber had not reached — and a client applying both then counted the same
// change twice.
//
// # Incarnation
//
// Sequence numbers mean something only within one run of the venue. A restart mints a
// new incarnation id, so a subscriber resuming with a stale cursor is refused rather
// than served different content under numbers it believes it already has. This is the
// same reasoning as the order-entry side, and the same failure it prevents.
type Feed struct {
	mu          sync.Mutex
	incarnation string
	seq         uint64
	ring        []Update
	start       uint64 // sequence of ring[0]; 0 when empty
	max         int

	// book is the current aggregated state, maintained from the same events, so a
	// snapshot can be minted at any point without asking the engine.
	book *L2Feed
	// lastTradePrice is carried in a snapshot because a subscriber joining mid-session
	// needs it to render anything, and it is not derivable from the depth alone.
	lastTradePrice int64
}

// ErrSequenceEvicted is returned when a subscriber asks for updates older than the
// feed still retains.
//
// It is not an error the subscriber should retry: the correct response is to take a
// fresh Snapshot and continue from its sequence. Returning it explicitly rather than
// handing back a truncated slice is deliberate — a partial answer that looks complete
// is how a subscriber ends up silently missing a price level forever.
var ErrSequenceEvicted = errors.New("marketdata: requested sequence no longer retained")

// ErrWrongIncarnation is returned when a subscriber resumes against a different run
// of the venue.
var ErrWrongIncarnation = errors.New("marketdata: cursor belongs to another incarnation")

// UpdateKind distinguishes what an Update carries.
type UpdateKind uint8

const (
	// UpdateBookDelta is an aggregated level change: Qty is the level's new total,
	// and zero means the level is gone.
	UpdateBookDelta UpdateKind = iota + 1
	// UpdateTrade is a print: price, quantity and which side took liquidity.
	UpdateTrade
	// UpdateStatus is a venue state change (halted, resumed), which a subscriber
	// needs in the same ordered stream as the data it qualifies.
	UpdateStatus
	// UpdateIndicative is what an auction would clear at if it uncrossed now, with
	// the imbalance left over. Published during an accumulating phase so participants
	// can react before the price is fixed — an auction that revealed nothing until it
	// printed would be a sealed-bid auction, a different market design.
	UpdateIndicative
	// UpdateBust annuls an earlier print, naming it by TradeID. It carries no book
	// change and none is implied: the book moved on long before the bust arrived,
	// and a subscriber that rewinds its own depth on one of these will diverge from
	// the venue. Adjust the tape, not the book. See docs/TRADE-BUST.md §2.
	UpdateBust
)

// Update is one sequenced market-data message.
//
// One struct with a Kind rather than an interface: the feed is a hot broadcast path
// and every subscriber decodes every message, so a type switch on a byte beats an
// allocation and a dynamic dispatch. Which fields are meaningful is documented per
// kind rather than enforced by the type, which is the trade this makes.
type Update struct {
	Seq  uint64
	Kind UpdateKind

	// UpdateBookDelta
	Side  types.Side
	Price int64
	Qty   int64 // the level's NEW total; 0 means the level is gone

	// UpdateTrade
	TradePrice int64
	TradeQty   int64
	Aggressor  types.Side
	// TradeID names the print — set on UpdateTrade and on the UpdateBust that
	// annuls it, and it is the only thing tying the two together. A feed that
	// published prints anonymously could report a bust and leave every subscriber
	// guessing which of its fills was meant; that was the state of this struct
	// until trade bust needed it.
	TradeID int64

	// UpdateBust
	BustReason string

	// UpdateStatus
	State VenueState

	// UpdateIndicative
	IndicativePrice  int64
	IndicativeVolume int64
	// Imbalance is buy minus sell interest at the indicative price, in lots. Positive
	// means more to buy than to sell.
	Imbalance int64
}

// VenueState is the trading state a UpdateStatus reports.
//
// Cancel-only is distinct from halted rather than folded into it: a subscriber told
// the venue is halted when it is actually still accepting cancels would draw the
// wrong conclusion about whether it can get out of a position.
type VenueState uint8

const (
	StateOpen VenueState = iota
	StateHalted
	StateCancelOnly
)

func (s VenueState) String() string {
	switch s {
	case StateHalted:
		return "HALTED"
	case StateCancelOnly:
		return "CANCEL_ONLY"
	default:
		return "OPEN"
	}
}

// Snapshot is the book as of a specific sequence — the starting point for a
// subscriber that has none, or one that has fallen too far behind.
//
// Seq is the load-bearing field: everything with a greater sequence applies on top of
// this, and nothing with a lesser or equal one does.
type Snapshot struct {
	Incarnation    string
	Seq            uint64
	Bids           []L2Delta // best first
	Asks           []L2Delta // best first
	LastTradePrice int64
}

// NewFeed builds a feed. retain bounds the gap-fill ring: a subscriber further behind
// than this is told to take a fresh snapshot rather than being served a partial
// answer. An unbounded ring would turn one stalled subscriber into a venue-wide
// memory leak, which is the same reason the order-entry ring is bounded.
func NewFeed(incarnation string, retain int) *Feed {
	if retain <= 0 {
		retain = 65536
	}
	return &Feed{
		incarnation: incarnation,
		ring:        make([]Update, 0, retain),
		max:         retain,
		book:        NewL2Feed(),
	}
}

// Adopt seeds the feed from a recovered book, so a subscriber's first snapshot shows
// the venue as it actually is.
//
// Without it the feed starts empty against a non-empty book, and every level appears
// to be new the first time it changes — a subscriber would see a book that is missing
// everything nobody has touched since the restart, which is most of it. Exactly the
// bug recovery had on the order-entry side before v0.12.0, where the session layer's
// index was left empty against a recovered book.
//
// It publishes nothing. These levels are not changes; they are the starting state, and
// a subscriber gets them from the snapshot rather than as a burst of deltas describing
// a book that was already there.
//
// Call once, after recovery and before serving.
func (f *Feed) Adopt(orders []*types.Order, lastTradePrice int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.book.Adopt(orders)
	f.lastTradePrice = lastTradePrice
}

// Incarnation identifies this run of the venue.
func (f *Feed) Incarnation() string { return f.incarnation }

// Seq is the highest sequence published so far.
func (f *Feed) Seq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq
}

// OnEvents implements matching.EventSink. It derives the level changes, records the
// prints, and publishes both in one ordered stream.
//
// Deltas precede the trade that caused them within a batch, because a subscriber
// applying them in order should see the book reach its post-trade state and then be
// told what printed — the reverse would briefly show a trade against depth that is
// still displayed.
func (f *Feed) OnEvents(evs []matching.Event) {
	// One lock for the whole batch. The L2 aggregation and the sequence assignment
	// have to be atomic together, or a Snapshot taken concurrently could capture a
	// book that includes a change whose Update has not been sequenced yet — and a
	// subscriber would then apply that change twice.
	f.mu.Lock()
	defer f.mu.Unlock()

	f.book.OnEvents(evs)
	for _, d := range f.book.Drain() {
		f.publishLocked(Update{Kind: UpdateBookDelta, Side: d.Side, Price: d.Price, Qty: d.Qty})
	}
	for i := range evs {
		e := &evs[i]
		switch e.Kind {
		case matching.EventTrade:
			if e.Trade == nil {
				continue
			}
			f.lastTradePrice = e.Trade.Price
			f.publishLocked(Update{
				Kind: UpdateTrade, TradePrice: e.Trade.Price,
				TradeQty: e.Trade.Quantity, Aggressor: e.Trade.TakerSide,
				TradeID: e.Trade.ID,
			})
		case matching.EventBusted:
			// No book change, and lastTradePrice is deliberately left alone: the
			// engine does not rewind its own reference price on a bust, and a feed
			// that rewound its copy would be reporting a market the venue is not
			// running. See docs/TRADE-BUST.md §2.3.
			f.publishLocked(Update{
				Kind: UpdateBust, TradeID: e.TradeID, BustReason: e.BustReason,
			})
		case matching.EventHalted:
			f.publishLocked(Update{Kind: UpdateStatus, State: StateHalted})
		case matching.EventCancelOnly:
			f.publishLocked(Update{Kind: UpdateStatus, State: StateCancelOnly})
		case matching.EventResumed:
			f.publishLocked(Update{Kind: UpdateStatus, State: StateOpen})
		}
	}
}

// publishLocked assigns the next sequence and appends, evicting the oldest update
// once the ring is full. Callers hold f.mu.
func (f *Feed) publishLocked(u Update) {
	f.seq++
	u.Seq = f.seq
	if len(f.ring) == f.max {
		copy(f.ring, f.ring[1:])
		f.ring = f.ring[:len(f.ring)-1]
		f.start++
	}
	if len(f.ring) == 0 {
		f.start = u.Seq
	}
	f.ring = append(f.ring, u)
}

// Since returns every update after seq, for a subscriber filling a gap.
//
// A subscriber that is up to date gets an empty slice and no error. One asking for
// something already evicted gets ErrSequenceEvicted and should take a fresh Snapshot.
// One claiming to be ahead of the feed is out of step and is refused rather than
// quietly served nothing, which would leave it stuck forever.
func (f *Feed) Since(seq uint64) ([]Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq > f.seq {
		return nil, ErrSequenceEvicted
	}
	if seq == f.seq {
		return nil, nil
	}
	if len(f.ring) == 0 || seq+1 < f.start {
		return nil, ErrSequenceEvicted
	}
	idx := int(seq + 1 - f.start)
	if idx < 0 || idx > len(f.ring) {
		return nil, ErrSequenceEvicted
	}
	out := make([]Update, len(f.ring)-idx)
	copy(out, f.ring[idx:])
	return out, nil
}

// Resume is Since with an incarnation check, which is what a subscriber reconnecting
// across a possible venue restart should call.
func (f *Feed) Resume(incarnation string, seq uint64) ([]Update, error) {
	if incarnation != f.incarnation {
		return nil, ErrWrongIncarnation
	}
	return f.Since(seq)
}

// Snapshot returns the current book and the sequence it is consistent with.
//
// The two are read under one lock, which is the entire point: everything after
// Snapshot.Seq applies on top of it, and nothing at or before it does. Taking the
// book and the sequence separately would let an update land in between and be applied
// twice — once folded into the snapshot, once again from the stream.
func (f *Feed) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Snapshot{
		Incarnation:    f.incarnation,
		Seq:            f.seq,
		Bids:           f.book.Levels(types.SideBuy),
		Asks:           f.book.Levels(types.SideSell),
		LastTradePrice: f.lastTradePrice,
	}
}

// PublishIndicative announces what an auction would currently clear at.
//
// The venue calls this on whatever cadence it chooses, rather than the feed deriving
// it from every order. During an auction the indicative price moves on essentially
// every message, and broadcasting that would be several times the traffic of the
// order flow itself for information nobody can act on at that granularity — real
// venues publish on a timer, and the timer is a venue decision.
//
// It takes a sequence like any other update, so it is ordered against the deltas
// around it and a subscriber resuming mid-auction gets it in the right place.
func (f *Feed) PublishIndicative(price, volume, imbalance int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishLocked(Update{
		Kind: UpdateIndicative, IndicativePrice: price,
		IndicativeVolume: volume, Imbalance: imbalance,
	})
}

// Retained reports the oldest sequence still available for gap-fill, and how many
// updates are held. A publisher can use it to decide whether to offer a subscriber a
// gap-fill or a fresh snapshot before it asks.
func (f *Feed) Retained() (oldest uint64, count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ring) == 0 {
		return 0, 0
	}
	return f.start, len(f.ring)
}
