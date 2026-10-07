package benchgate

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/intrepidkarthi/orderbook/internal/tape"
)

// The cross-engine protocol (docs/CROSS-ENGINE.md). Another engine cannot read an
// obtape file or write OBDG records, so a tape goes to its adapter as plain lines and
// comes back as plain lines, and this side folds those into the core chain. One
// encoder for every engine means a digest that differs is the engine's, not an
// encoder's.

// ExportText renders a basic tape for an adapter:
//
//	N <count>
//	S <pos> <acct> <side> <price> <qty>    limit GTC submit, side 0 buy / 1 sell
//	C <pos> <acct> <target>                cancel the order made at position target
//
// Any command outside that subset is an error: an adapter is never asked to express
// what the basic tape promised it would not have to.
func ExportText(tp *Tape, w io.Writer) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "N %d\n", len(tp.Cmds))
	for _, c := range tp.Cmds {
		acct, err := strconv.Atoi(strings.TrimPrefix(c.User, "u"))
		if err != nil || !strings.HasPrefix(c.User, "u") {
			return fmt.Errorf("xeng: command %d: account %q is not u<n>", c.Pos, c.User)
		}
		switch {
		case c.Kind == tape.Submit && !c.MarketOrd && !c.PostOnly && c.TIF == 0 && c.STP == 0 && !c.Privileged && c.TradeGroup == 0:
			fmt.Fprintf(bw, "S %d %d %d %d %d\n", c.Pos, acct, b2i(c.Sell), c.Price, c.Qty)
		case c.Kind == tape.Cancel:
			fmt.Fprintf(bw, "C %d %d %d\n", c.Pos, acct, c.Target)
		default:
			return fmt.Errorf("xeng: command %d (%v) is outside the basic subset", c.Pos, c.Kind)
		}
	}
	return bw.Flush()
}

// XResult is what an adapter's output folds into.
type XResult struct {
	Core     [32]byte
	Resting  int
	Trades   uint64
	ReplayNS int64
}

// DigestText reads an adapter's output and folds it into the core chain:
//
//	c <pos> <refused>                               one per command, in tape order
//	X <price> <qty> <maker> <taker> <B|S>           that command's trades, engine order
//	L <pos> <side> <price> <qty> <filled>           the terminal book, in priority order
//	E <last trade price> <trade count>
//	T <replay ns>
//
// It is strict on purpose. A missing, repeated or out-of-order command, a trade naming
// a position that made no order, or a trade count that disagrees with the X lines is
// an error, not a digest that happens to differ.
func DigestText(tp *Tape, r io.Reader) (XResult, error) {
	made := make([]bool, len(tp.Cmds))
	for _, c := range tp.Cmds {
		made[c.Pos] = c.Kind == tape.Submit
	}
	order := func(v int64, what string, line int) (ref, error) {
		if v < 0 || v >= int64(len(made)) || !made[v] {
			return ref{}, fmt.Errorf("xeng: line %d: %s %d made no order", line, what, v)
		}
		return ref{pos: uint64(v)}, nil
	}
	dg := newDigester(tp.FileSHA256, false)
	var (
		cur      *outcome
		term     terminal
		next     int64
		res      XResult
		sawE     bool
		sawT     bool
		inTrades bool
	)
	flush := func() error {
		if cur == nil {
			return nil
		}
		err := dg.command(*cur)
		cur = nil
		return err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<16)
	for line := 1; sc.Scan(); line++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		want := map[string]int{"c": 3, "X": 6, "L": 6, "E": 3, "T": 2}[f[0]]
		if want == 0 || len(f) != want {
			return res, fmt.Errorf("xeng: line %d: %q is not a protocol line", line, sc.Text())
		}
		v := make([]int64, len(f)-1)
		for i, s := range f[1:] {
			if f[0] == "X" && i == 4 {
				continue
			}
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return res, fmt.Errorf("xeng: line %d: %v", line, err)
			}
			v[i] = n
		}
		if sawE && f[0] != "T" || sawT {
			return res, fmt.Errorf("xeng: line %d: %q after the terminal record", line, sc.Text())
		}
		switch f[0] {
		case "c":
			if term.book != nil {
				return res, fmt.Errorf("xeng: line %d: command after the terminal book", line)
			}
			if err := flush(); err != nil {
				return res, err
			}
			if v[0] != next {
				return res, fmt.Errorf("xeng: line %d: command %d where %d was due", line, v[0], next)
			}
			next++
			st := uint8(stNew)
			switch v[1] {
			case 0:
			case 1:
				st = stRejected
			default:
				return res, fmt.Errorf("xeng: line %d: refused must be 0 or 1", line)
			}
			cur = &outcome{pos: uint64(v[0]), status: st}
			inTrades = true
		case "X":
			if cur == nil || !inTrades {
				return res, fmt.Errorf("xeng: line %d: trade outside a command", line)
			}
			maker, err := order(v[2], "maker", line)
			if err != nil {
				return res, err
			}
			taker, err := order(v[3], "taker", line)
			if err != nil {
				return res, err
			}
			if f[5] != "B" && f[5] != "S" {
				return res, fmt.Errorf("xeng: line %d: aggressor %q", line, f[5])
			}
			cur.trades = append(cur.trades, dTrade{price: v[0], qty: v[1], maker: maker, taker: taker, aggressor: f[5][0]})
			res.Trades++
		case "L":
			if err := flush(); err != nil {
				return res, err
			}
			inTrades = false
			o, err := order(v[0], "resting order", line)
			if err != nil {
				return res, err
			}
			if v[1] != 0 && v[1] != 1 {
				return res, fmt.Errorf("xeng: line %d: side must be 0 or 1", line)
			}
			if term.book == nil {
				term.book = []restingOrder{}
			}
			term.book = append(term.book, restingOrder{ref: o, sell: v[1] == 1, price: v[2], qty: v[3], filled: v[4]})
		case "E":
			if err := flush(); err != nil {
				return res, err
			}
			inTrades = false
			if uint64(v[1]) != res.Trades || v[1] < 0 {
				return res, fmt.Errorf("xeng: line %d: %d trades claimed, %d printed", line, v[1], res.Trades)
			}
			term.lastTrade = v[0]
			sawE = true
		case "T":
			if !sawE {
				return res, fmt.Errorf("xeng: line %d: timing before the terminal record", line)
			}
			res.ReplayNS = v[0]
			sawT = true
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if next != int64(len(tp.Cmds)) {
		return res, fmt.Errorf("xeng: %d of %d commands reported", next, len(tp.Cmds))
	}
	if !sawE || !sawT {
		return res, fmt.Errorf("xeng: output ends before the terminal record and timing")
	}
	d := dg.finish(term)
	res.Core, res.Resting = d.Core, d.Resting
	return res, nil
}
