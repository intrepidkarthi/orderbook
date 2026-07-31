package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/gateway"
	"github.com/intrepidkarthi/orderbook/pkg/marketdata"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
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
	// MDAddr, when set, starts a market-data listener on its own port. Order entry
	// is authenticated and per-account; market data is anonymous and identical for
	// everyone. Sharing one port would put an unauthenticated subscriber on the same
	// code path as order entry, which is the wrong default however carefully it is
	// written.
	MDAddr string
	// SessionClose reports when the trading session ends, which is what a DAY order
	// expires at. Nil means the venue has declared no session and DAY orders are
	// refused.
	SessionClose func() time.Time
	// ExpireEvery drives the expiry sweep. The engine expires lazily on command
	// arrival, which is enough for correctness — an expired order is gone before it
	// could match — but in a quiet market a market-data subscriber would see depth
	// that should have left. Zero disables the ticker.
	ExpireEvery time.Duration
	// MDRetain bounds the market-data gap-fill ring. A subscriber further behind than
	// this is told to resubscribe from a snapshot rather than served a partial answer.
	MDRetain int
	// AdminAddr, when set, starts an HTTP listener serving /metrics, /healthz and
	// /readyz on its own port. Empty means the venue trades unobserved, which is
	// legal here and is not something to do on purpose — see admin.go.
	AdminAddr string
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
	if c.MDRetain <= 0 {
		c.MDRetain = 1 << 16
	}
	if c.ExpireEvery <= 0 {
		c.ExpireEvery = time.Second
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
	feed   *marketdata.Feed

	// metrics is a sink like the others, so it counts exactly what the book saw
	// rather than what the gateway believed it sent.
	metrics   *observability.Collector
	applyHist *observability.Histogram
	admin     admin

	wal      *wal.Writer
	ln       net.Listener
	mdLn     net.Listener
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
	// The market-data feed shares the incarnation: both edges are describing the same
	// run of the venue, and a subscriber that reconnects after a restart must be
	// refused for the same reason an order-entry client is.
	feed := marketdata.NewFeed(cfg.Incarnation, cfg.MDRetain)
	// The collector is a sink like the other two, so it counts what the book saw
	// rather than what the gateway believed it sent. Attaching it here rather than
	// wrapping the handlers is the difference between metrics that can disagree with
	// the engine and metrics that cannot.
	col := observability.NewCollector()
	// MultiSink rather than replacing the publisher: Config.EventSink is a single
	// slot, so attaching the feed on its own would silently stop every execution
	// report the order-entry side depends on.
	sink := matching.MultiSink{pub, feed, col}

	eng := matching.DefaultConfig(cfg.Symbol)
	eng.DedupClientOrderIDs = 4096
	eng.SessionClose = cfg.SessionClose

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
		// Rebuild the session layer's index over the recovered book. Recovery used
		// to restore the book and nothing else, which left every recovered order
		// unnameable: a client could see them in a Query reply and could not cancel
		// or reduce them, and a fill against one produced no execution report at all
		// because the publisher had no record of the order.
		reg.Adopt(recovered.RestingOrders())
		// Seed the market-data feed too, so a subscriber's first snapshot shows the
		// venue as it actually is. A feed starting empty against a recovered book
		// would show only what has changed since the restart, which is almost
		// nothing — the same shape of bug the session index had before v0.12.0.
		feed.Adopt(recovered.RestingOrders(), recovered.LastTradePrice())
		// Only now do the sinks go on, so live events are published and replayed
		// history is not.
		recovered.SetEventSink(sink)
		if w, err = wal.Open(cfg.WALPath); err != nil {
			return nil, fmt.Errorf("obgw: open wal: %w", err)
		}
	}

	eng.EventSink = sink

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

	srv := &Server{
		wal:       w,
		cfg:       cfg,
		runner:    runner,
		metrics:   col,
		applyHist: col.Histogram(applyLatencyMetric),
		gate:      gateway.New(runner, gateway.Config{Rate: cfg.RatePerSec, Burst: cfg.Burst}),
		reg:       reg,
		pub:       pub,
		feed:      feed,
		quit:      make(chan struct{}),
		conns:     map[net.Conn]struct{}{},
	}
	srv.registerGauges()
	return srv, nil
}

// Addr reports the bound address, valid after Listen.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Listen binds the socket(s) without serving, so a test can learn the port.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	if s.cfg.MDAddr != "" {
		mdLn, err := net.Listen("tcp", s.cfg.MDAddr)
		if err != nil {
			_ = ln.Close()
			return err
		}
		s.mdLn = mdLn
	}
	// The admin edge comes up with the sockets, not with the market: an operator
	// wants /metrics answering while the venue is still recovering its log, which is
	// exactly when they most want to know what it is doing.
	if err := s.startAdmin(); err != nil {
		_ = ln.Close()
		if s.mdLn != nil {
			_ = s.mdLn.Close()
		}
		return err
	}
	return nil
}

// MDAddr reports the bound market-data address, valid after Listen when configured.
func (s *Server) MDAddr() net.Addr {
	if s.mdLn == nil {
		return nil
	}
	return s.mdLn.Addr()
}

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	go s.pub.Pump()
	go s.expireLoop()
	if s.mdLn != nil {
		go func() {
			if err := s.serveMarketData(); err != nil {
				log.Printf("obgw: market data: %v", err)
			}
		}()
	}
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
		if s.mdLn != nil {
			_ = s.mdLn.Close()
		}
		// Established connections must be closed explicitly: handlers are parked
		// in a blocking read, and closing the listener does not reach them.
		s.connMu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.connMu.Unlock()
		// Before the wait, not after: the admin server's goroutine is in the same
		// WaitGroup, and http.Server.Serve does not return until Shutdown is called.
		s.closeAdmin()

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

// expireLoop drives time-in-force expiry on a ticker, so a DAY or GTD order leaves
// the book at its deadline rather than at the next command. Correctness does not
// depend on it — an expired order is removed before anything can match against it —
// but market data would otherwise show depth that should have gone.
func (s *Server) expireLoop() {
	t := time.NewTicker(s.cfg.ExpireEvery)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			s.runner.ExpireDue()
		}
	}
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
	// cancelOnDisconnect is read by the handler after the connection drops and
	// written by the read loop, so it is atomic rather than a plain bool.
	cancelOnDisconnect atomic.Bool
	// emitted is the highest stream sequence followStream has queued for this
	// connection. Anything that claims "everything up to Seq has reached you" has to
	// wait on this, not merely on the publisher — see waitForStream.
	emitted atomic.Uint64
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
	// The sweep runs after the read loop returns, i.e. once the connection is
	// genuinely gone, so a client that logs out cleanly still gets it — dropping the
	// socket and logging out are the same thing to a book that must not keep quoting
	// on behalf of somebody who can no longer manage it.
	defer sess.pullBookIfRequested()

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
	defer sess.srv.observeApply(time.Now())

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
	case wire.MsgReduce:
		sess.reduce(payload)
	case wire.MsgReplaceOrder:
		sess.replaceOrder(payload)
	case wire.MsgEnterDated:
		sess.enterDated(payload)
	case wire.MsgEnterStop:
		sess.enterStop(payload)
	case wire.MsgEnterOCO:
		sess.enterOCO(payload)
	case wire.MsgEnterIceberg:
		sess.enterIceberg(payload)
	case wire.MsgEnterPegged:
		sess.enterPegged(payload)
	case wire.MsgEnterTrailing:
		sess.enterTrailing(payload)
	case wire.MsgMassCancel:
		sess.massCancel(payload)
	case wire.MsgCancelOnDisconnect:
		sess.setCancelOnDisconnect(payload)
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
	// Built through the same path as every conditional entry. This handler used to
	// carry its own copy of the side/type/TIF mapping, and the two drifted the moment
	// DAY and GTD were added: a plain Enter carrying the new TIF bytes fell through to
	// the default and rested as GTC, so an order the client believed had a deadline
	// would have lived forever. One mapping, one place.
	o, reason := sess.buildOrder(wire.BaseOrder{
		ClOrdID: m.ClOrdID, Symbol: m.Symbol, Side: m.Side, Type: m.Type,
		TIF: m.TIF, PostOnly: m.PostOnly, Price: m.Price, Quantity: m.Quantity,
	})
	if reason != 0 {
		sess.reject(m.ClOrdID, reason)
		return
	}

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

// buildOrder turns a wire base-order block into an engine order, applying the same
// rules Enter does: the account comes from the authenticated session and never from
// the wire, and an order naming a different instrument is refused rather than being
// booked here anyway.
//
// reason is non-zero when the order cannot be built, so each caller rejects with the
// client's own id rather than a bare code.
func (sess *session) buildOrder(b wire.BaseOrder) (*types.Order, uint16) {
	if b.Symbol != sess.srv.cfg.Symbol {
		return nil, orderentry.ReasonMalformed
	}
	side := types.SideBuy
	if b.Side == wire.SideSell {
		side = types.SideSell
	}
	otype := types.OrderTypeLimit
	if b.Type == wire.TypeMarket {
		otype = types.OrderTypeMarket
	}
	tif := types.TIFGoodTillCancel
	switch b.TIF {
	case wire.TIFImmediateOrCanc:
		tif = types.TIFImmediateOrCancel
	case wire.TIFFillOrKill:
		tif = types.TIFFillOrKill
	case wire.TIFDay:
		tif = types.TIFDay
	case wire.TIFGoodTillDate:
		// GTD has nowhere on a plain Enter to carry its deadline, so it is only
		// legal via EnterDated. Refused rather than quietly downgraded to GTC,
		// which would leave an order the client believes is dated resting forever.
		return nil, orderentry.ReasonMalformed
	}
	o, err := types.NewOrder(sess.account, sess.srv.cfg.Symbol, side, otype, b.Price, b.Quantity, tif)
	if err != nil {
		return nil, orderentry.ReasonMalformed
	}
	o.ClientOrderID = b.ClOrdID
	o.PostOnly = b.PostOnly
	return o, 0
}

// submitConditional applies the admission gate and enqueues, so every conditional
// entry is rate-limited and backpressured exactly like a plain Enter. A path that
// skipped the gate would be a way around the venue's own throttle.
func (sess *session) submitConditional(clOrdID string, o *types.Order, enqueue func() error) {
	if !sess.srv.gate.Allow(o, time.Now()) {
		sess.reject(clOrdID, orderentry.ReasonThrottled)
		return
	}
	if err := enqueue(); err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(clOrdID, reason)
	}
}

// enterDated places an order carrying its own expiry (GTD).
//
// The deadline is validated by the engine, which refuses one already in the past
// rather than accepting the order and expiring it on the next command.
func (sess *session) enterDated(payload []byte) {
	m, err := wire.DecodeEnterDated(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// The TIF byte is forced rather than trusted: this message exists precisely to
	// carry a deadline, and an EnterDated claiming to be GTC would be a contradiction
	// the engine would then have to guess about.
	m.Order.TIF = wire.TIFGoodTillCancel
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	o.TimeInForce = types.TIFGoodTillDate
	o.ExpiresAt = time.Unix(0, m.ExpiresAt).UTC()

	if !sess.srv.gate.Allow(o, time.Now()) {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonThrottled)
		return
	}
	if err := sess.srv.runner.TryEnqueue(o); err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(m.Order.ClOrdID, reason)
	}
}

// enterStop places a stop or stop-limit order.
func (sess *session) enterStop(payload []byte) {
	m, err := wire.DecodeEnterStop(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	stop, err := types.NewStopOrder(o, m.StopPrice)
	if err != nil {
		// A non-positive stop price lands here. Refused rather than treated as
		// "trigger now": an order that fires on arrival is a market order.
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.srv.runner.TryEnqueueStop(stop)
	})
}

// enterOCO places a primary order paired with a stop leg.
func (sess *session) enterOCO(payload []byte) {
	m, err := wire.DecodeEnterOCO(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	primary, reason := sess.buildOrder(m.Primary)
	if reason != 0 {
		sess.reject(m.Primary.ClOrdID, reason)
		return
	}
	// The stop leg inherits symbol, side, quantity and TIF from the primary; only
	// its own identifier and prices come off the wire. Legs of differing size would
	// leave a residual position behind whichever fired.
	leg := m.Primary
	leg.ClOrdID = m.StopClOrdID
	leg.Type = wire.TypeMarket
	leg.Price = 0
	if m.StopLimitPrice != 0 {
		leg.Type = wire.TypeLimit
		leg.Price = m.StopLimitPrice
	}
	stopOrder, reason := sess.buildOrder(leg)
	if reason != 0 {
		sess.reject(m.StopClOrdID, reason)
		return
	}
	stop, err := types.NewStopOrder(stopOrder, m.StopPrice)
	if err != nil {
		sess.reject(m.StopClOrdID, orderentry.ReasonMalformed)
		return
	}
	oco, err := types.NewOCOOrder(primary, stop)
	if err != nil {
		sess.reject(m.Primary.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Primary.ClOrdID, primary, func() error {
		return sess.srv.runner.TryEnqueueOCO(oco)
	})
}

// enterIceberg places an order that shows only a slice at a time.
func (sess *session) enterIceberg(payload []byte) {
	m, err := wire.DecodeEnterIceberg(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	ib, err := types.NewIcebergOrder(o, m.DisplayQty)
	if err != nil {
		// A display size that is zero, negative, or larger than the total.
		sess.reject(m.Order.ClOrdID, orderentry.ReasonInvalidQuantity)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.srv.runner.TryEnqueueIceberg(ib)
	})
}

// enterPegged places an order that tracks a reference price.
func (sess *session) enterPegged(payload []byte) {
	m, err := wire.DecodeEnterPegged(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// The peg computes the price, so a client-supplied one is refused rather than
	// silently overwritten — otherwise a client believes it set a price the venue
	// then replaced.
	if m.Order.Price != 0 {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	// The engine computes the price AND sets the type (ProcessPegged), so the order
	// is built as a market order here: types.NewOrder refuses a limit order priced at
	// zero, and zero is the only honest price to send for an order whose price the
	// venue derives.
	m.Order.Type = wire.TypeMarket
	var ref types.PegReference
	switch m.Ref {
	case wire.PegBid:
		ref = types.PegToBid
	case wire.PegAsk:
		ref = types.PegToAsk
	case wire.PegMid:
		ref = types.PegToMid
	default:
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	pegged, err := types.NewPeggedOrder(o, ref, m.Offset)
	if err != nil {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.srv.runner.TryEnqueuePegged(pegged)
	})
}

// enterTrailing places a stop whose trigger follows the market.
func (sess *session) enterTrailing(payload []byte) {
	m, err := wire.DecodeEnterTrailing(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	o, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	ts, err := types.NewTrailingStop(o, m.Trail)
	if err != nil {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonMalformed)
		return
	}
	sess.submitConditional(m.Order.ClOrdID, o, func() error {
		return sess.srv.runner.TryEnqueueTrailing(ts)
	})
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

// reduce shrinks a resting order in place, keeping its queue position.
//
// The enqueue happens here on the read loop rather than in a goroutine, so a
// reduce stays behind the order it names: dispatching it concurrently would let
// it overtake its own Enter and be refused for an order that does not exist yet.
// Only the wait for the outcome is moved off the read loop, because the matcher
// must never be able to stall a connection's ingress.
//
// Unlike a cancel, the outcome is reported. A reduce fails for reasons the client
// caused and can correct — asking to grow, or to shrink below what is already
// filled — and a client that hears nothing cannot tell that from a reduce still
// in flight.
func (sess *session) reduce(payload []byte) {
	m, err := wire.DecodeReduce(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	// A non-positive size is refused here rather than passed down: the engine
	// would reject it anyway, and there is no reason to spend a queue slot and a
	// log record on a message that cannot succeed.
	if m.Quantity <= 0 {
		sess.reject(m.ClOrdID, orderentry.ReasonInvalidQuantity)
		return
	}
	id, ok := sess.srv.reg.OrderIDFor(sess.account, m.ClOrdID)
	if !ok {
		sess.reject(m.ClOrdID, orderentry.ReasonUnknownOrder)
		return
	}
	// The account is the session's, so the engine's own (orderID, userID) check
	// makes naming another client's order impossible rather than merely refused.
	done, err := sess.srv.runner.TryReduceAsync(id, m.Quantity, sess.account)
	if err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(m.ClOrdID, reason)
		return
	}
	go func() {
		select {
		case rerr := <-done:
			if rerr != nil {
				sess.reject(m.ClOrdID, orderentry.ReasonFor(rerr))
			}
			// Success needs nothing here: the engine emits Replaced, which reaches
			// this client over its own stream like every other outcome.
		case <-sess.closed:
		case <-sess.srv.quit:
		}
	}()
}

// waitForStream blocks until this connection has queued every stream message up to
// target, so a message asserting "everything through Seq has reached you" is true
// when it is written rather than merely true of the publisher.
//
// Draining the publisher is not enough on its own. That only moves events into the
// account's stream; the connection receives them from a separate polling goroutine
// (followStream), so a reply written directly to the outbound queue can overtake
// stream messages it claims to come after. A client applying them in arrival order
// would then apply the same execution twice — the exact failure the drain was
// introduced to prevent.
//
// Bounded: a client that is not being served within the timeout gets the message
// anyway rather than never getting one, since a late boundary is recoverable and a
// missing terminator is not.
func (sess *session) waitForStream(target uint64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for sess.emitted.Load() < target {
		select {
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		default:
		}
		if time.Now().After(deadline) {
			log.Printf("obgw: %s stream lagged past %v waiting for seq %d", sess.account, timeout, target)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// replaceOrder cancels one order and enters another atomically.
//
// Enqueued on the read loop so the replace cannot overtake the order it names; only
// the outcome is awaited elsewhere. There is no bespoke acknowledgement — a
// successful replace is the Canceled for the old ClOrdID followed by the Accepted for
// the new one, which describes it exactly, and a failure is a CmdReject naming the
// original.
func (sess *session) replaceOrder(payload []byte) {
	m, err := wire.DecodeReplaceOrder(payload)
	if err != nil || m.Version != wire.Version {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	id, ok := sess.srv.reg.OrderIDFor(sess.account, m.OrigClOrdID)
	if !ok {
		sess.reject(m.OrigClOrdID, orderentry.ReasonUnknownOrder)
		return
	}
	replacement, reason := sess.buildOrder(m.Order)
	if reason != 0 {
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	// The replacement is new liquidity, so it passes the admission gate exactly as a
	// plain Enter would. Replace must not be a way around the venue's throttle.
	if !sess.srv.gate.Allow(replacement, time.Now()) {
		sess.reject(m.Order.ClOrdID, orderentry.ReasonThrottled)
		return
	}
	done, err := sess.srv.runner.TryReplaceAsync(id, sess.account, replacement)
	if err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject(m.Order.ClOrdID, reason)
		return
	}
	go func() {
		select {
		case rerr := <-done:
			if rerr != nil {
				// The cancel half failed, so nothing was replaced and nothing new was
				// entered. Named by the ORIGINAL id: that is the order the client was
				// wrong about.
				sess.reject(m.OrigClOrdID, orderentry.ReasonFor(rerr))
			}
		case <-sess.closed:
		case <-sess.srv.quit:
		}
	}()
}

// massCancel pulls everything the account has resting.
//
// The enqueue is on the read loop so the sweep cannot overtake an order entered
// immediately before it — the ordering argument that applies to reduce applies with
// more force here, since a sweep that ran early would leave behind the very order
// the client was trying to get rid of.
//
// The acknowledgement is written only after the publisher has been drained, so every
// Canceled the sweep produced has already reached the client. Without that, an ack
// saying "12 orders cancelled" could arrive before any of the twelve Canceled
// messages, and a client applying them in order would briefly believe it had a book
// the venue had already emptied.
func (sess *session) massCancel(payload []byte) {
	if _, err := wire.DecodeMassCancel(payload); err != nil {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	done, err := sess.srv.runner.TryCancelAllAsync(sess.account)
	if err != nil {
		reason := orderentry.ReasonOverloaded
		if errors.Is(err, matching.ErrShuttingDown) {
			reason = orderentry.ReasonShuttingDown
		}
		sess.reject("", reason)
		return
	}
	go func() {
		var n int
		select {
		case n = <-done:
		case <-sess.closed:
			return
		case <-sess.srv.quit:
			return
		}
		sess.srv.pub.Wait()
		seq := sess.srv.reg.Stream(sess.account).Seq()
		sess.waitForStream(seq, 5*time.Second)
		b, encErr := wire.EncodeMassCancelAck(nil, wire.MassCancelAck{
			Version: wire.Version, Count: uint32(n), Seq: seq,
		})
		if encErr != nil {
			log.Printf("obgw: encode mass cancel ack: %v", encErr)
			return
		}
		sess.send(b)
	}()
}

// setCancelOnDisconnect turns the account's book into something that does not
// outlive this connection.
//
// Acknowledged explicitly because a client must never be guessing about a control
// that decides whether its orders survive: silence would leave "enabled" and
// "message ignored" indistinguishable.
func (sess *session) setCancelOnDisconnect(payload []byte) {
	m, err := wire.DecodeCancelOnDisconnect(payload)
	if err != nil {
		sess.reject("", orderentry.ReasonMalformed)
		return
	}
	sess.cancelOnDisconnect.Store(m.Enabled)
	b, encErr := wire.EncodeCODAck(nil, wire.CODAck{Version: wire.Version, Enabled: m.Enabled})
	if encErr != nil {
		return
	}
	sess.send(b)
}

// pullBookIfRequested runs the cancel-on-disconnect sweep. It is called once the
// connection is gone, so there is nobody to acknowledge to and nothing to wait for;
// the point is that the book does not keep quoting on behalf of a client that can no
// longer manage it.
//
// The sweep is account-wide because orders are not tagged with the session that
// entered them. An account holding a second connection loses those orders too, which
// is documented on the wire type rather than left to be discovered.
func (sess *session) pullBookIfRequested() {
	if !sess.cancelOnDisconnect.Load() {
		return
	}
	// Not when the VENUE is the one going away. A graceful shutdown drops every
	// connection at once, and firing the sweep here would journal a cancel for every
	// order held by every cancel-on-disconnect session — permanently destroying books
	// that are supposed to come back after the restart. The control means "if I lose
	// my session", not "if the venue closes for the day".
	select {
	case <-sess.srv.quit:
		return
	default:
	}
	if _, err := sess.srv.runner.TryCancelAllAsync(sess.account); err != nil {
		log.Printf("obgw: cancel-on-disconnect for %s: %v", sess.account, err)
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
	// Draining the publisher put those events in the stream; this waits until they
	// have actually been queued for THIS connection. Without it the report could
	// overtake them and QueryEnd.Seq would assert a boundary the client had not
	// reached.
	reportSeq := stream.Seq()
	sess.waitForStream(reportSeq, 5*time.Second)
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
		Version: wire.Version, Count: uint32(len(orders)), Seq: reportSeq,
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
	sess.emitted.Store(from)
	for _, m := range backlog {
		sess.emit(m)
		cursor = m.Seq
		sess.emitted.Store(m.Seq)
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
				sess.emitted.Store(m.Seq)
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
