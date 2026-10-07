// Command geseq replays a basic cross-engine tape through github.com/geseq/orderbook
// and prints what the engine did, in the line protocol internal/benchgate/xeng.go
// reads back.
//
// The engine names orders by uint64 id. The order made at tape position pos gets
// id pos+1, so id 0 (the value a released Order is zeroed to) is never used, and a
// cancel naming a position that made no order names an id the engine never saw.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	ob "github.com/geseq/orderbook"
	"github.com/geseq/udecimal"
)

type command struct {
	cancel bool
	sell   bool
	target int64 // cancels: the position whose order is cancelled
	qty    int64 // submits: original quantity, for the terminal book's filled column
	side   ob.SideType
	price  udecimal.Decimal
	dqty   udecimal.Decimal
}

type trade struct {
	price, qty   udecimal.Decimal
	maker, taker uint64
}

// recorder is the engine's NotificationHandler. Inside the timed loop it only
// sets a flag or appends; decimals are converted after timing.
type recorder struct {
	refused bool
	trades  []trade
}

// PutOrder sees every accept, reject and cancel. A cancel of an id that is not
// resting comes back as Rejected with ErrOrderNotExists.
func (r *recorder) PutOrder(_ ob.MsgType, s ob.OrderStatus, _ uint64, _ udecimal.Decimal, _ error) {
	if s == ob.Rejected {
		r.refused = true
	}
}

func (r *recorder) PutTrade(maker, taker uint64, _, _ ob.OrderStatus, qty, price udecimal.Decimal) {
	r.trades = append(r.trades, trade{price: price, qty: qty, maker: maker, taker: taker})
}

func main() {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail("read stdin: %v", err)
	}
	cmds, submits := parse(in)

	// A trade either fills its maker or ends its taker's command, so there are at
	// most submits+len(cmds) of them.
	rec := &recorder{trades: make([]trade, 0, submits+len(cmds))}
	refused := make([]bool, len(cmds))
	tradeEnd := make([]int, len(cmds))
	book := ob.NewOrderBook(rec)

	// Every AddOrder and CancelOrder must carry the previous token plus one.
	var tok uint64
	start := time.Now()
	for i := range cmds {
		c := &cmds[i]
		rec.refused = false
		tok++
		if c.cancel {
			book.CancelOrder(tok, uint64(c.target)+1)
		} else {
			book.AddOrder(tok, uint64(i)+1, ob.Limit, c.side, c.dqty, c.price, udecimal.Zero, ob.None)
		}
		refused[i] = rec.refused
		tradeEnd[i] = len(rec.trades)
	}
	replayNS := time.Since(start).Nanoseconds()

	w := bufio.NewWriterSize(os.Stdout, 1<<20)
	t := 0
	for i := range cmds {
		fmt.Fprintf(w, "c %d %d\n", i, b2i(refused[i]))
		for ; t < tradeEnd[i]; t++ {
			tr := rec.trades[t]
			taker := pos(tr.taker, len(cmds))
			aggr := 'B'
			if cmds[taker].sell {
				aggr = 'S'
			}
			fmt.Fprintf(w, "X %d %d %d %d %c\n",
				whole(tr.price), whole(tr.qty), pos(tr.maker, len(cmds)), taker, aggr)
		}
	}

	// The book is read from the engine: Snapshot gives bids best-first, then asks
	// best-first, FIFO within a level, each with its remaining quantity. The engine
	// does not keep an order's original quantity, so that comes from the tape.
	snap := book.Snapshot()
	for _, side := range [][]ob.RestingOrder{snap.Bids, snap.Asks} {
		for _, o := range side {
			p := pos(o.ID, len(cmds))
			s := 0
			if o.Side == ob.Sell {
				s = 1
			}
			fmt.Fprintf(w, "L %d %d %d %d %d\n", p, s, whole(o.Price), cmds[p].qty, cmds[p].qty-whole(o.Qty))
		}
	}

	var last int64
	if n := len(rec.trades); n > 0 {
		last = whole(rec.trades[n-1].price)
	}
	fmt.Fprintf(w, "E %d %d\n", last, len(rec.trades))
	fmt.Fprintf(w, "T %d\n", replayNS)
	if err := w.Flush(); err != nil {
		fail("write stdout: %v", err)
	}
}

// parse reads the whole tape. It returns the commands indexed by position and the
// number of submits.
func parse(in []byte) ([]command, int) {
	lines := bytes.Split(in, []byte("\n"))
	var cmds []command
	submits := 0
	seenN := false
	for ln, line := range lines {
		f := bytes.Fields(line)
		if len(f) == 0 {
			continue
		}
		v := make([]int64, len(f)-1)
		for i, s := range f[1:] {
			n, err := strconv.ParseInt(string(s), 10, 64)
			if err != nil {
				fail("line %d: %v", ln+1, err)
			}
			v[i] = n
		}
		switch {
		case string(f[0]) == "N" && len(v) == 1 && v[0] >= 0 && !seenN:
			cmds = make([]command, 0, v[0])
			seenN = true
		case string(f[0]) == "S" && len(v) == 5 && seenN:
			// S pos acct side price qty
			if v[0] != int64(len(cmds)) || v[2] < 0 || v[2] > 1 || v[3] <= 0 || v[4] <= 0 {
				fail("line %d: bad submit", ln+1)
			}
			c := command{
				sell:  v[2] == 1,
				qty:   v[4],
				side:  ob.Buy,
				price: udecimal.NewI(uint64(v[3]), 0),
				dqty:  udecimal.NewI(uint64(v[4]), 0),
			}
			if c.sell {
				c.side = ob.Sell
			}
			cmds = append(cmds, c)
			submits++
		case string(f[0]) == "C" && len(v) == 3 && seenN:
			// C pos acct target
			if v[0] != int64(len(cmds)) {
				fail("line %d: bad cancel", ln+1)
			}
			cmds = append(cmds, command{cancel: true, target: v[2]})
		default:
			fail("line %d: %q is not a tape line", ln+1, line)
		}
	}
	if !seenN || len(cmds) != cap(cmds) {
		fail("tape: header count does not match %d commands", len(cmds))
	}
	return cmds, submits
}

// pos maps an engine order id back to the tape position that made it.
func pos(id uint64, n int) int {
	if id == 0 || id > uint64(n) {
		fail("engine reported unknown order id %d", id)
	}
	return int(id - 1)
}

// whole converts an engine decimal to the integer it must hold. Prices and
// quantities go in as integers, so a fraction coming out is an engine error.
func whole(d udecimal.Decimal) int64 {
	n := d.Int()
	if !d.Equal(udecimal.NewI(n, 0)) {
		fail("engine reported non-integer value %s", d)
	}
	return int64(n)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "geseq: "+format+"\n", a...)
	os.Exit(1)
}
