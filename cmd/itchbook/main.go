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
	replays := flag.Int("replays", 0, "decode the messages into memory, then build the books this many times and time only the builds (0: one pass, timed with reading)")
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
	if err := run(in, os.Stdout, os.Stderr, *symbol, *levels, *n, *replays); err != nil {
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

func run(in io.Reader, out, rate io.Writer, symbol string, levels int, max int64, replays int) error {
	cfg := itch.Config{}
	if symbol != "" {
		cfg.Symbols = []string{symbol}
	}
	if replays > 0 {
		return runReplays(in, out, rate, cfg, symbol, levels, max, replays)
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

	summarize(out, books, r.Offset(), symbol, levels)
	if s := &books.Stats; s.Messages > 0 {
		fmt.Fprintf(rate, "replayed %d messages in %v: %.0f ns/message\n",
			s.Messages, elapsed.Round(time.Millisecond), float64(elapsed.Nanoseconds())/float64(s.Messages))
	}
	return nil
}

// runReplays decodes up to max messages into memory, then builds every book from
// them replays times, each from empty, and times only the builds. Reading and
// decoding stay outside the clock, so two builds of this command can be compared on
// the book's own cost over real order flow (docs/BOOK-LADDER.md §6.9). The summary
// is the last build's; each build of the same messages produces the same one.
func runReplays(in io.Reader, out, rate io.Writer, cfg itch.Config, symbol string, levels int, max int64, replays int) error {
	r := itch.NewReader(in)
	var msgs []itch.Message
	var m itch.Message
	for max == 0 || int64(len(msgs)) < max {
		err := r.Next(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("after %d messages: %w", len(msgs), err)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return errors.New("no messages to replay")
	}
	var books *itch.Books
	perMsg := make([]float64, 0, replays)
	for i := 0; i < replays; i++ {
		books = itch.NewBooks(cfg)
		start := time.Now()
		for j := range msgs {
			books.Apply(&msgs[j])
		}
		ns := float64(time.Since(start).Nanoseconds()) / float64(len(msgs))
		perMsg = append(perMsg, ns)
		fmt.Fprintf(rate, "build %d: %.1f ns/message\n", i+1, ns)
	}
	summarize(out, books, r.Offset(), symbol, levels)
	sort.Float64s(perMsg)
	// One line a script can read: the median build over the replays.
	fmt.Fprintf(rate, "build_ns_per_message %.2f\n", perMsg[len(perMsg)/2])
	return nil
}

// summarize writes what the books hold: counts, anomalies and, for one stock, its
// top levels.
func summarize(out io.Writer, books *itch.Books, offset int64, symbol string, levels int) {
	s := &books.Stats
	fmt.Fprintf(out, "messages %d, %d bytes\n", s.Messages, offset)
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
