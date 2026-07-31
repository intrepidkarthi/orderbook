// Command obsoak drives a running obgw at a sustained rate for a long time and
// reports whether anything grows.
//
// It exists because docs/PRODUCTION-READINESS.md names sustained load as the largest
// unknown in this repository, and an unknown that nobody measures stays unknown. Every
// other number this project publishes comes from a microbenchmark over seconds. The
// failures this looks for do not appear in seconds: a leaked goroutine per
// disconnected client, a map that never shrinks, a descriptor that is never closed, a
// retention ring that grows because nothing evicts it. All four look perfect for the
// first minute.
//
// # What it measures, and from where
//
// Two vantage points, because they answer different questions.
//
// From the client: end-to-end latency, from writing an order onto a socket to reading
// its first response back off one. That is the number a participant experiences, and
// it is the only one the server cannot measure — the server's own histogram starts
// after the kernel has already handed it the bytes.
//
// From the venue's /metrics: heap, goroutines, descriptors, resting orders and queue
// depth, sampled on a fixed interval. Growth in any of them across a run that has
// reached steady state is the finding.
//
// # Steady state is the whole methodology
//
// A load generator that only adds orders will grow the book without limit, and then
// "memory grew" measures the workload rather than the venue. So each participant holds
// at most -resting orders and cancels its oldest to make room, and a share of the flow
// is marketable and never rests. The book plateaus, and after that any growth is the
// server's.
//
// The first -warmup of the run is excluded from the growth analysis for the same
// reason: heap climbing while caches fill and the book fills is not a leak, and
// including it would produce a positive slope on every healthy run.
//
// # Running it
//
//	ACCTS=$(obsoak -conns 50 -print-accounts)
//	obgw -addr :9000 -admin :9100 -accounts "$ACCTS" -rate 1e9 -burst 1e9 -wal /tmp/soak.wal
//	obsoak -addr :9000 -admin :9100 -conns 50 -rate 4000 -duration 30m
//
// Two things about that invocation are not incidental.
//
// One account per connection. Order entry is per-account and so is self-trade
// prevention, so running every connection as the same participant makes the engine
// correctly refuse to cross any of them with any other: the run prints no trades at
// all and measures the book path with the matching path switched off. The first
// version of this harness did exactly that — 482,394 orders, zero fills — and the
// numbers it produced looked entirely plausible.
//
// The venue's per-account rate limit lifted, or the soak measures the rate limiter.
// Leaving it in place is a different and also worthwhile test; conflating the two
// would make both unreadable.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/observability"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

// writeTimeout bounds one client send. Generous relative to any healthy response
// time, so it fires only when the venue has genuinely stopped reading.
const writeTimeout = 5 * time.Second

// drainTimeout bounds the wait for participants to stop after the run ends.
const drainTimeout = 30 * time.Second

type config struct {
	addr      string
	adminAddr string
	prefix    string
	password  string
	users     int
	symbol    string
	conns     int
	rate      float64
	duration  time.Duration
	warmup    time.Duration
	sample    time.Duration
	resting   int
	takerPct  float64
	refPrice  int64
	spread    int64
	qty       int64
	seed      int64
}

func main() {
	var cfg config
	acct := flag.String("account", "soak:pw", "account prefix and password; connection i logs in as <prefix><i%users>")
	printAccounts := flag.Bool("print-accounts", false, "print the -accounts string obgw needs for this run, and exit")
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:9000", "order-entry address")
	flag.StringVar(&cfg.adminAddr, "admin", "127.0.0.1:9100", "admin address to sample /metrics from (empty = client side only)")
	flag.StringVar(&cfg.symbol, "symbol", "BTC-USD", "instrument")
	flag.IntVar(&cfg.conns, "conns", 25, "concurrent connections")
	flag.IntVar(&cfg.users, "users", 0, "distinct accounts to spread connections over (0 = one per connection)")
	flag.Float64Var(&cfg.rate, "rate", 10000, "aggregate messages per second across all connections")
	flag.DurationVar(&cfg.duration, "duration", 5*time.Minute, "how long to sustain load")
	flag.DurationVar(&cfg.warmup, "warmup", 30*time.Second, "leading period excluded from the growth analysis")
	flag.DurationVar(&cfg.sample, "sample", 10*time.Second, "how often to scrape /metrics")
	flag.IntVar(&cfg.resting, "resting", 100, "orders each connection keeps resting before it cancels its oldest")
	flag.Float64Var(&cfg.takerPct, "taker", 0.2, "share of orders that are marketable and never rest")
	flag.Int64Var(&cfg.refPrice, "price", 100000, "reference price in ticks")
	flag.Int64Var(&cfg.spread, "spread", 50, "how far from the reference a quote may sit, in ticks")
	flag.Int64Var(&cfg.qty, "qty", 10, "order quantity in lots")
	flag.Int64Var(&cfg.seed, "seed", 1, "random seed, so a run is reproducible")
	flag.Parse()

	user, pass, ok := strings.Cut(*acct, ":")
	if !ok {
		log.Fatalf("obsoak: -account wants user:password, got %q", *acct)
	}
	cfg.prefix, cfg.password = user, pass
	if cfg.users <= 0 {
		cfg.users = cfg.conns
	}
	if *printAccounts {
		fmt.Println(cfg.accountList())
		return
	}
	// One account per connection by default, and it is not a detail. Order entry is
	// per-account, and so is self-trade prevention: run every connection as the same
	// participant and the engine correctly refuses to cross any of them with any
	// other, so the run prints no trades at all and measures the book path with the
	// matching path switched off. The first version of this harness did exactly that
	// — 482,394 orders, zero fills — and the number it produced looked plausible.
	if cfg.users < 2 {
		log.Println("obsoak: fewer than two accounts — self-trade prevention will suppress every match, and this run will measure a venue that never trades")
	}
	if cfg.warmup >= cfg.duration {
		log.Fatalf("obsoak: -warmup %s is not shorter than -duration %s; there would be no steady state to analyse", cfg.warmup, cfg.duration)
	}

	run(cfg)
}

// accountFor names the participant connection i logs in as.
func (c config) accountFor(i int) string { return fmt.Sprintf("%s%d", c.prefix, i%c.users) }

// accountList renders the -accounts string obgw needs, so a run is reproducible
// without hand-writing a credential list.
func (c config) accountList() string {
	out := make([]string, c.users)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d:%s", c.prefix, i, c.password)
	}
	return strings.Join(out, ",")
}

// stats is what every connection contributes to and the report reads.
type stats struct {
	sent     atomic.Int64
	acked    atomic.Int64
	rejected atomic.Int64
	fills    atomic.Int64
	errors   atomic.Int64
	// orphans are sends whose response never arrived before the run ended. A handful
	// at the end is the pipeline draining; a large number is the finding.
	orphans atomic.Int64
	latency *observability.Histogram

	// Rejections by reason. A soak that reports "22,937 rejects" and not why is
	// telling an operator that something is wrong and nothing about what — and the
	// first run of this harness produced exactly that number and no way to read it.
	reasonMu sync.Mutex
	reasons  map[string]int64

	// believed is the sum, across participants, of orders they think are resting. It
	// is compared against the venue's own count at the end: a client-side model that
	// has silently diverged from the book is the failure this harness exists to
	// notice, and it cannot notice it by looking at either number alone.
	believed atomic.Int64
}

func (s *stats) countReason(kind string, code uint16) {
	s.reasonMu.Lock()
	s.reasons[kind+" "+reasonText(code)]++
	s.reasonMu.Unlock()
}

// retryable reports whether a refused command would succeed later. Backpressure and
// throttling would; an order the venue has never heard of would not, however many
// times it is asked.
func retryable(code uint16) bool {
	switch code {
	case orderentry.ReasonOverloaded, orderentry.ReasonThrottled:
		return true
	}
	return false
}

func reasonText(code uint16) string {
	switch code {
	case orderentry.ReasonNone:
		return "none"
	case orderentry.ReasonOther:
		return "other"
	case orderentry.ReasonUnknownOrder:
		return "unknown order"
	case orderentry.ReasonDuplicateClOrd:
		return "duplicate ClOrdID"
	case orderentry.ReasonTooSmall:
		return "too small"
	case orderentry.ReasonTooLarge:
		return "too large"
	case orderentry.ReasonPriceBand:
		return "price band"
	case orderentry.ReasonSelfTrade:
		return "self-trade prevention"
	case orderentry.ReasonPostOnlyCross:
		return "post-only would cross"
	case orderentry.ReasonFOKCannotFill:
		return "FOK cannot fill"
	case orderentry.ReasonHalted:
		return "halted"
	case orderentry.ReasonThrottled:
		return "rate limited"
	case orderentry.ReasonOverloaded:
		return "queue full"
	case orderentry.ReasonNotAuthorised:
		return "not authorised"
	case orderentry.ReasonMalformed:
		return "malformed"
	case orderentry.ReasonShuttingDown:
		return "shutting down"
	case orderentry.ReasonInvalidQuantity:
		return "invalid quantity"
	case orderentry.ReasonTooSoon:
		return "minimum resting time"
	}
	return "code " + strconv.Itoa(int(code))
}

func run(cfg config) {
	st := &stats{latency: observability.NewHistogram(), reasons: map[string]int64{}}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	fmt.Printf("obsoak: %d connections over %d accounts against %s, target %s msg/s for %s (warmup %s)\n",
		cfg.conns, cfg.users, cfg.addr, comma(int64(cfg.rate)), cfg.duration, cfg.warmup)

	perConn := cfg.rate / float64(cfg.conns)
	for i := 0; i < cfg.conns; i++ {
		p, err := newParticipant(cfg, i, st, perConn)
		if err != nil {
			log.Fatalf("obsoak: connection %d: %v", i, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.run(stop)
		}()
	}

	samples := make(chan sample, 1024)
	var samplerWG sync.WaitGroup
	if cfg.adminAddr != "" {
		samplerWG.Add(1)
		go func() {
			defer samplerWG.Done()
			sampleVenue(cfg, stop, samples)
		}()
	}

	probeBefore := speedProbe()
	start := time.Now()
	select {
	case <-time.After(cfg.duration):
	case <-sigs:
		fmt.Println("\nobsoak: interrupted; reporting what was collected")
	}
	close(stop)
	// Bounded. Every participant should return promptly once its socket is closed, and
	// the report is the deliverable — waiting forever for a straggler would throw away
	// everything the run learned in order to be tidy about how it ended.
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		fmt.Printf("obsoak: %s after stopping, some connections had not drained; reporting anyway\n", drainTimeout)
	}
	samplerWG.Wait()
	close(samples)

	collected := make([]sample, 0, 1024)
	for s := range samples {
		collected = append(collected, s)
	}
	elapsed := time.Since(start)
	probeAfter := speedProbe()
	report(cfg, st, collected, elapsed, probeBefore, probeAfter)
}

// --- one participant ---------------------------------------------------------

type participant struct {
	cfg    config
	id     int
	st     *stats
	conn   net.Conn
	rng    *rand.Rand
	perSec float64
	seq    int64

	// mu guards everything the reader and the writer both touch. The reader has to
	// reach the resting list because the writer's belief about what is in the book is
	// only correct if rejected cancels are put back — the first version dropped the
	// id the moment it sent the cancel, so every cancel the venue refused leaked an
	// order that no participant would ever cancel again, and the book grew to the
	// engine's MaxOrders ceiling while the harness reported a bounded workload.
	mu       sync.Mutex
	inflight map[string]time.Time
	resting  []string // ids believed live, oldest first
}

// dropResting removes an id the venue has told us is not resting after all.
func (p *participant) dropResting(id string) {
	for i, v := range p.resting {
		if v == id {
			p.resting = append(p.resting[:i], p.resting[i+1:]...)
			return
		}
	}
}

func newParticipant(cfg config, id int, st *stats, perSec float64) (*participant, error) {
	conn, err := net.Dial("tcp", cfg.addr)
	if err != nil {
		return nil, err
	}
	p := &participant{
		cfg: cfg, id: id, st: st, conn: conn, perSec: perSec,
		rng:      rand.New(rand.NewSource(cfg.seed + int64(id))),
		resting:  make([]string, 0, cfg.resting+1),
		inflight: make(map[string]time.Time, 1024),
	}
	if err := p.login(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return p, nil
}

func (p *participant) login() error {
	b, err := wire.EncodeLoginRequest(nil, wire.LoginRequest{
		Username: p.cfg.accountFor(p.id), Password: p.cfg.password, Sequence: 0,
	})
	if err != nil {
		return err
	}
	if err := wire.WritePacket(p.conn, wire.PacketLoginRequest, b); err != nil {
		return err
	}
	_ = p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	pkt, err := wire.ReadPacket(p.conn, make([]byte, wire.MaxPayload))
	if err != nil {
		return err
	}
	if pkt.Type != wire.PacketLoginAccepted {
		return fmt.Errorf("login refused (packet %q)", pkt.Type)
	}
	_ = p.conn.SetReadDeadline(time.Time{})
	return nil
}

func (p *participant) run(stop <-chan struct{}) {
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		p.readLoop(stop)
	}()

	// One exit, and it closes the socket before waiting for the reader.
	//
	// The reader is parked in a blocking read with no deadline, which is what a real
	// client does; closing the connection is the only thing that releases it. The
	// error path used to wait for it WITHOUT closing, which is a deadlock — and it was
	// unreachable until client sends got a deadline of their own, at which point a
	// slow venue could fail a write and hang the harness on the way out. A fix for one
	// problem making a latent second one reachable is the ordinary case, not a
	// surprise.
	defer func() {
		_ = p.conn.Close()
		readerWG.Wait()
		p.mu.Lock()
		p.st.orphans.Add(int64(len(p.inflight)))
		p.st.believed.Add(int64(len(p.resting)))
		p.mu.Unlock()
	}()

	// Deadline pacing rather than a ticker per message: at ten thousand messages a
	// second a ticker's own wakeups become the thing being measured, and drift
	// accumulates so the run finishes short of its target rate without saying so.
	interval := time.Duration(float64(time.Second) / p.perSec)
	next := time.Now()
	for {
		select {
		case <-stop:
			return
		default:
		}
		next = next.Add(interval)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
		if err := p.act(); err != nil {
			p.st.errors.Add(1)
			return
		}
	}
}

// act sends one message: a marketable order that will not rest, a quote, or a cancel
// of this participant's oldest quote once it is holding its share of the book.
func (p *participant) act() error {
	p.mu.Lock()
	atCap := len(p.resting) >= p.cfg.resting
	var oldest string
	if atCap {
		oldest = p.resting[0]
		p.resting = p.resting[1:]
	}
	p.mu.Unlock()
	if atCap {
		return p.send(wire.MsgCancel, oldest, 0, 0, 0)
	}
	id := p.nextID()
	if p.rng.Float64() < p.cfg.takerPct {
		// Marketable IOC: it trades against whatever is there or it is gone. Either
		// way it does not rest, which is what keeps the book bounded while still
		// exercising the matching path rather than just the book path.
		side := wire.SideBuy
		price := p.cfg.refPrice + p.cfg.spread
		if p.rng.Intn(2) == 0 {
			side, price = wire.SideSell, p.cfg.refPrice-p.cfg.spread
		}
		return p.send(wire.MsgEnter, id, side, price, wire.TIFImmediateOrCanc)
	}
	// A quote, offset from the reference so the book has depth rather than one level.
	side := wire.SideBuy
	price := p.cfg.refPrice - 1 - p.rng.Int63n(p.cfg.spread)
	if p.rng.Intn(2) == 0 {
		side, price = wire.SideSell, p.cfg.refPrice+1+p.rng.Int63n(p.cfg.spread)
	}
	p.mu.Lock()
	p.resting = append(p.resting, id)
	p.mu.Unlock()
	return p.send(wire.MsgEnter, id, side, price, wire.TIFGoodTillCancel)
}

func (p *participant) nextID() string {
	p.seq++
	return fmt.Sprintf("%d-%d", p.id, p.seq)
}

func (p *participant) send(msgType uint8, id string, side uint8, price int64, tif uint8) error {
	var (
		b   []byte
		err error
	)
	switch msgType {
	case wire.MsgEnter:
		b, err = wire.EncodeEnter(nil, wire.Enter{
			Version: wire.Version, ClOrdID: id, Symbol: p.cfg.symbol,
			Side: side, Type: wire.TypeLimit, TIF: tif,
			Price: price, Quantity: p.cfg.qty,
		})
	case wire.MsgCancel:
		b, err = wire.EncodeCancel(nil, wire.Cancel{Version: wire.Version, ClOrdID: id})
	}
	if err != nil {
		return err
	}
	// Timestamped before the write, not after: the write is part of what is being
	// measured, and a socket buffer that has filled is exactly the latency a
	// participant would feel.
	p.mu.Lock()
	p.inflight[id] = time.Now()
	p.mu.Unlock()

	// A deadline, because a saturated venue stops draining its socket and a blocking
	// write then lasts as long as the backlog does. Without it the run still ends —
	// this is not a hang — but the report arrives whenever the writes happen to clear,
	// which on a 7-minute saturated run was six minutes late. A soak whose findings
	// turn up long after the soak is a soak nobody will run overnight.
	//
	// It is also what a real client does. One with no send timeout is not a model of
	// anything.
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := wire.WritePacket(p.conn, wire.PacketUnsequenced, b); err != nil {
		p.mu.Lock()
		delete(p.inflight, id)
		p.mu.Unlock()
		return err
	}
	p.st.sent.Add(1)
	return nil
}

func (p *participant) readLoop(stop <-chan struct{}) {
	buf := make([]byte, wire.MaxPayload)
	for {
		pkt, err := wire.ReadPacket(p.conn, buf)
		if err != nil {
			select {
			case <-stop:
			default:
				if err != io.EOF {
					p.st.errors.Add(1)
				}
			}
			return
		}
		if pkt.Type != wire.PacketSequencedData {
			continue // a heartbeat
		}
		msgType, ok := wire.MsgTypeOf(pkt.Payload)
		if !ok {
			continue
		}
		var id string
		switch msgType {
		case wire.MsgAccepted:
			m, err := wire.DecodeAccepted(pkt.Payload)
			if err != nil {
				continue
			}
			id = m.ClOrdID
			p.st.acked.Add(1)
		case wire.MsgExecuted:
			m, err := wire.DecodeExecuted(pkt.Payload)
			if err != nil {
				continue
			}
			id = m.ClOrdID
			p.st.fills.Add(1)
		case wire.MsgRejected:
			m, err := wire.DecodeRejected(pkt.Payload)
			if err != nil {
				continue
			}
			id = m.ClOrdID
			p.st.rejected.Add(1)
			p.st.countReason("order", m.Reason)
			// The order never rested, so the optimistic append has to come back out.
			p.mu.Lock()
			p.dropResting(id)
			p.mu.Unlock()
		case wire.MsgCanceled:
			m, err := wire.DecodeCanceled(pkt.Payload)
			if err != nil {
				continue
			}
			id = m.ClOrdID
			p.st.acked.Add(1)
		case wire.MsgCmdReject:
			m, err := wire.DecodeCmdReject(pkt.Payload)
			if err != nil {
				continue
			}
			id = m.ClOrdID
			p.st.rejected.Add(1)
			p.st.countReason("command", m.Reason)
			// A cancel the venue refused for a transient reason leaves the order
			// resting, so it goes back at the front of the queue to be retried. Refused
			// because the order is unknown means it is genuinely gone and there is
			// nothing to put back.
			if retryable(m.Reason) {
				p.mu.Lock()
				p.resting = append([]string{id}, p.resting...)
				p.mu.Unlock()
			}
		default:
			continue
		}
		// First response only. A cancel names the order it is cancelling, so the id
		// comes back a second time; by then it is out of the map and the later
		// arrival is correctly ignored rather than recorded as a suspiciously fast
		// round trip.
		p.mu.Lock()
		sentAt, live := p.inflight[id]
		if live {
			delete(p.inflight, id)
		}
		p.mu.Unlock()
		if live {
			p.st.latency.Observe(time.Since(sentAt))
		}
	}
}

// --- sampling the venue ------------------------------------------------------

type sample struct {
	at         time.Duration
	heap       float64
	goroutines float64
	fds        float64
	resting    float64
	queue      float64
	queueCap   float64
	trades     float64
	events     float64
}

func sampleVenue(cfg config, stop <-chan struct{}, out chan<- sample) {
	start := time.Now()
	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(cfg.sample)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		body, err := scrape(client, cfg.adminAddr)
		if err != nil {
			log.Printf("obsoak: scrape: %v", err)
			continue
		}
		s := sample{
			at:         time.Since(start),
			heap:       body["obgw_heap_bytes"],
			goroutines: body["obgw_goroutines"],
			fds:        body["obgw_open_files"],
			resting:    body["orderbook_resting_orders"],
			queue:      body["orderbook_queue_depth"],
			queueCap:   body["orderbook_queue_capacity"],
			trades:     body["orderbook_trades_total"],
			events:     body["orderbook_events_total"],
		}
		select {
		case out <- s:
		default: // the report is bounded; dropping a sample beats blocking the sampler
		}
	}
}

// scrape reads the exposition format into a map, ignoring labelled series — nothing
// this harness reports on has labels, and parsing them would mean deciding which one
// of a family it meant.
func scrape(client *http.Client, addr string) (map[string]float64, error) {
	resp, err := client.Get("http://" + addr + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, val, ok := strings.Cut(line, " ")
		if !ok || strings.ContainsRune(name, '{') {
			continue
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		out[name] = v
	}
	return out, sc.Err()
}

// --- how much of the machine did this run actually get? -----------------------

// probeSink keeps the compiler from deleting the probe's arithmetic.
var probeSink uint64

// speedProbe runs a fixed amount of arithmetic and reports how long it took.
//
// Every absolute figure in this report — throughput, latency, the rate at which the
// queue saturates — is only comparable to another run's if both got the same share of
// the machine. On a dedicated box that is a safe assumption. On anything that is also
// running a browser and a window server it is not, and the first version of this
// harness published capacity numbers that did not reproduce four hours later on the
// same machine, on the same code, because the machine had got busier in between.
//
// A load average would answer this on Linux and need a different mechanism on Darwin.
// This needs neither, and it measures the thing that actually matters — how much CPU
// this process can get — rather than a number the kernel keeps about everybody.
func speedProbe() time.Duration {
	start := time.Now()
	x := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	probeSink = x
	return time.Since(start)
}

// --- the report --------------------------------------------------------------

func report(cfg config, st *stats, samples []sample, elapsed time.Duration, probeBefore, probeAfter time.Duration) {
	sent := st.sent.Load()
	fmt.Printf("\n=== obsoak: %s over %s, %d connections ===\n\n", cfg.symbol, elapsed.Truncate(time.Second), cfg.conns)

	// First, because it decides whether anything below is comparable to another run.
	fmt.Printf("machine   fixed-work probe %s before, %s after", probeBefore.Truncate(time.Millisecond), probeAfter.Truncate(time.Millisecond))
	if drift := probeDrift(probeBefore, probeAfter); drift > 0.15 {
		fmt.Printf("  — %.0f%% APART\n", 100*drift)
		fmt.Println("          This process got a different share of the machine at the two ends of the run.")
		fmt.Println("          The absolute figures below are not comparable with another run's; the")
		fmt.Println("          structural ones — orphaned orders, leaked goroutines — still are.")
	} else {
		fmt.Println("  — stable")
	}
	fmt.Println()

	fmt.Println("throughput")
	fmt.Printf("  sent          %12s   (%s/s, target %s/s)\n",
		comma(sent), comma(int64(float64(sent)/elapsed.Seconds())), comma(int64(cfg.rate)))
	fmt.Printf("  acked         %12s\n", comma(st.acked.Load()))
	fmt.Printf("  fills         %12s\n", comma(st.fills.Load()))
	fmt.Printf("  rejected      %12s   (%.3f%%)\n", comma(st.rejected.Load()), pct(st.rejected.Load(), sent))
	fmt.Printf("  errors        %12s\n", comma(st.errors.Load()))
	fmt.Printf("  unanswered    %12s   (%.3f%%, sends whose response never arrived)\n",
		comma(st.orphans.Load()), pct(st.orphans.Load(), sent))
	st.reasonMu.Lock()
	names := make([]string, 0, len(st.reasons))
	for name := range st.reasons {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return st.reasons[names[i]] > st.reasons[names[j]] })
	for _, name := range names {
		fmt.Printf("    %-28s %12s\n", name, comma(st.reasons[name]))
	}
	st.reasonMu.Unlock()

	fmt.Println("\nclient-observed latency, socket write to first response")
	if st.latency.Count() == 0 {
		fmt.Println("  no responses observed")
	} else {
		fmt.Printf("  samples       %12s\n", comma(st.latency.Count()))
		for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
			fmt.Printf("  p%-5s        %12s\n", strconv.FormatFloat(q*100, 'g', -1, 64), dur(st.latency.Quantile(q)))
		}
		fmt.Printf("  mean          %12s\n", dur(st.latency.Sum()/st.latency.Count()))
		fmt.Println("  (bucketed: each figure is the bucket's upper bound, not an exact value)")
	}

	if len(samples) == 0 {
		fmt.Println("\nvenue: not sampled (no -admin address, or no successful scrape)")
		fmt.Println("\nVERDICT: inconclusive — client side only. Growth is what this harness is for.")
		return
	}

	fmt.Println("\nvenue")
	fmt.Printf("  %8s  %10s  %10s  %6s  %10s  %6s\n", "t", "heap", "goroutines", "fds", "resting", "queue")
	for _, s := range samples {
		fmt.Printf("  %8s  %10s  %10.0f  %6.0f  %10s  %6.0f\n",
			s.at.Truncate(time.Second), bytesOf(s.heap), s.goroutines, s.fds, comma(int64(s.resting)), s.queue)
	}

	// The cross-check. Participants know what they think is resting; the venue knows
	// what is. They should agree to within the orders that were in flight when the
	// run stopped, and when they do not, every order in the gap is one no client can
	// cancel because no client believes it exists.
	last := samples[len(samples)-1]
	believed := st.believed.Load()
	fmt.Printf("\nbook, as the clients see it and as the venue does\n")
	fmt.Printf("  participants believe   %12s resting\n", comma(believed))
	fmt.Printf("  venue reports          %12s resting\n", comma(int64(last.resting)))
	if gap := int64(last.resting) - believed; gap > believed/10+100 {
		fmt.Printf("  *** ORPHANED: %s orders rest in the book that no participant is tracking.\n", comma(gap))
		fmt.Printf("      Nothing will ever cancel them. They occupy the book until the venue restarts.\n")
	}

	steady := make([]sample, 0, len(samples))
	for _, s := range samples {
		if s.at >= cfg.warmup {
			steady = append(steady, s)
		}
	}
	if len(steady) < 3 {
		fmt.Printf("\nVERDICT: inconclusive — only %d samples after the %s warmup. Run longer.\n", len(steady), cfg.warmup)
		return
	}

	window := steady[len(steady)-1].at - steady[0].at
	fmt.Printf("\nsteady state: %d samples over %s, after a %s warmup\n", len(steady), window.Truncate(time.Second), cfg.warmup)

	// A run whose command queue sat full measured the slowest thing in the pipeline,
	// not the venue's behaviour over time. Every other number below is still true and
	// none of them means what it looks like it means, so this is said before them
	// rather than after.
	var saturated int
	for _, s := range steady {
		if s.queueCap > 0 && s.queue/s.queueCap > 0.95 {
			saturated++
		}
	}
	if saturated*2 > len(steady) {
		fmt.Printf("\n  *** SATURATED: the command queue was full in %d of %d samples.\n", saturated, len(steady))
		fmt.Printf("      This run measured the venue's capacity limit, not its behaviour over time.\n")
		fmt.Printf("      Lower -rate until the queue drains, then the growth analysis means something.\n")
	}

	type series struct {
		name   string
		get    func(sample) float64
		format func(float64) string
		// leak reports whether a rising floor here means something is being leaked
		// rather than a workload that has simply not finished filling.
		leak bool
	}
	all := []series{
		{"heap", func(s sample) float64 { return s.heap }, bytesOf, true},
		{"goroutines", func(s sample) float64 { return s.goroutines }, round, true},
		{"descriptors", func(s sample) float64 { return s.fds }, round, true},
		{"resting orders", func(s sample) float64 { return s.resting }, round, false},
	}
	var findings []string
	for _, ser := range all {
		// The floor, not the peak, and not the endpoints.
		//
		// Live heap saw-tooths under Go's GC pacing: it climbs to the next target and
		// drops, so a sample taken at a peak and a sample taken at a trough differ by
		// more than any leak would in the same window. What a leak moves is the floor
		// — the low-water mark is data that survived a collection. Comparing the floor
		// of the first half against the floor of the second is robust to where in the
		// cycle the scrapes happened to land, and a least-squares fit through raw
		// samples is not.
		half := len(steady) / 2
		lo, hi := floorOf(steady[:half], ser.get), floorOf(steady[half:], ser.get)
		slope := slopePerHour(steady, ser.get)
		sign := "+"
		if slope < 0 {
			sign = "-"
		}
		fmt.Printf("  %-15s floor %10s -> %10s   trend %s%s/hour\n",
			ser.name, ser.format(lo), ser.format(hi), sign, ser.format(math.Abs(slope)))
		if !ser.leak || lo <= 0 {
			continue
		}
		// Proportional, not absolute: a hundred bytes an hour is noise on a 40 MiB
		// heap and a fleet-ending leak on a 4 KiB one.
		if growth := (hi - lo) / lo; growth > 0.10 {
			findings = append(findings, fmt.Sprintf("%s floor up %.1f%%", ser.name, 100*growth))
		}
	}

	fmt.Println()
	// Saturation outranks everything below it. A run whose queue never drained was
	// measuring the slowest thing in the pipeline, and a growth figure taken from it
	// describes a backlog rather than a leak. This used to be printed as a banner and
	// then contradicted by a VERDICT line announcing growth — and the VERDICT line is
	// the one people read.
	if saturated*2 > len(steady) {
		fmt.Printf("VERDICT: saturated at %s msg/s — the queue never drained, so nothing here is a statement about growth.\n",
			comma(int64(float64(sent)/elapsed.Seconds())))
		if len(findings) > 0 {
			fmt.Printf("(%s, which is what a permanent backlog looks like. Not a leak until a run that is not saturated says so.)\n",
				strings.Join(findings, "; "))
		}
		fmt.Println("Find the sustainable rate first: halve -rate until the queue sits near zero.")
		return
	}

	// A window this short cannot distinguish a leak from GC pacing, from a book that
	// is still filling, from a cache that has not warmed — and it cannot do so in
	// EITHER direction. The gate used to apply only when nothing had been found, so a
	// twenty-nine-second run could still announce "heap floor up 32.7%". Saying "no
	// growth" on four minutes and saying "growth" on twenty-nine seconds are the same
	// mistake, and both are sentences somebody quotes.
	if window < minGrowthWindow {
		fmt.Printf("VERDICT: inconclusive — %s of steady state is too short to distinguish a leak from GC pacing.\n",
			window.Truncate(time.Second))
		if len(findings) > 0 {
			fmt.Printf("Worth a longer run rather than a conclusion: %s.\n", strings.Join(findings, "; "))
		} else {
			fmt.Println("Nothing grew, and that is not evidence of anything.")
		}
		fmt.Printf("Run for at least %s.\n", minGrowthWindow)
		return
	}
	switch {
	case len(findings) > 0:
		fmt.Printf("VERDICT: growth detected over %s of steady state — %s.\n", window.Truncate(time.Second), strings.Join(findings, "; "))
		fmt.Println("A longer run will say whether it plateaus. Treat this as a lead, not a conclusion.")
	default:
		fmt.Printf("VERDICT: no growth in heap, goroutines or descriptors over %s of steady state at %s msg/s.\n",
			window.Truncate(time.Second), comma(int64(float64(sent)/elapsed.Seconds())))
		fmt.Printf("This is evidence for %s, and for nothing longer.\n", window.Truncate(time.Second))
	}
}

// probeDrift is the fractional difference between two probe timings.
func probeDrift(a, b time.Duration) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	lo, hi := float64(a), float64(b)
	if lo > hi {
		lo, hi = hi, lo
	}
	return (hi - lo) / lo
}

// minGrowthWindow is the shortest steady-state window this tool will draw a
// conclusion from. Below it, "nothing grew" is not a finding.
const minGrowthWindow = 5 * time.Minute

// floorOf is the low-water mark of a series: what survived every collection in the
// window, rather than whatever the last scrape happened to catch.
func floorOf(ss []sample, get func(sample) float64) float64 {
	if len(ss) == 0 {
		return 0
	}
	min := get(ss[0])
	for _, s := range ss[1:] {
		if v := get(s); v < min {
			min = v
		}
	}
	return min
}

// slopePerHour is an ordinary least-squares fit, in units per hour. A first-versus-last
// comparison would be at the mercy of whichever moment the last scrape landed in;
// every sample should get a vote.
func slopePerHour(ss []sample, get func(sample) float64) float64 {
	var sumX, sumY, sumXY, sumXX float64
	n := float64(len(ss))
	for _, s := range ss {
		x := s.at.Hours()
		y := get(s)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / den
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func dur(ns int64) string { return time.Duration(ns).String() }

func round(v float64) string { return comma(int64(math.Round(v))) }

func bytesOf(v float64) string {
	const unit = 1024.0
	if math.Abs(v) < unit {
		return fmt.Sprintf("%.0f B", v)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	sign := 1.0
	if v < 0 {
		sign, v = -1, -v
	}
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", sign*v, units[i])
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
