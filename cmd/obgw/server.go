package main

import (
	"errors"
	"fmt"
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
	"github.com/intrepidkarthi/orderbook/pkg/wal"
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
	// WALPath, when set, turns on durability: every command is written to the log
	// before it is applied, and the server recovers from it on start.
	WALPath string
	// SnapshotPath is where periodic checkpoints are written. Recovery replays
	// only the log tail after the snapshot, so this bounds restart time.
	SnapshotPath string
	// CheckpointEvery bounds how much log a restart must replay. Zero disables
	// checkpointing, which is legal but means replay grows without limit.
	CheckpointEvery time.Duration
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

	wal      *wal.Writer
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

// NewServer builds a server and its engine. With WALPath set it recovers from
// disk first, then serves — which is the whole point of shipping a log.
func NewServer(cfg Config) (*Server, error) {
	cfg.applyDefaults()

	reg := orderentry.NewRegistry(cfg.Incarnation, cfg.StreamRing)
	pub := orderentry.NewPublisher(reg, 1<<15)

	eng := matching.DefaultConfig(cfg.Symbol)
	eng.DedupClientOrderIDs = 4096

	var (
		w         *wal.Writer
		recovered *matching.Engine
	)
	if cfg.WALPath != "" {
		// Recover with NO event sink attached. Replaying the log re-emits every
		// historical event, and a publisher attached during recovery would fan a
		// lifetime of executions at whoever connected next.
		var err error
		if recovered, err = wal.Recover(eng, cfg.SnapshotPath, cfg.WALPath); err != nil {
			return nil, fmt.Errorf("obgw: recover: %w", err)
		}
		if n := recovered.OrderCount(); n > 0 {
			log.Printf("obgw: recovered %d resting orders from %s", n, cfg.WALPath)
		}
		// Only now does the publisher go on, so live events are published and
		// replayed history is not.
		recovered.SetEventSink(pub)
		if w, err = wal.Open(cfg.WALPath); err != nil {
			return nil, fmt.Errorf("obgw: open wal: %w", err)
		}
	}

	eng.EventSink = pub

	// Only populate Log when there really is one. Assigning a nil *wal.Writer to
	// the CommandLog interface field yields a NON-nil interface holding a nil
	// pointer, so the Runner's `log != nil` check passes and the first command
	// dereferences nil. This is the standard Go typed-nil trap and it cost a
	// segfault on the first run with durability disabled.
	rc := matching.RunnerConfig{Engine: eng, QueueSize: 8192}
	if w != nil {
		rc.Log = w
	}
	// recovered is nil without a WAL, in which case NewRunnerFor builds a fresh
	// engine from the config. With one, the recovered book is what we serve —
	// building from the bare config here is how the first attempt silently threw
	// away everything it had just read back from disk.
	runner := matching.NewRunnerFor(recovered, rc)

	return &Server{
		wal:    w,
		cfg:    cfg,
		runner: runner,
		gate:   gateway.New(runner, gateway.Config{Rate: cfg.RatePerSec, Burst: cfg.Burst}),
		reg:    reg,
		pub:    pub,
		quit:   make(chan struct{}),
		conns:  map[net.Conn]struct{}{},
	}, nil
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
	if s.wal != nil {
		go s.syncLoop()
		if s.cfg.CheckpointEvery > 0 && s.cfg.SnapshotPath != "" {
			go s.checkpointLoop()
		}
	}
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
		// The log is closed last, after the matcher has stopped producing, and
		// Close syncs — so a clean shutdown loses nothing.
		if s.wal != nil {
			if err := s.wal.Close(); err != nil {
				log.Printf("obgw: wal close: %v", err)
			}
		}
	})
}

// syncLoop group-commits the log. Syncing per command would put a disk write in
// the matching path; syncing never would make the log decorative. The interval is
// the durability window: a crash loses at most this much.
func (s *Server) syncLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			if err := s.wal.Sync(); err != nil {
				log.Printf("obgw: wal sync: %v", err)
			}
		}
	}
}

// checkpointLoop bounds restart time by snapshotting on a cadence. The snapshot
// is taken on the matching goroutine and stamped with the log position it is
// consistent with, so recovery replays only the tail after it.
func (s *Server) checkpointLoop() {
	t := time.NewTicker(s.cfg.CheckpointEvery)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			snap, err := s.runner.Checkpoint()
			if err != nil {
				return // shutting down
			}
			if err := wal.WriteSnapshot(s.cfg.SnapshotPath, snap); err != nil {
				log.Printf("obgw: checkpoint: %v", err)
			}
		}
	}
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

// Connection lifecycle bounds. An order-entry session is long-lived and mostly
// idle, so the read timeout is generous relative to the heartbeat: a client that
// is alive will always have sent or been sent something well inside it.
const (
	idleTimeout       = 30 * time.Second
	heartbeatInterval = 5 * time.Second
	writeTimeout      = 10 * time.Second
	loginTimeout      = 10 * time.Second
)

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

	// An unauthenticated peer gets the least patience: connect-and-say-nothing
	// is the cheapest possible resource-exhaustion attack.
	_ = conn.SetReadDeadline(time.Now().Add(loginTimeout))

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
	go sess.heartbeat()
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

		// An idle peer must not hold a goroutine, a buffer and a stream forever.
		// The deadline is refreshed on every packet, and the client's heartbeat
		// counts, so a live-but-quiet session stays up while a dead one is reaped.
		_ = sess.conn.SetReadDeadline(time.Now().Add(idleTimeout))

		pkt, err := wire.ReadPacket(sess.conn, buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// A malformed frame or an idle timeout is terminal; drop rather
				// than guess at what the peer meant.
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

// apply turns one inbound message into an engine command, dispatching on the
// declared message type. Inferring the type from payload length would mean any
// future message sharing a length with an existing one is silently misread.
func (sess *session) apply(payload []byte) {
	msgType, ok := wire.MsgTypeOf(payload)
	if !ok {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	if v, _ := wire.VersionOf(payload); v != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	switch msgType {
	case wire.MsgEnter:
		sess.enter(payload)
	case wire.MsgCancel:
		sess.cancel(payload)
	case wire.MsgQuery:
		go sess.reportOpenOrders() // reads the book and drains the pump; not on the read loop
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

	// The gateway serves one instrument. A client naming a different one is
	// refused rather than silently having its order booked in this one.
	if m.Symbol != sess.srv.cfg.Symbol {
		sess.reject(m.ClOrdID, orderentry.ReasonMalformed)
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

// reportOpenOrders answers a Query with the venue's authoritative view of the
// account's live orders.
//
// The ordering here is the whole point. The book is read on the matching
// goroutine, then the publisher is drained, and only then is the report written.
// That sequence guarantees every event up to the instant of the read has already
// reached the client, so the report and the stream cannot contradict each other:
// everything after the reported Seq is a change to apply on top of it.
//
// Reading the book without draining first would let an execution that happened
// before the read arrive after the report, and the client would apply it twice.
func (sess *session) reportOpenOrders() {
	orders, err := sess.srv.runner.OpenOrdersFor(sess.account)
	if err != nil {
		sess.reject("", orderentry.ReasonShuttingDown)
		return
	}
	sess.srv.pub.Wait()

	stream := sess.srv.reg.Stream(sess.account)
	for _, o := range orders {
		side := wire.SideBuy
		if o.Side == types.SideSell {
			side = wire.SideSell
		}
		b, encErr := wire.EncodeOpenOrder(nil, wire.OpenOrder{
			Version: wire.Version, ClOrdID: o.ClientOrderID,
			Price: o.Price, LeavesQty: o.RemainingQty, Side: side,
		})
		if encErr != nil {
			log.Printf("obgw: encode open order: %v", encErr)
			continue
		}
		sess.send(b)
	}

	// The terminator carries the count and the stream position. Without it a
	// client cannot tell "you have nothing open" from "the report was cut short",
	// which are opposite conclusions.
	b, encErr := wire.EncodeQueryEnd(nil, wire.QueryEnd{
		Version: wire.Version, Count: uint32(len(orders)), Seq: stream.Seq(),
	})
	if encErr != nil {
		return
	}
	sess.send(b)
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

// heartbeat sends a server heartbeat on an idle outbound path, so a client can
// distinguish "the venue has nothing for me" from "the venue is gone". Declaring
// the packet type and never sending it would leave clients unable to tell.
func (sess *session) heartbeat() {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		case <-t.C:
			select {
			case sess.out <- nil: // nil payload => heartbeat
			default:
				// Send queue full: the client is not reading, and the outbound
				// path will disconnect it. Do not add to the backlog.
			}
		}
	}
}

func (sess *session) writeLoop() {
	for {
		select {
		case <-sess.closed:
			_ = sess.conn.Close()
			return
		case b := <-sess.out:
			typ := wire.PacketSequencedData
			if b == nil {
				typ = wire.PacketServerHeartbt
			}
			_ = sess.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := wire.WritePacket(sess.conn, typ, b); err != nil {
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
