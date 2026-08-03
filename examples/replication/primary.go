package main

import (
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// shipBuffer is each follower's in-flight allowance, in entries. A follower
// further behind than this is cut, because the alternative is the matching
// goroutine waiting on it.
const shipBuffer = 1024

// frame is one wire message, newline-delimited JSON. Exactly one field is set.
type frame struct {
	Snapshot *matching.EngineSnapshot `json:"snapshot,omitempty"`
	Entry    *wal.Entry               `json:"entry,omitempty"`
}

// hello is the follower's opening message. Have is the highest log sequence it
// has applied; zero asks for a snapshot bootstrap.
type hello struct {
	Have int64 `json:"have"`
}

// Primary is a venue that ships its command log. It is the ordinary durable
// setup — a Runner journaling to a wal.Writer — plus the Writer's OnAppend hook
// fanning each record out to followers over TCP.
type Primary struct {
	Runner *matching.Runner

	wal     *wal.Writer
	walPath string
	ln      net.Listener

	mu   sync.Mutex
	subs map[chan wal.Entry]struct{}

	shed atomic.Int64
	wg   sync.WaitGroup
}

// NewPrimary opens the log, wires the fan-out BEFORE the Runner exists — so no
// command can be journalled unobserved — and starts accepting followers.
func NewPrimary(symbol, walPath, addr string) (*Primary, error) {
	w, err := wal.Open(walPath)
	if err != nil {
		return nil, err
	}
	p := &Primary{wal: w, walPath: walPath, subs: map[chan wal.Entry]struct{}{}}
	w.SetOnAppend(p.fanout)
	p.Runner = matching.NewRunner(matching.RunnerConfig{Engine: matching.DefaultConfig(symbol), Log: w})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.Runner.Close()
		w.Close()
		return nil, err
	}
	p.ln = ln
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

// Addr is the address followers dial.
func (p *Primary) Addr() string { return p.ln.Addr().String() }

// LogSeq is the sequence of the last journalled command — the "written" side of
// the lag a follower's Applied is measured against.
func (p *Primary) LogSeq() int64 { return p.wal.Seq() }

// Shed counts followers cut for falling more than shipBuffer behind. It is the
// drop counter an operator alerts on: a rising Shed with a reconnecting
// follower is a follower that cannot keep up, not a network blip.
func (p *Primary) Shed() int64 { return p.shed.Load() }

// fanout runs on the matching goroutine, under the Writer's lock, for every
// journalled command — so it must never wait. Each follower gets a non-blocking
// send into its buffer; a follower whose buffer is full is cut on the spot.
// Cutting rather than skipping is a correctness decision, not a courtesy: a
// skipped entry would be a silent gap in that follower's stream, and a follower
// with a gap has a different book that no later entry repairs. A cut follower
// knows it was cut and reconnects with its position.
func (p *Primary) fanout(e wal.Entry) {
	p.mu.Lock()
	for ch := range p.subs {
		select {
		case ch <- e:
		default:
			delete(p.subs, ch)
			close(ch)
			p.shed.Add(1)
		}
	}
	p.mu.Unlock()
}

func (p *Primary) unsubscribe(ch chan wal.Entry) {
	p.mu.Lock()
	if _, ok := p.subs[ch]; ok {
		delete(p.subs, ch)
		close(ch)
	}
	p.mu.Unlock()
}

func (p *Primary) accept() {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.serve(conn)
		}()
	}
}

// serve feeds one follower. The order of operations at the top is the whole
// correctness argument: subscribe FIRST, snapshot second. Everything appended
// after the subscription is in the buffer; everything applied before the
// checkpoint is in the snapshot; the overlap is deduplicated by sequence on the
// follower's side. Subscribe after the snapshot instead and there is a window
// covered by neither.
func (p *Primary) serve(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	var h hello
	if dec.Decode(&h) != nil {
		return
	}

	ch := make(chan wal.Entry, shipBuffer)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	defer p.unsubscribe(ch)

	enc := json.NewEncoder(conn)
	if h.Have == 0 {
		// Checkpoint runs on the matching goroutine, so the snapshot is a real
		// point in the command order, stamped with the log position it matches.
		snap, err := p.Runner.Checkpoint()
		if err != nil || enc.Encode(frame{Snapshot: snap}) != nil {
			return
		}
	} else {
		// A reconnecting follower catches up from the file. Sync first: the live
		// stream it is already subscribed to starts at "appended after now", and
		// entries appended before now must therefore all be readable on disk.
		if p.wal.Sync() != nil {
			return
		}
		entries, err := wal.ReadAll(p.walPath)
		if err != nil {
			return
		}
		for i := range entries {
			if entries[i].Seq <= h.Have {
				continue
			}
			if enc.Encode(frame{Entry: &entries[i]}) != nil {
				return
			}
		}
	}

	for e := range ch {
		if enc.Encode(frame{Entry: &e}) != nil {
			return
		}
	}
	// The channel closed: this follower was shed by fanout, or the primary is
	// closing. Either way the deferred conn.Close tells it so.
}

// Close stops the venue: no new followers, drain the Runner (whose final
// commands still fan out — the subscriptions are cut after the drain), then
// sync and close the log.
func (p *Primary) Close() {
	p.ln.Close()
	p.Runner.Close()
	p.mu.Lock()
	for ch := range p.subs {
		delete(p.subs, ch)
		close(ch)
	}
	p.mu.Unlock()
	p.wg.Wait()
	p.wal.Sync()
	p.wal.Close()
}
