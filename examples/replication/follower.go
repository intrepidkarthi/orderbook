package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/wal"
)

// Follower is a warm standby: an engine bootstrapped from the primary's
// snapshot that then never stops replaying. There is no Runner here until
// promotion — the tail goroutine is the engine's single writer, and the mutex
// extends that discipline to the read-side accessors.
//
// The follower runs the same Config as the primary, and that is an obligation,
// not a convenience: replay mode bypasses the time-based admission controls,
// but everything else about matching — pro-rata, bands, caps — is Config, and
// a follower configured differently is a divergence generator with a working
// network connection.
type Follower struct {
	symbol string
	conn   net.Conn
	done   chan struct{}

	mu      sync.Mutex
	engine  *matching.Engine
	applied int64 // highest log sequence applied
	err     error // terminal protocol violation; nil on a clean end of feed
}

// StartFollower dials the primary and begins tailing. Have=0: the primary
// answers with a snapshot taken mid-stream, then entries.
func StartFollower(symbol, addr string) (*Follower, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(hello{Have: 0}); err != nil {
		conn.Close()
		return nil, err
	}
	f := &Follower{symbol: symbol, conn: conn, done: make(chan struct{})}
	go f.tail()
	return f, nil
}

// tail applies the feed in arrival order. The sequence arithmetic is the
// protocol's entire integrity story, so it is explicit rather than delegated:
// an entry at or below applied is the expected overlap between the snapshot and
// the live stream and is skipped; applied+1 is applied; anything further ahead
// is a gap, and a gap is terminal — a book missing one command is not behind,
// it is different, and no later entry repairs it.
func (f *Follower) tail() {
	defer close(f.done)
	defer f.conn.Close()
	dec := json.NewDecoder(f.conn)
	for {
		var fr frame
		if err := dec.Decode(&fr); err != nil {
			// EOF or a closed connection is the normal end of a feed — the
			// primary died, shed us, or Promote hung up. Not an error: what to
			// do about a dead feed is the operator's decision, not the tail's.
			return
		}
		switch {
		case fr.Snapshot != nil:
			eng, err := matching.RestoreEngine(matching.DefaultConfig(f.symbol), fr.Snapshot)
			if err != nil {
				f.fail(fmt.Errorf("bootstrap: %w", err))
				return
			}
			f.mu.Lock()
			f.engine = eng
			f.applied = fr.Snapshot.WALSeq
			f.mu.Unlock()
		case fr.Entry != nil:
			f.mu.Lock()
			switch e := *fr.Entry; {
			case f.engine == nil:
				f.mu.Unlock()
				f.fail(errors.New("entry before snapshot"))
				return
			case e.Seq <= f.applied:
				f.mu.Unlock() // snapshot/live overlap: already have it
			case e.Seq == f.applied+1:
				wal.RestoreAfter(f.engine, []wal.Entry{e}, f.applied)
				f.applied = e.Seq
				f.mu.Unlock()
			default:
				f.mu.Unlock()
				f.fail(fmt.Errorf("gap in the feed: applied %d, received %d", f.applied, e.Seq))
				return
			}
		}
	}
}

func (f *Follower) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// Err reports a terminal protocol violation (a gap, an entry before any
// snapshot). A feed that merely ended returns nil.
func (f *Follower) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Applied is the highest log sequence applied — the follower's half of the lag
// gauge. The primary's LogSeq minus this is the loss window, in commands.
func (f *Follower) Applied() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied
}

// Digest fingerprints the follower's book (EngineSnapshot.Digest). Equal to the
// primary's digest at the same applied sequence, or one of them has diverged.
func (f *Follower) Digest() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.engine == nil {
		return "", errors.New("no snapshot received yet")
	}
	return f.engine.TakeSnapshot().Digest(), nil
}

// WaitApplied blocks until the follower has applied seq, or gives up. Purely a
// convenience for demos and drills — production watches Applied as a gauge.
func (f *Follower) WaitApplied(seq int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if f.Applied() >= seq {
			return nil
		}
		if err := f.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: applied %d, want %d", f.Applied(), seq)
		}
		select {
		case <-f.done:
			if f.Applied() >= seq {
				return nil
			}
			if err := f.Err(); err != nil {
				return err
			}
			return fmt.Errorf("feed ended at %d, want %d", f.Applied(), seq)
		case <-time.After(time.Millisecond):
		}
	}
}

// Promote turns the standby into a venue: stop the tail, write the promoted
// book as the new venue's base snapshot, open a fresh log, and hand the engine
// to a Runner — from here it is the ordinary durable setup.
//
// What Promote does NOT do is as load-bearing as what it does. It does not mint
// the new incarnation id — the caller does, for its orderentry.Registry — and
// that id is the fence: sequence numbers are scoped to one incarnation, so a
// client resuming with the dead primary's cursor is refused instead of served
// different content under numbers it believes it already has. And it does not
// fence the old primary itself: a partitioned primary that still runs is a
// consensus problem, out of scope by design (docs/REPLICATION.md §5, §8).
func (f *Follower) Promote(walPath, snapPath string) (*matching.Runner, error) {
	f.conn.Close()
	<-f.done

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.engine == nil {
		return nil, errors.New("promote: no snapshot ever arrived")
	}
	if f.err != nil {
		return nil, fmt.Errorf("promote: refusing a book with a known defect: %w", f.err)
	}

	// The new venue's recovery base. Its WALSeq is zeroed because positions in
	// the old primary's log mean nothing to the log this venue is about to
	// start; leaving it would point recovery at a tail that does not exist.
	snap := f.engine.TakeSnapshot()
	snap.WALSeq = 0
	if err := wal.WriteSnapshot(snapPath, snap); err != nil {
		return nil, fmt.Errorf("promote: base snapshot: %w", err)
	}
	w, err := wal.Open(walPath)
	if err != nil {
		return nil, fmt.Errorf("promote: new log: %w", err)
	}
	runner := matching.NewRunnerFor(f.engine, matching.RunnerConfig{Log: w})
	f.engine = nil // the Runner's goroutine owns it now; the tail must not
	return runner, nil
}
