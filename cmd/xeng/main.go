// Command xeng runs the cross-engine comparison's side of the protocol
// (docs/CROSS-ENGINE.md).
//
//	xeng export <tape.obt>                       the tape as adapter input, on stdout
//	xeng run                                     this engine as an adapter: stdin to stdout
//	xeng table <tape.obt> <digest> name=out...   fold each output, check it, tabulate
//
// "run" drives the engine through its public API the way an adapter for any other
// engine drives that one, so all of them pay the same protocol.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/benchgate"
	"github.com/intrepidkarthi/orderbook/pkg/matching"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "export":
		if len(os.Args) != 3 {
			usage()
		}
		var tp *benchgate.Tape
		if tp, err = readTape(os.Args[2]); err == nil {
			err = benchgate.ExportText(tp, os.Stdout)
		}
	case "run":
		err = run(os.Stdin, os.Stdout)
	case "table":
		if len(os.Args) < 5 {
			usage()
		}
		err = table(os.Args[2], os.Args[3], os.Args[4:], os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "xeng:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: xeng export <tape.obt> | xeng run | xeng table <tape.obt> <digest> name=out...")
	os.Exit(2)
}

func readTape(path string) (*benchgate.Tape, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return benchgate.Parse(b)
}

type cmd struct {
	cancel     bool
	pos, acct  int64
	sell       bool
	price, qty int64
	target     int64
	user       string
}

type trade struct {
	price, qty, maker, taker int64
	sell                     bool
}

// run is this engine's adapter. Everything is parsed and allocated before the clock
// starts; inside the timed loop it only calls the engine and records what came back.
func run(in io.Reader, out io.Writer) error {
	cmds, err := parse(in)
	if err != nil {
		return err
	}
	cfg := matching.DefaultConfig("XENG")
	cfg.MaxOrders = 1_000_000
	e := matching.NewEngine(cfg)

	idOf := make([]int64, len(cmds))          // position -> engine id, 0 if none
	posOf := make(map[int64]int64, len(cmds)) // engine id -> position
	orders := make([]*types.Order, len(cmds))
	for i, c := range cmds {
		if c.cancel {
			continue
		}
		side := types.SideBuy
		if c.sell {
			side = types.SideSell
		}
		o, err := types.NewOrder(c.user, "XENG", side, types.OrderTypeLimit, c.price, c.qty, types.TIFGoodTillCancel)
		if err != nil {
			return fmt.Errorf("command %d: %v", c.pos, err)
		}
		orders[i] = o
	}
	refused := make([]bool, len(cmds))
	ends := make([]int, len(cmds)) // trades[ends[i-1]:ends[i]] belong to command i
	trades := make([]trade, 0, len(cmds))
	buf := make([]types.Trade, 0, 64)

	start := time.Now()
	for i, c := range cmds {
		if c.cancel {
			_, err := e.Cancel(idOf[c.target], c.user)
			refused[i] = err != nil
		} else {
			o := orders[i]
			var st types.OrderStatus
			buf, st, err = e.Match(o, buf[:0])
			idOf[i] = o.ID
			posOf[o.ID] = c.pos
			refused[i] = err != nil || st == types.OrderStatusRejected
			for _, t := range buf {
				trades = append(trades, trade{price: t.Price, qty: t.Quantity,
					maker: posOf[t.MakerOrderID], taker: posOf[t.TakerOrderID], sell: t.TakerSide == types.SideSell})
			}
		}
		ends[i] = len(trades)
	}
	elapsed := time.Since(start)

	w := bufio.NewWriterSize(out, 1<<20)
	from := 0
	for i, c := range cmds {
		fmt.Fprintf(w, "c %d %d\n", c.pos, b2i(refused[i]))
		for _, t := range trades[from:ends[i]] {
			agg := "B"
			if t.sell {
				agg = "S"
			}
			fmt.Fprintf(w, "X %d %d %d %d %s\n", t.price, t.qty, t.maker, t.taker, agg)
		}
		from = ends[i]
	}
	for _, o := range e.Book().Orders() {
		fmt.Fprintf(w, "L %d %d %d %d %d\n", posOf[o.ID], b2i(o.Side == types.SideSell), o.Price, o.Quantity, o.FilledQty)
	}
	fmt.Fprintf(w, "E %d %d\nT %d\n", e.LastTradePrice(), len(trades), elapsed.Nanoseconds())
	return w.Flush()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parse(in io.Reader) ([]cmd, error) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<16), 1<<16)
	if !sc.Scan() {
		return nil, fmt.Errorf("empty input")
	}
	var n int
	if _, err := fmt.Sscanf(sc.Text(), "N %d", &n); err != nil {
		return nil, fmt.Errorf("first line %q: %v", sc.Text(), err)
	}
	cmds := make([]cmd, 0, n)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		v := make([]int64, len(f))
		for i := 1; i < len(f); i++ {
			x, err := strconv.ParseInt(f[i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("line %q: %v", sc.Text(), err)
			}
			v[i] = x
		}
		switch {
		case len(f) == 6 && f[0] == "S":
			cmds = append(cmds, cmd{pos: v[1], acct: v[2], sell: v[3] == 1, price: v[4], qty: v[5]})
		case len(f) == 4 && f[0] == "C":
			cmds = append(cmds, cmd{cancel: true, pos: v[1], acct: v[2], target: v[3]})
		default:
			return nil, fmt.Errorf("line %q is not S or C", sc.Text())
		}
		c := &cmds[len(cmds)-1]
		c.user = "u" + strconv.FormatInt(c.acct, 10)
		if c.pos != int64(len(cmds)-1) || (c.cancel && (c.target < 0 || c.target >= c.pos)) {
			return nil, fmt.Errorf("line %q: position out of order", sc.Text())
		}
	}
	if len(cmds) != n {
		return nil, fmt.Errorf("%d commands, header says %d", len(cmds), n)
	}
	return cmds, sc.Err()
}

// table folds every output, checks each against the committed core digest, and
// prints a Markdown table. An engine is timed only if every one of its outputs
// agreed: the time of a run that did different work is not comparable.
func table(tapePath, digestPath string, outs []string, w io.Writer) error {
	tp, err := readTape(tapePath)
	if err != nil {
		return err
	}
	db, err := os.ReadFile(digestPath)
	if err != nil {
		return err
	}
	want, err := benchgate.ParseDigestFile(db)
	if err != nil {
		return err
	}
	type row struct {
		name   string
		ms     []float64
		agrees bool
		note   string
	}
	var rows []*row
	byName := map[string]*row{}
	for _, arg := range outs {
		name, path, ok := strings.Cut(arg, "=")
		if !ok {
			return fmt.Errorf("%q is not name=output", arg)
		}
		r := byName[name]
		if r == nil {
			r = &row{name: name, agrees: true}
			byName[name] = r
			rows = append(rows, r)
		}
		f, err := os.Open(path)
		if err != nil {
			r.agrees, r.note = false, err.Error()
			continue
		}
		res, err := benchgate.DigestText(tp, f)
		f.Close()
		switch {
		case err != nil:
			r.agrees, r.note = false, err.Error()
		case res.Core != want.Core:
			r.agrees = false
			if r.note == "" {
				r.note = fmt.Sprintf("core %x…, %d trades, %d resting", res.Core[:6], res.Trades, res.Resting)
			}
		default:
			r.ms = append(r.ms, float64(res.ReplayNS)/1e6)
		}
	}
	fmt.Fprintf(w, "| engine | core digest | runs | median replay | fastest | slowest |\n|---|---|---|---|---|---|\n")
	for _, r := range rows {
		if !r.agrees {
			fmt.Fprintf(w, "| %s | **differs** (%s) | | not timed | | |\n", r.name, r.note)
			continue
		}
		sort.Float64s(r.ms)
		fmt.Fprintf(w, "| %s | agrees | %d | %.2f ms | %.2f ms | %.2f ms |\n",
			r.name, len(r.ms), median(r.ms), r.ms[0], r.ms[len(r.ms)-1])
	}
	fmt.Fprintf(w, "\n%d commands of `%s`, core `%x`.\n", len(tp.Cmds), tp.Provenance, want.Core)
	return nil
}

func median(s []float64) float64 {
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
