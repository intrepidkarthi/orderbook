package main

import (
	"errors"
	"log"
	"net"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// The market-data edge, served on its own listener from the same engine.
//
// One process, two edges, which is how a venue is actually shaped: order entry is
// authenticated and per-account, market data is anonymous and identical for everyone,
// and they have different security properties, different backpressure answers and
// different reasons to disconnect you. Running them on one port would mean an
// unauthenticated subscriber sharing a code path with order entry, which is the wrong
// default however carefully it is written.
//
// # What a subscriber gets
//
// Subscribe with sequence 0 and you get a snapshot — a run of MDLevel followed by an
// MDSnapshotEnd naming the sequence it is consistent with — and then the live stream
// from that point. Subscribe with a sequence you already hold and you get everything
// after it, with no snapshot.
//
// Either way the contract is the one marketdata.Feed guarantees: everything after
// MDSnapshotEnd.Seq applies on top of the snapshot, and nothing at or below it does.
//
// # Backpressure
//
// A subscriber that stops reading is disconnected, exactly as on the order-entry side.
// For market data there is a better answer available — conflate, and hand the
// subscriber a fresh snapshot when it catches up, since nothing is owed to it
// personally — and that is deliberately not implemented here rather than half
// implemented. Disconnecting is correct and boring; conflation is a feature with its
// own failure modes.

// mdIdleTimeout bounds a silent subscriber. It is longer than the order-entry idle
// timeout because a market-data subscriber has nothing to say after subscribing: it
// is a listener, and the server's own traffic is the liveness signal in the other
// direction.
const (
	mdIdleTimeout    = 120 * time.Second
	mdSubscribeGrace = 10 * time.Second
	mdPollInterval   = 2 * time.Millisecond
)

// serveMarketData accepts subscribers until Close.
func (s *Server) serveMarketData() error {
	for {
		conn, err := s.mdLn.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil
			default:
				return err
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleSubscriber(conn)
		}()
	}
}

func (s *Server) handleSubscriber(conn net.Conn) {
	s.trackConn(conn)
	defer func() {
		s.untrackConn(conn)
		_ = conn.Close()
	}()

	buf := make([]byte, wire.MaxPayload)
	// An unsubscribed peer gets the least patience, for the same reason the
	// order-entry side gives a login deadline: connect-and-say-nothing is the
	// cheapest resource-exhaustion attack there is.
	_ = conn.SetReadDeadline(time.Now().Add(mdSubscribeGrace))
	pkt, err := wire.ReadPacket(conn, buf)
	if err != nil || pkt.Type != wire.PacketUnsequenced {
		return
	}
	sub, err := wire.DecodeMDSubscribe(pkt.Payload)
	if err != nil || sub.Version != wire.Version {
		s.mdRejectAndClose(conn, wire.MDRejectMalformed)
		return
	}

	cursor, ok := s.mdStart(conn, sub)
	if !ok {
		return
	}
	s.mdStream(conn, cursor)
}

// mdStart resolves where this subscriber begins: a snapshot for a new one, or the
// resume point for one that already holds a cursor. It returns the sequence the
// subscriber is now consistent with.
func (s *Server) mdStart(conn net.Conn, sub wire.MDSubscribe) (uint64, bool) {
	if sub.Seq > 0 {
		// A resume. The incarnation is checked first, because a cursor from another
		// run of the venue is meaningless here and serving it would hand the
		// subscriber different content under numbers it believes it already has.
		updates, err := s.feed.Resume(sub.Incarnation, sub.Seq)
		switch {
		case errors.Is(err, marketdata.ErrWrongIncarnation):
			s.mdRejectAndClose(conn, wire.MDRejectWrongIncarnation)
			return 0, false
		case errors.Is(err, marketdata.ErrSequenceEvicted):
			// Refused explicitly rather than quietly restarted from a snapshot: the
			// subscriber asked a precise question and deserves to know the answer is
			// "that is gone", so it can decide to resubscribe from zero.
			s.mdRejectAndClose(conn, wire.MDRejectEvicted)
			return 0, false
		case err != nil:
			s.mdRejectAndClose(conn, wire.MDRejectMalformed)
			return 0, false
		}
		if !s.mdSendUpdates(conn, updates) {
			return 0, false
		}
		if len(updates) > 0 {
			return updates[len(updates)-1].Seq, true
		}
		return sub.Seq, true
	}

	// A new subscriber. The snapshot carries the sequence it is consistent with, and
	// Feed.Snapshot takes both under one lock so that sequence is exact.
	snap := s.feed.Snapshot()
	for _, side := range [2][]marketdata.L2Delta{snap.Bids, snap.Asks} {
		for _, lvl := range side {
			b, err := wire.EncodeMDLevel(nil, wire.MDLevel{
				Version: wire.Version, Side: mdSide(lvl.Side), Price: lvl.Price, Qty: lvl.Qty,
			})
			if err != nil {
				return 0, false
			}
			if !mdWrite(conn, b) {
				return 0, false
			}
		}
	}
	end, err := wire.EncodeMDSnapshotEnd(nil, wire.MDSnapshotEnd{
		Version: wire.Version, Count: uint32(len(snap.Bids) + len(snap.Asks)),
		Seq: snap.Seq, LastTradePrice: snap.LastTradePrice,
	})
	if err != nil {
		return 0, false
	}
	if !mdWrite(conn, end) {
		return 0, false
	}
	return snap.Seq, true
}

// mdStream tails the feed from cursor until the subscriber or the venue goes away.
//
// Polling rather than subscribing keeps Feed free of per-listener state, the same
// trade the order-entry side makes; the interval bounds latency and a real deployment
// would swap it for a condition variable. Called out here rather than left to be
// discovered.
func (s *Server) mdStream(conn net.Conn, cursor uint64) {
	tick := time.NewTicker(mdPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-tick.C:
			updates, err := s.feed.Since(cursor)
			if err != nil {
				// The subscriber fell behind the retention ring while connected. It
				// is told, rather than silently resynchronised, so it knows its own
				// picture had a hole in it.
				s.mdRejectAndClose(conn, wire.MDRejectEvicted)
				return
			}
			if len(updates) == 0 {
				continue
			}
			if !s.mdSendUpdates(conn, updates) {
				return
			}
			cursor = updates[len(updates)-1].Seq
		}
	}
}

func (s *Server) mdSendUpdates(conn net.Conn, updates []marketdata.Update) bool {
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
		case marketdata.UpdateIndicative:
			b, err = wire.EncodeMDIndicative(nil, wire.MDIndicative{
				Version: wire.Version, Seq: u.Seq, Price: u.IndicativePrice,
				Volume: u.IndicativeVolume, Imbalance: u.Imbalance,
			})
		case marketdata.UpdateStatus:
			b, err = wire.EncodeMDStatus(nil, wire.MDStatus{
				Version: wire.Version, Seq: u.Seq, State: mdState(u.State),
			})
		default:
			continue
		}
		if err != nil {
			log.Printf("obgw: encode market data: %v", err)
			return false
		}
		if !mdWrite(conn, b) {
			return false
		}
	}
	return true
}

// mdWrite sends one payload under a write deadline. A subscriber that has stopped
// reading fills the socket buffer, the deadline expires, and it is dropped — the
// venue must never block on a slow consumer.
func mdWrite(conn net.Conn, payload []byte) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return wire.WritePacket(conn, wire.PacketSequencedData, payload) == nil
}

func (s *Server) mdRejectAndClose(conn net.Conn, reason uint8) {
	b, err := wire.EncodeMDReject(nil, wire.MDReject{Version: wire.Version, Reason: reason})
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = wire.WritePacket(conn, wire.PacketSequencedData, b)
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
