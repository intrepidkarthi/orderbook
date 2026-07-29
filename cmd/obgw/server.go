package main

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

// Config configures the reference server.
type Config struct {
	Addr        string
	Symbol      string
	Incarnation string
	// Accounts maps username to password. Authentication defaults to DENY: an
	// empty map rejects every login rather than admitting everyone, because the
	// failure mode of the other default is an open venue.
	Accounts map[string]string
	// OutboundDepth bounds each connection's send queue. A client that stops
	// reading is disconnected rather than allowed to back up into the venue.
	OutboundDepth int
	StreamRing    int
	RatePerSec    float64
	Burst         float64
}

func (c *Config) applyDefaults() {
	if c.Symbol == "" {
		c.Symbol = "X"
	}
	if c.OutboundDepth <= 0 {
		c.OutboundDepth = 1024
	}
	if c.StreamRing <= 0 {
		c.StreamRing = 8192
	}
	if c.RatePerSec <= 0 {
		c.RatePerSec = 1000
	}
	if c.Burst <= 0 {
		c.Burst = 200
	}
	if c.Incarnation == "" {
		c.Incarnation = "INC0000001"
	}
}

// Server is a single-symbol order-entry gateway.
//
// Single-symbol is a real constraint, not a shortcut: engine order ids and event
// sequences are per-Engine, so there is no venue-wide identifier space to hand a
// client across symbols. Running several symbols means several engines, several
// registries and a routing layer above this — which is the embedder's design
// decision, not one this reference should make for them.
type Server struct {
	cfg    Config
	runner *matching.Runner
	gate   *gateway.Gateway
	reg    *orderentry.Registry
	pub    *orderentry.Publisher

	ln       net.Listener
	wg       sync.WaitGroup
	quit     chan struct{}
	closeOne sync.Once

	// live connections, so shutdown can unblock handlers parked in a read.
	// Closing the listener stops new accepts but does nothing to established
	// sockets, so without this Close waits forever on its own handlers.
	connMu sync.Mutex
	conns  map[net.Conn]struct{}
}

// NewServer builds a server and its engine.
func NewServer(cfg Config) *Server {
	cfg.applyDefaults()

	reg := orderentry.NewRegistry(cfg.Incarnation, cfg.StreamRing)
	pub := orderentry.NewPublisher(reg, 1<<15)

	eng := matching.DefaultConfig(cfg.Symbol)
	eng.EventSink = pub
	eng.DedupClientOrderIDs = 4096

	runner := matching.NewRunner(matching.RunnerConfig{Engine: eng, QueueSize: 8192})

	return &Server{
		cfg:    cfg,
		runner: runner,
		gate:   gateway.New(runner, gateway.Config{Rate: cfg.RatePerSec, Burst: cfg.Burst}),
		reg:    reg,
		pub:    pub,
		quit:   make(chan struct{}),
		conns:  map[net.Conn]struct{}{},
	}
}

// Addr reports the bound address, valid after Listen.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Listen binds the socket without serving, so a test can learn the port.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	go s.pub.Pump()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil // an expected close, not a failure
			default:
				return err
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

// Close stops accepting, drops live connections, drains the matcher and stops the
// pump — in that order, so nothing is published after the streams stop being read
// and no producer is left waiting on a matcher that has gone.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.quit)
		if s.ln != nil {
			_ = s.ln.Close()
		}
		// Established connections must be closed explicitly: handlers are parked
		// in a blocking read, and closing the listener does not reach them.
		s.connMu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.connMu.Unlock()

		s.wg.Wait()
		s.runner.Close()
		s.pub.Close()
	})
}

func (s *Server) trackConn(c net.Conn) {
	s.connMu.Lock()
	s.conns[c] = struct{}{}
	s.connMu.Unlock()
}

func (s *Server) untrackConn(c net.Conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

// session is one authenticated connection.
type session struct {
	srv     *Server
	conn    net.Conn
	account string
	out     chan []byte
	closed  chan struct{}
	once    sync.Once
}

func (s *Server) handle(conn net.Conn) {
	s.trackConn(conn)
	defer func() {
		s.untrackConn(conn)
		_ = conn.Close()
	}()

	buf := make([]byte, wire.MaxPayload)

	// Login must be the first packet. Anything else is a protocol error and the
	// connection is dropped without a reply — an unauthenticated peer gets no
	// information about the venue.
	pkt, err := wire.ReadPacket(conn, buf)
	if err != nil || pkt.Type != wire.PacketLoginRequest {
		return
	}
	req, err := wire.DecodeLoginRequest(pkt.Payload)
	if err != nil {
		return
	}

	want, ok := s.cfg.Accounts[req.Username]
	if !ok || want == "" || want != req.Password {
		_ = wire.WritePacket(conn, wire.PacketLoginRejected, []byte{wire.RejectNotAuthorised})
		return
	}

	// Resume, if asked. A cursor from another incarnation, or one already evicted,
	// is refused explicitly rather than quietly served from the start — the client
	// must know it has a gap.
	var backlog []orderentry.Msg
	if req.Session != "" {
		backlog, err = s.reg.Resume(req.Session, req.Username, req.Sequence)
		if err != nil {
			code := wire.RejectBadSequence
			if errors.Is(err, orderentry.ErrNoSuchStream) {
				code = wire.RejectNoSession
			}
			_ = wire.WritePacket(conn, wire.PacketLoginRejected, []byte{code})
			return
		}
	}

	stream := s.reg.Stream(req.Username)
	acc, err := wire.EncodeLoginAccepted(nil, wire.LoginAccepted{
		Session:  s.reg.Incarnation(),
		Sequence: stream.Seq(),
	})
	if err != nil {
		return
	}
	if err := wire.WritePacket(conn, wire.PacketLoginAccepted, acc); err != nil {
		return
	}

	sess := &session{
		srv:     s,
		conn:    conn,
		account: req.Username,
		out:     make(chan []byte, s.cfg.OutboundDepth),
		closed:  make(chan struct{}),
	}
	defer sess.close()

	go sess.writeLoop()
	go sess.followStream(stream, req.Sequence, backlog)

	sess.readLoop(buf)
}

func (sess *session) close() {
	sess.once.Do(func() { close(sess.closed) })
}

// readLoop applies inbound commands until the peer goes away.
func (sess *session) readLoop(buf []byte) {
	for {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		default:
		}

		pkt, err := wire.ReadPacket(sess.conn, buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A malformed frame is a protocol error; drop rather than guess.
				return
			}
			return
		}
		switch pkt.Type {
		case wire.PacketUnsequenced:
			sess.apply(pkt.Payload)
		case wire.PacketClientHeartbt:
			// nothing to do; the read itself is the liveness signal
		case wire.PacketLogoutRequest:
			return
		}
	}
}

// apply turns one inbound message into an engine command.
func (sess *session) apply(payload []byte) {
	if len(payload) == 0 {
		return
	}
	switch payload[0] {
	case wire.Version:
		// The first byte of every payload is the version; the message type is
		// carried by the packet's second byte in this protocol, so an Enter and a
		// Cancel are distinguished by length.
	}
	switch len(payload) {
	case wire.EnterLen:
		sess.enter(payload)
	case wire.CancelLen:
		sess.cancel(payload)
	default:
		sess.reject("", orderentry.ReasonMalformed)
	}
}

func (sess *session) enter(payload []byte) {
	m, err := wire.DecodeEnter(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}

	side := types.SideBuy
	if m.Side == wire.SideSell {
		side = types.SideSell
	}
	otype := types.OrderTypeLimit
	if m.Type == wire.TypeMarket {
		otype = types.OrderTypeMarket
	}
	tif := types.TIFGoodTillCancel
	switch m.TIF {
	case wire.TIFImmediateOrCanc:
		tif = types.TIFImmediateOrCancel
	case wire.TIFFillOrKill:
		tif = types.TIFFillOrKill
	}

	// The account comes from the authenticated session, never from the wire.
	o, err := types.NewOrder(sess.account, sess.srv.cfg.Symbol, side, otype, m.Price, m.Quantity, tif)
	if err != nil {
		sess.reject(m.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	o.ClientOrderID = m.ClOrdID
	o.PostOnly = m.PostOnly

	if !sess.srv.gate.Allow(o, time.Now()) {
		sess.reject(m.ClOrdID, orderentry.ReasonThrottled)
		return
	}

	// Fire and forget: the outcome arrives on the event stream. The synchronous
	// API would hand back the engine-owned order, which this goroutine must not
	// read while the matcher is mutating it.
	if err := sess.srv.runner.TryEnqueue(o); err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(m.ClOrdID, reason)
	}
}

func (sess *session) cancel(payload []byte) {
	m, err := wire.DecodeCancel(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	id, ok := sess.srv.reg.OrderIDFor(sess.account, m.ClOrdID)
	if !ok {
		sess.reject(m.ClOrdID, orderentry.ReasonUnknownOrder)
		return
	}
	// The account is the session's, so a client can only ever cancel its own.
	if err := sess.srv.runner.TryEnqueueCancel(id, sess.account); err != nil {
		sess.reject(m.ClOrdID, orderentry.ReasonOverloaded)
	}
}

// reject reports that the command itself was refused, as distinct from an order
// the engine looked at and declined.
func (sess *session) reject(clOrdID string, reason uint16) {
	b, err := wire.EncodeCmdReject(nil, wire.CmdReject{Version: wire.Version, ClOrdID: clOrdID, Reason: reason})
	if err != nil {
		return
	}
	sess.send(b)
}

// send queues an outbound payload, dropping the connection if the client has
// stopped reading. A slow consumer must never be allowed to back up into the
// venue.
func (sess *session) send(payload []byte) {
	select {
	case sess.out <- payload:
	default:
		sess.close()
	}
}

func (sess *session) writeLoop() {
	for {
		select {
		case <-sess.closed:
			_ = sess.conn.Close()
			return
		case b := <-sess.out:
			if err := wire.WritePacket(sess.conn, wire.PacketSequencedData, b); err != nil {
				sess.close()
				return
			}
		}
	}
}

// followStream replays anything the client missed, then tails its stream.
//
// Polling rather than subscribing keeps Stream free of per-listener state; the
// interval bounds latency, and a real deployment would swap this for a condition
// variable. It is called out here rather than left for a reader to discover.
func (sess *session) followStream(stream *orderentry.Stream, from uint64, backlog []orderentry.Msg) {
	cursor := from
	for _, m := range backlog {
		sess.emit(m)
		cursor = m.Seq
	}
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		case <-tick.C:
			msgs, err := stream.Since(cursor)
			if err != nil {
				sess.close()
				return
			}
			for _, m := range msgs {
				sess.emit(m)
				cursor = m.Seq
			}
		}
	}
}

// emit encodes one outbound message and queues it.
func (sess *session) emit(m orderentry.Msg) {
	var (
		b   []byte
		err error
	)
	switch m.Kind {
	case orderentry.KindAccepted:
		side := wire.SideBuy
		if m.Side == types.SideSell {
			side = wire.SideSell
		}
		b, err = wire.EncodeAccepted(nil, wire.Accepted{
			Version: wire.Version, ClOrdID: m.ClOrdID,
			Price: m.Price, Quantity: m.Quantity, Side: side,
		})
	case orderentry.KindRejected:
		b, err = wire.EncodeRejected(nil, wire.Rejected{Version: wire.Version, ClOrdID: m.ClOrdID, Reason: m.Reason})
	case orderentry.KindExecuted:
		agg := wire.SideBuy
		if m.Aggressor == types.SideSell {
			agg = wire.SideSell
		}
		b, err = wire.EncodeExecuted(nil, wire.Executed{
			Version: wire.Version, ClOrdID: m.ClOrdID,
			Price: m.Price, Quantity: m.Quantity, LeavesQty: m.LeavesQty, Aggressor: agg,
		})
	case orderentry.KindCanceled:
		b, err = wire.EncodeCanceled(nil, wire.Canceled{Version: wire.Version, ClOrdID: m.ClOrdID, Reason: m.Reason})
	case orderentry.KindReplaced:
		b, err = wire.EncodeReplaced(nil, wire.Replaced{Version: wire.Version, ClOrdID: m.ClOrdID, LeavesQty: m.LeavesQty})
	default:
		return
	}
	if err != nil {
		log.Printf("obgw: encode %d: %v", m.Kind, err)
		return
	}
	sess.send(b)
}
