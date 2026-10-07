// Command obquote is a live market maker for a running obgw: it keeps a ladder of
// two-sided quotes resting, refreshes it on an interval around a mid that wanders,
// and leans the ladder against the inventory its fills give it.
//
// It exists so a venue started from compose.yaml has a book worth looking at
// (docs/EXCHANGE-IN-A-BOX.md). It is a demonstration client, not a strategy:
// cmd/obmm and examples/marketmaker are where quoting is studied.
//
//	obquote -addr 127.0.0.1:9000 -account mm:secret -symbol BTC-USD
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
)

type config struct {
	addr, user, password, symbol string
	mid, half, step, qty, walk   int64
	levels                       int
	skew                         float64
	refresh, duration            time.Duration
	seed                         int64
}

func main() {
	var c config
	account := flag.String("account", "mm:mm", "user:password")
	flag.StringVar(&c.addr, "addr", "127.0.0.1:9000", "obgw order-entry address")
	flag.StringVar(&c.symbol, "symbol", "BTC-USD", "instrument")
	flag.Int64Var(&c.mid, "mid", 100000, "starting mid, in ticks")
	flag.Int64Var(&c.half, "half-spread", 5, "ticks from the mid to the best quote on each side")
	flag.Int64Var(&c.step, "step", 2, "ticks between ladder levels")
	flag.IntVar(&c.levels, "levels", 5, "quotes on each side")
	flag.Int64Var(&c.qty, "qty", 10, "lots per quote")
	flag.Int64Var(&c.walk, "walk", 3, "largest move of the mid per refresh, in ticks")
	flag.Float64Var(&c.skew, "skew", 0.1, "ticks the ladder leans per lot of inventory")
	flag.DurationVar(&c.refresh, "refresh", time.Second, "how often the ladder is replaced")
	flag.DurationVar(&c.duration, "duration", 0, "stop after this long (0: run until killed)")
	flag.Int64Var(&c.seed, "seed", 1, "random seed for the mid's walk")
	flag.Parse()
	var ok bool
	if c.user, c.password, ok = strings.Cut(*account, ":"); !ok {
		log.Fatalf("obquote: -account %q is not user:password", *account)
	}
	if err := run(c, os.Stdout); err != nil {
		log.Fatalf("obquote: %v", err)
	}
}

// ladder is the prices of one refresh: levels bids below mid and levels asks above
// it, best first, never crossing.
func ladder(mid, half, step int64, levels int) (bids, asks []int64) {
	if half < 1 {
		half = 1
	}
	for i := 0; i < levels; i++ {
		off := half + int64(i)*step
		bids = append(bids, mid-off)
		asks = append(asks, mid+off)
	}
	return bids, asks
}

// quoter holds the live state the read loop and the refresh loop share.
type quoter struct {
	mu        sync.Mutex
	sides     map[string]uint8 // live ClOrdID -> side
	inventory int64
	fills     int64
	rejects   int64
}

func run(c config, out io.Writer) error {
	conn, err := dial(c)
	if err != nil {
		return err
	}
	defer conn.Close()
	q := &quoter{sides: map[string]uint8{}}
	readErr := make(chan error, 1)
	go func() { readErr <- q.read(conn) }()

	rng := rand.New(rand.NewSource(c.seed))
	mid, seq := c.mid, 0
	// A prefix per run, so a quoter restarted after the venue drops it never reuses
	// an id the venue still remembers. Base 36 seconds plus a counter fit the wire's
	// 20-byte ClOrdID for years.
	prefix := strconv.FormatInt(time.Now().Unix(), 36)
	var stop <-chan time.Time
	if c.duration > 0 {
		stop = time.After(c.duration)
	}
	tick := time.NewTicker(c.refresh)
	defer tick.Stop()
	for n := 0; ; n++ {
		// Cancel the last ladder, then quote the next one around a mid that has
		// moved, leaning against inventory so a one-sided flow pushes the quotes away.
		q.mu.Lock()
		var old []string
		for id := range q.sides {
			old = append(old, id)
		}
		inv := q.inventory
		q.mu.Unlock()
		for _, id := range old {
			if err := send(conn, wire.Cancel{Version: wire.Version, ClOrdID: id}); err != nil {
				return err
			}
		}
		mid += rng.Int63n(2*c.walk+1) - c.walk
		center := mid - int64(c.skew*float64(inv))
		bids, asks := ladder(center, c.half, c.step, c.levels)
		for i := range bids {
			for _, s := range []struct {
				side  uint8
				price int64
			}{{wire.SideBuy, bids[i]}, {wire.SideSell, asks[i]}} {
				seq++
				id := prefix + "-" + strconv.Itoa(seq)
				q.mu.Lock()
				q.sides[id] = s.side
				q.mu.Unlock()
				if err := send(conn, wire.Enter{Version: wire.Version, ClOrdID: id, Symbol: c.symbol, Side: s.side,
					Type: wire.TypeLimit, TIF: wire.TIFGoodTillCancel, Price: s.price, Quantity: c.qty}); err != nil {
					return err
				}
			}
		}
		if n%10 == 0 {
			q.mu.Lock()
			fmt.Fprintf(out, "obquote: mid %d, quoting %d-%d / %d-%d, inventory %+d, fills %d, rejects %d\n",
				mid, bids[len(bids)-1], bids[0], asks[0], asks[len(asks)-1], q.inventory, q.fills, q.rejects)
			q.mu.Unlock()
		}
		select {
		case <-tick.C:
		case <-stop:
			return nil
		case err := <-readErr:
			return fmt.Errorf("connection lost: %w", err)
		}
	}
}

func dial(c config) (net.Conn, error) {
	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		return nil, err
	}
	b, err := wire.EncodeLoginRequest(nil, wire.LoginRequest{Username: c.user, Password: c.password})
	if err == nil {
		err = wire.WritePacket(conn, wire.PacketLoginRequest, b)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	pkt, err := wire.ReadPacket(conn, make([]byte, wire.MaxPayload))
	if err != nil {
		conn.Close()
		return nil, err
	}
	if pkt.Type != wire.PacketLoginAccepted {
		conn.Close()
		return nil, fmt.Errorf("login as %q refused", c.user)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, nil
}

func send(conn net.Conn, m any) error {
	var b []byte
	var err error
	switch m := m.(type) {
	case wire.Enter:
		b, err = wire.EncodeEnter(nil, m)
	case wire.Cancel:
		b, err = wire.EncodeCancel(nil, m)
	}
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return wire.WritePacket(conn, wire.PacketUnsequenced, b)
}

// read tracks which quotes are live and what inventory the fills left.
func (q *quoter) read(conn net.Conn) error {
	buf := make([]byte, wire.MaxPayload)
	for {
		pkt, err := wire.ReadPacket(conn, buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if pkt.Type != wire.PacketSequencedData {
			continue
		}
		t, ok := wire.MsgTypeOf(pkt.Payload)
		if !ok {
			continue
		}
		q.mu.Lock()
		switch t {
		case wire.MsgExecuted:
			if m, err := wire.DecodeExecuted(pkt.Payload); err == nil {
				q.fills++
				if q.sides[m.ClOrdID] == wire.SideBuy {
					q.inventory += m.Quantity
				} else {
					q.inventory -= m.Quantity
				}
				if m.LeavesQty == 0 {
					delete(q.sides, m.ClOrdID)
				}
			}
		case wire.MsgCanceled:
			if m, err := wire.DecodeCanceled(pkt.Payload); err == nil {
				delete(q.sides, m.ClOrdID)
			}
		case wire.MsgRejected:
			if m, err := wire.DecodeRejected(pkt.Payload); err == nil {
				q.rejects++
				delete(q.sides, m.ClOrdID)
			}
		case wire.MsgCmdReject:
			// A cancel of a quote that filled first: the quote is already gone.
			if m, err := wire.DecodeCmdReject(pkt.Payload); err == nil {
				q.rejects++
				delete(q.sides, m.ClOrdID)
			}
		}
		q.mu.Unlock()
	}
}
