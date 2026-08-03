package main

import (
	"net"
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

// feed is an ordinary market-data subscriber: it dials the venue's md port,
// speaks the wire protocol like any client would, and maintains the book the
// way docs/PROTOCOL.md says a subscriber should — snapshot, then everything
// after the snapshot's sequence on top. The dashboard consuming the same
// stream participants do is the point: it can never show an operator a book
// the venue did not publish.
//
// Reconnection is always a fresh subscribe (Seq 0). The resume path exists on
// the wire, but a dashboard holds no position and owes nothing to its history —
// rebuilding from a snapshot is simpler and always correct, and the venue's
// eviction policy never applies to a subscriber that never falls behind by
// more than a reconnect.
type feed struct {
	addr string

	mu        sync.Mutex
	connected bool
	seq       uint64
	lastMsg   time.Time
	lastTrade int64
	state     byte // wire.MDStateOpen etc.; 0 = unknown
	bids      map[int64]int64
	asks      map[int64]int64
	trades    []feedTrade // newest first, capped
	indic     *feedIndicative
}

type feedTrade struct {
	Seq       uint64 `json:"seq"`
	Price     int64  `json:"price"`
	Qty       int64  `json:"qty"`
	Aggressor string `json:"aggressor"`
}

type feedIndicative struct {
	Price     int64 `json:"price"`
	Volume    int64 `json:"volume"`
	Imbalance int64 `json:"imbalance"`
}

const tradeRing = 30

func newFeed(addr string) *feed {
	return &feed{
		addr: addr,
		bids: map[int64]int64{},
		asks: map[int64]int64{},
	}
}

// run dials, subscribes, applies until the connection dies, and starts over.
// Backoff is bounded: an operator's dashboard should be back within seconds of
// the venue being back, and hammering a dead address costs nobody anything at
// one attempt per few seconds.
func (f *feed) run() {
	backoff := time.Second
	for {
		if err := f.session(); err == nil {
			backoff = time.Second
		}
		time.Sleep(backoff)
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

func (f *feed) session() error {
	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	sub, err := wire.EncodeMDSubscribe(nil, wire.MDSubscribe{Version: wire.Version})
	if err != nil {
		return err
	}
	if err := wire.WritePacket(conn, wire.PacketUnsequenced, sub); err != nil {
		return err
	}

	// A fresh subscription starts with a fresh book: the snapshot about to
	// arrive is the whole truth, and stale levels from the last session must
	// not survive underneath it.
	f.mu.Lock()
	f.bids = map[int64]int64{}
	f.asks = map[int64]int64{}
	f.connected = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.connected = false
		f.mu.Unlock()
	}()

	buf := make([]byte, wire.MaxPayload)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		pkt, err := wire.ReadPacket(conn, buf)
		if err != nil {
			return err
		}
		f.apply(pkt.Payload)
	}
}

// apply is one payload into the subscriber's view — the same switch the
// protocol's own conformance tests run, kept as a method so the tests here can
// drive it with wire-synthesized bytes and no socket.
func (f *feed) apply(payload []byte) {
	if len(payload) < 2 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMsg = time.Now()

	switch payload[0] {
	case wire.MsgMDLevel:
		l, err := wire.DecodeMDLevel(payload)
		if err != nil {
			return
		}
		f.side(l.Side)[l.Price] = l.Qty
	case wire.MsgMDSnapshotEnd:
		e, err := wire.DecodeMDSnapshotEnd(payload)
		if err != nil {
			return
		}
		f.seq = e.Seq
		f.lastTrade = e.LastTradePrice
	case wire.MsgMDDelta:
		d, err := wire.DecodeMDDelta(payload)
		if err != nil {
			return
		}
		// Qty is the level's NEW total — absolute, so a dropped delta heals on
		// the next touch of the level instead of corrupting the book forever.
		if d.Qty == 0 {
			delete(f.side(d.Side), d.Price)
		} else {
			f.side(d.Side)[d.Price] = d.Qty
		}
		f.seq = d.Seq
	case wire.MsgMDTrade:
		tr, err := wire.DecodeMDTrade(payload)
		if err != nil {
			return
		}
		f.lastTrade = tr.Price
		f.seq = tr.Seq
		agg := "buy"
		if tr.Aggressor == wire.SideSell {
			agg = "sell"
		}
		f.trades = append([]feedTrade{{Seq: tr.Seq, Price: tr.Price, Qty: tr.Qty, Aggressor: agg}}, f.trades...)
		if len(f.trades) > tradeRing {
			f.trades = f.trades[:tradeRing]
		}
	case wire.MsgMDStatus:
		st, err := wire.DecodeMDStatus(payload)
		if err != nil {
			return
		}
		f.state = st.State
		f.seq = st.Seq
	case wire.MsgMDIndicative:
		in, err := wire.DecodeMDIndicative(payload)
		if err != nil {
			return
		}
		f.indic = &feedIndicative{Price: in.Price, Volume: in.Volume, Imbalance: in.Imbalance}
		f.seq = in.Seq
	}
}

func (f *feed) side(s uint8) map[int64]int64 {
	if s == wire.SideSell {
		return f.asks
	}
	return f.bids
}

// level is one aggregated price level, in ticks and lots as the wire carries
// them — display conversion is the page's job, with the factors the operator
// configured.
type level struct {
	Price int64 `json:"price"`
	Qty   int64 `json:"qty"`
}

// snapshot is the JSON-ready copy of the subscriber's view.
type snapshot struct {
	Connected bool            `json:"connected"`
	StaleMs   int64           `json:"stale_ms"` // since the last md packet
	Seq       uint64          `json:"seq"`
	State     string          `json:"state"`
	LastTrade int64           `json:"last_trade"`
	Bids      []level         `json:"bids"` // best first
	Asks      []level         `json:"asks"` // best first
	Trades    []feedTrade     `json:"trades"`
	Indic     *feedIndicative `json:"indicative,omitempty"`
}

func (f *feed) snapshot(depth int) snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := snapshot{
		Connected: f.connected,
		Seq:       f.seq,
		LastTrade: f.lastTrade,
		Bids:      topLevels(f.bids, depth, false),
		Asks:      topLevels(f.asks, depth, true),
		Trades:    append([]feedTrade(nil), f.trades...),
		Indic:     f.indic,
	}
	if !f.lastMsg.IsZero() {
		out.StaleMs = time.Since(f.lastMsg).Milliseconds()
	}
	switch f.state {
	case wire.MDStateOpen:
		out.State = "open"
	case wire.MDStateHalted:
		out.State = "halted"
	case wire.MDStateCancelOnly:
		out.State = "cancel-only"
	default:
		out.State = "unknown"
	}
	return out
}

func topLevels(m map[int64]int64, depth int, ascending bool) []level {
	out := make([]level, 0, len(m))
	for p, q := range m {
		out = append(out, level{Price: p, Qty: q})
	}
	// Insertion sort: depth is small and the map is the aggregated book, not
	// the order flow — clarity over asymptotics here.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			less := out[j].Price < out[j-1].Price
			if !ascending {
				less = out[j].Price > out[j-1].Price
			}
			if !less {
				break
			}
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > depth {
		out = out[:depth]
	}
	return out
}
