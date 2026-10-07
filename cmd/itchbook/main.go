// Command itchbook rebuilds order books from a NASDAQ TotalView-ITCH 5.0 file and
// prints what it found (docs/ITCH.md).
//
//	itchbook [-symbol AAPL] [-levels 5] [-n MAX] FILE|-
//
// A FILE ending in .gz is decompressed. The summary goes to stdout and the replay
// rate to stderr, so the summary of a given input is always the same bytes.
package main

import (
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/itch"
	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func main() {
	symbol := flag.String("symbol", "", "build and print only this stock's book")
	levels := flag.Int("levels", 5, "price levels to print on each side")
	n := flag.Int64("n", 0, "stop after this many messages (0: read to the end)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: itchbook [-symbol SYM] [-levels N] [-n MAX] FILE|-")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	in, err := open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "itchbook:", err)
		os.Exit(1)
	}
	if err := run(in, os.Stdout, os.Stderr, *symbol, *levels, *n); err != nil {
		fmt.Fprintln(os.Stderr, "itchbook:", err)
		os.Exit(1)
	}
}

func open(path string) (io.Reader, error) {
	if path == "-" {
		return os.Stdin, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		return gzip.NewReader(f)
	}
	return f, nil
}

func run(in io.Reader, out, rate io.Writer, symbol string, levels int, max int64) error {
	cfg := itch.Config{}
	if symbol != "" {
		cfg.Symbols = []string{symbol}
	}
	books := itch.NewBooks(cfg)
	r := itch.NewReader(in)
	var m itch.Message
	start := time.Now()
	for max == 0 || int64(books.Stats.Messages) < max {
		err := r.Next(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("after %d messages: %w", books.Stats.Messages, err)
		}
		books.Apply(&m)
	}
	elapsed := time.Since(start)

	s := &books.Stats
	fmt.Fprintf(out, "messages %d, %d bytes\n", s.Messages, r.Offset())
	var types []string
	for t, c := range s.ByType {
		if c > 0 {
			types = append(types, fmt.Sprintf("%c %d", t, c))
		}
	}
	fmt.Fprintf(out, "by type: %s\n", strings.Join(types, ", "))
	fmt.Fprintf(out, "not held %d, over-executed %d, book full %d, skipped %d\n",
		s.NotHeld, s.OverExecuted, s.BookFull, s.Skipped)
	syms := books.Symbols()
	sort.Strings(syms)
	fmt.Fprintf(out, "books %d\n", len(syms))
	if symbol != "" {
		printBook(out, symbol, books.Book(symbol), levels)
	}
	if s.Messages > 0 {
		fmt.Fprintf(rate, "replayed %d messages in %v: %.0f ns/message\n",
			s.Messages, elapsed.Round(time.Millisecond), float64(elapsed.Nanoseconds())/float64(s.Messages))
	}
	return nil
}

func printBook(w io.Writer, symbol string, b *orderbook.OrderBook, levels int) {
	if b == nil {
		fmt.Fprintf(w, "%s: no orders seen\n", symbol)
		return
	}
	fmt.Fprintf(w, "%s: %d resting orders\n", symbol, b.Count())
	asks := b.GetAskLevels(levels)
	for i := len(asks) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "  ask %s %8d  (%d)\n", dollars(asks[i].Price), asks[i].TotalQty, len(b.GetOrdersAtPrice(types.SideSell, asks[i].Price)))
	}
	for _, l := range b.GetBidLevels(levels) {
		fmt.Fprintf(w, "  bid %s %8d  (%d)\n", dollars(l.Price), l.TotalQty, len(b.GetOrdersAtPrice(types.SideBuy, l.Price)))
	}
}

// dollars renders an ITCH Price(4): an integer with four implied decimals.
func dollars(p int64) string { return fmt.Sprintf("%6d.%04d", p/10000, p%10000) }
