// Command multisymbol is a two-book venue: the smallest thing that can be wrong
// in the ways docs/MULTI-SYMBOL.md cares about.
//
// It exists for the reason examples/replication exists. The multi-symbol seams —
// partitioned ids, a durable manifest, one command log per shard, and a
// market-data subscription that names its instrument — are each plausible on the
// page, and this repository's record with seams claimed but never consumed is
// documented and bad. So: build the consumer that would notice if any of them were
// wrong.
//
// What it is not: cmd/obgw. The reference gateway serves one instrument and
// deliberately still does — converting it is a larger change to the most-tested
// component here, and doing that in the same pass as the protocol would mean
// debugging both at once. This is the proof the wire and the core compose; the
// gateway catching up is its own arc.
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Venue is a set of symbols, each an independent shard with its own book, its own
// command log and its own market-data feed.
//
// There is no venue-wide sequence and no venue-wide "as of" instant, by design —
// buying one would need a serialisation point every command passes through, which
// is the bottleneck sharding exists to remove (MULTI-SYMBOL §2). The only thing
// shared across symbols is the id space, and it is *partitioned* rather than
// centralised precisely so that sharing costs no coordination.
type Venue struct {
	manifest *matching.Manifest
	shards   *matching.Shards

	mu      sync.RWMutex
	feeds   map[string]*marketdata.Feed
	writers map[string]*wal.Writer

	incarnation string
	ln          net.Listener
	quit        chan struct{}
	wg          sync.WaitGroup
}

// VenueConfig configures a Venue.
type VenueConfig struct {
	Symbols     []string
	Dir         string // manifest and per-symbol logs live here
	Incarnation string
	MDAddr      string
	MDRetain    int
	QueueSize   int
}

// NewVenue opens (or reopens) every symbol and starts the market-data listener.
func NewVenue(cfg VenueConfig) (*Venue, error) {
	if len(cfg.Symbols) == 0 {
		return nil, errors.New("multisymbol: no symbols")
	}
	if cfg.MDRetain <= 0 {
		cfg.MDRetain = 1 << 16
	}

	man, err := matching.LoadManifest(cfg.Dir + "/venue.json")
	if err != nil {
		return nil, fmt.Errorf("multisymbol: manifest: %w", err)
	}
	v := &Venue{
		manifest:    man,
		feeds:       map[string]*marketdata.Feed{},
		writers:     map[string]*wal.Writer{},
		incarnation: cfg.Incarnation,
		quit:        make(chan struct{}),
	}

	// Every shard gets its own feed and its own log. The feed is attached as the
	// shard's event sink through NewConfig, which is the seam that already existed;
	// the log goes through NewLog, which did not, and whose absence meant a sharded
	// venue could not survive a restart at all.
	v.shards = matching.NewShards(matching.ShardsConfig{
		Manifest:  man,
		QueueSize: cfg.QueueSize,
		NewConfig: func(symbol string) matching.Config {
			feed := marketdata.NewFeed(cfg.Incarnation, cfg.MDRetain)
			v.mu.Lock()
			v.feeds[symbol] = feed
			v.mu.Unlock()

			ec := matching.DefaultConfig(symbol)
			ec.DedupClientOrderIDs = 4096
			ec.EventSink = feed
			return ec
		},
		NewLog: func(symbol string) (matching.CommandLog, error) {
			w, err := wal.Open(cfg.Dir + "/" + symbol + ".wal")
			if err != nil {
				return nil, err
			}
			v.mu.Lock()
			v.writers[symbol] = w
			v.mu.Unlock()
			return w, nil
		},
	})

	// Open every symbol eagerly so a misconfigured one fails at startup rather than
	// on the first order that names it.
	for _, sym := range cfg.Symbols {
		if v.shards.Runner(sym) == nil {
			return nil, fmt.Errorf("multisymbol: open %s: %w", sym, v.shards.Err())
		}
	}

	if cfg.MDAddr != "" {
		if v.ln, err = net.Listen("tcp", cfg.MDAddr); err != nil {
			v.Close()
			return nil, err
		}
		v.wg.Add(1)
		go v.accept()
	}
	return v, nil
}

// MDAddr is the address market-data subscribers dial.
func (v *Venue) MDAddr() string { return v.ln.Addr().String() }

// Runner returns a symbol's shard, or nil if the venue does not serve it.
func (v *Venue) Runner(symbol string) *matching.Runner {
	v.mu.RLock()
	_, known := v.feeds[symbol]
	v.mu.RUnlock()
	if !known {
		return nil
	}
	return v.shards.Runner(symbol)
}

// RunnerFor routes by order id, using the shard field the id carries. This is what
// the partitioned id space buys: a command that names an order names its book.
func (v *Venue) RunnerFor(orderID int64) *matching.Runner { return v.shards.RunnerFor(orderID) }

// Feed returns a symbol's market-data feed, or nil.
func (v *Venue) Feed(symbol string) *marketdata.Feed {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.feeds[symbol]
}

// Symbols returns the venue's live symbols, sorted.
func (v *Venue) Symbols() []string { return v.shards.Symbols() }

// ShardIndex returns a symbol's id-space partition.
func (v *Venue) ShardIndex(symbol string) (int, error) { return v.manifest.IndexFor(symbol) }

// Checkpoint writes one symbol's snapshot. Snapshots are per shard and are taken
// at unrelated moments, which is correct rather than tolerated: no invariant spans
// two books, so there is nothing a skewed set of snapshots could violate.
func (v *Venue) Checkpoint(symbol, path string) error {
	r := v.Runner(symbol)
	if r == nil {
		return fmt.Errorf("multisymbol: unknown symbol %s", symbol)
	}
	snap, err := r.Checkpoint()
	if err != nil {
		return err
	}
	return wal.WriteSnapshot(path, snap)
}

// Close stops the venue: listener, shards, then logs.
func (v *Venue) Close() {
	select {
	case <-v.quit:
		return
	default:
		close(v.quit)
	}
	if v.ln != nil {
		v.ln.Close()
	}
	v.shards.Close()
	v.wg.Wait()
	v.mu.Lock()
	for _, w := range v.writers {
		w.Sync()
		w.Close()
	}
	v.mu.Unlock()
}

func (v *Venue) accept() {
	defer v.wg.Done()
	for {
		conn, err := v.ln.Accept()
		if err != nil {
			return
		}
		v.wg.Add(1)
		go func() {
			defer v.wg.Done()
			defer conn.Close()
			v.serveMD(conn)
		}()
	}
}

// serveMD handles one subscriber. The subscription names its symbol — the whole
// point of wire v4 — and every message after it belongs to that symbol, so nothing
// downstream needs to carry one.
func (v *Venue) serveMD(conn net.Conn) {
	buf := make([]byte, wire.MaxPayload)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	pkt, err := wire.ReadPacket(conn, buf)
	if err != nil {
		return
	}
	sub, err := wire.DecodeMDSubscribe(pkt.Payload)
	if err != nil || sub.Version != wire.Version {
		mdReject(conn, wire.MDRejectMalformed)
		return
	}
	feed := v.Feed(sub.Symbol)
	if feed == nil {
		// Refused rather than served the wrong book. A subscriber cannot detect
		// that mistake for itself: every message after the subscribe is
		// symbol-free by design, so a wrong feed looks exactly like a right one.
		mdReject(conn, wire.MDRejectUnknownSymbol)
		return
	}
	if sub.Incarnation != v.incarnation && sub.Seq > 0 {
		mdReject(conn, wire.MDRejectWrongIncarnation)
		return
	}

	cursor, ok := v.sendStart(conn, feed, sub)
	if !ok {
		return
	}
	v.stream(conn, feed, cursor)
}

func (v *Venue) sendStart(conn net.Conn, feed *marketdata.Feed, sub wire.MDSubscribe) (uint64, bool) {
	if sub.Seq > 0 {
		updates, err := feed.Resume(sub.Incarnation, sub.Seq)
		switch {
		case errors.Is(err, marketdata.ErrWrongIncarnation):
			mdReject(conn, wire.MDRejectWrongIncarnation)
			return 0, false
		case errors.Is(err, marketdata.ErrSequenceEvicted):
			mdReject(conn, wire.MDRejectEvicted)
			return 0, false
		case err != nil:
			mdReject(conn, wire.MDRejectMalformed)
			return 0, false
		}
		if !sendUpdates(conn, updates) {
			return 0, false
		}
		if len(updates) > 0 {
			return updates[len(updates)-1].Seq, true
		}
		return sub.Seq, true
	}

	snap := feed.Snapshot()
	for _, side := range [2][]marketdata.L2Delta{snap.Bids, snap.Asks} {
		for _, lvl := range side {
			b, err := wire.EncodeMDLevel(nil, wire.MDLevel{
				Version: wire.Version, Side: mdSide(lvl.Side), Price: lvl.Price, Qty: lvl.Qty,
			})
			if err != nil || !mdWrite(conn, b) {
				return 0, false
			}
		}
	}
	end, err := wire.EncodeMDSnapshotEnd(nil, wire.MDSnapshotEnd{
		Version: wire.Version, Count: uint32(len(snap.Bids) + len(snap.Asks)),
		Seq: snap.Seq, LastTradePrice: snap.LastTradePrice,
	})
	if err != nil || !mdWrite(conn, end) {
		return 0, false
	}
	return snap.Seq, true
}

func (v *Venue) stream(conn net.Conn, feed *marketdata.Feed, cursor uint64) {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-v.quit:
			return
		case <-tick.C:
			updates, err := feed.Since(cursor)
			if err != nil || len(updates) == 0 {
				if err != nil {
					return
				}
				continue
			}
			if !sendUpdates(conn, updates) {
				return
			}
			cursor = updates[len(updates)-1].Seq
		}
	}
}

func sendUpdates(conn net.Conn, updates []marketdata.Update) bool {
	for _, u := range updates {
		var (
			b   []byte
			err error
		)
		switch u.Kind {
		case marketdata.UpdateBookDelta:
			b, err = wire.EncodeMDDelta(nil, wire.MDDelta{
				Version: wire.Version, Seq: u.Seq, Side: mdSide(u.Side), Price: u.Price, Qty: u.Qty,
			})
		case marketdata.UpdateTrade:
			b, err = wire.EncodeMDTrade(nil, wire.MDTrade{
				Version: wire.Version, Seq: u.Seq, Price: u.TradePrice,
				Qty: u.TradeQty, Aggressor: mdSide(u.Aggressor), TradeID: u.TradeID,
			})
		case marketdata.UpdateBust:
			b, err = wire.EncodeMDBust(nil, wire.MDBust{
				Version: wire.Version, Seq: u.Seq, TradeID: u.TradeID,
			})
		case marketdata.UpdateStatus:
			b, err = wire.EncodeMDStatus(nil, wire.MDStatus{
				Version: wire.Version, Seq: u.Seq, State: mdState(u.State),
			})
		default:
			continue
		}
		if err != nil || !mdWrite(conn, b) {
			return false
		}
	}
	return true
}

func mdWrite(conn net.Conn, payload []byte) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return wire.WritePacket(conn, wire.PacketSequencedData, payload) == nil
}

func mdReject(conn net.Conn, reason uint8) {
	b, err := wire.EncodeMDReject(nil, wire.MDReject{Version: wire.Version, Reason: reason})
	if err == nil {
		mdWrite(conn, b)
	}
}

func mdSide(s types.Side) uint8 {
	if s == types.SideSell {
		return wire.SideSell
	}
	return wire.SideBuy
}

func mdState(s marketdata.VenueState) uint8 {
	switch s {
	case marketdata.StateHalted:
		return wire.MDStateHalted
	case marketdata.StateCancelOnly:
		return wire.MDStateCancelOnly
	default:
		return wire.MDStateOpen
	}
}

func main() {
	log.Println("multisymbol: a reference two-book venue; see the package comment and its tests")
}

// recoverSymbol rebuilds one symbol's engine from its own log (and snapshot, if
// there is one). Venue startup is this, once per symbol in the manifest — there is
// no venue-wide recovery step because there is nothing venue-wide to recover.
func recoverSymbol(cfg matching.Config, walPath string) (*matching.Engine, error) {
	return wal.Recover(cfg, walPath+".snap", walPath)
}
