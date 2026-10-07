package itch

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/intrepidkarthi/orderbook/pkg/orderbook"
	"github.com/intrepidkarthi/orderbook/pkg/types"
)

func stock(s string) (b [8]byte) {
	copy(b[:], s+strings.Repeat(" ", 8-len(s)))
	return
}

func encode(t testing.TB, msgs ...Message) []byte {
	t.Helper()
	var b []byte
	for i := range msgs {
		var err error
		if b, err = Append(b, &msgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestEveryDecodedTypeRoundTrips(t *testing.T) {
	msgs := []Message{
		{Type: 'A', Locate: 7, Tracking: 2, Timestamp: 34_200_000_000_123, Ref: 1, Side: 'B', Shares: 100, Stock: stock("AAPL"), Price: 1_502_500},
		{Type: 'F', Locate: 7, Timestamp: 1<<48 - 1, Ref: 2, Side: 'S', Shares: 200, Stock: stock("AAPL"), Price: 1_503_000, MPID: [4]byte{'G', 'S', 'C', 'O'}},
		{Type: 'E', Locate: 7, Ref: 1, Shares: 40, Match: 9_000_001},
		{Type: 'C', Locate: 7, Ref: 2, Shares: 50, Match: 9_000_002, Printable: 'Y', Price: 1_502_900},
		{Type: 'X', Locate: 7, Ref: 2, Shares: 25},
		{Type: 'D', Locate: 7, Ref: 1},
		{Type: 'U', Locate: 7, Ref: 2, NewRef: 3, Shares: 300, Price: 1_503_100},
	}
	r := NewReader(bytes.NewReader(encode(t, msgs...)))
	for i, want := range msgs {
		var got Message
		if err := r.Next(&got); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("message %d:\n got %+v\nwant %+v", i, got, want)
		}
	}
	var m Message
	if err := r.Next(&m); err != io.EOF {
		t.Fatalf("after the last message: %v, want io.EOF", err)
	}
}

// TestKnownLayoutBytes pins one Add Order against bytes written out from the ITCH
// 5.0 specification's field table, so Append and Decode cannot agree on a wrong
// layout between themselves.
func TestKnownLayoutBytes(t *testing.T) {
	raw := []byte{
		0x00, 0x24, // length 36
		'A',
		0x00, 0x07, // locate
		0x00, 0x02, // tracking
		0x00, 0x07, 0xF6, 0x7A, 0x97, 0xCA, // timestamp 34,200,000,458 ns: 09:30:00.000000458
		0, 0, 0, 0, 0, 0, 0, 0x2A, // ref 42
		'S',
		0x00, 0x00, 0x01, 0x2C, // shares 300
		'M', 'S', 'F', 'T', ' ', ' ', ' ', ' ',
		0x00, 0x2F, 0x82, 0x20, // price 3,113,504 = $311.3504
	}
	var m Message
	if err := NewReader(bytes.NewReader(raw)).Next(&m); err != nil {
		t.Fatal(err)
	}
	want := Message{Type: 'A', Locate: 7, Tracking: 2, Timestamp: 34_200_000_458, Ref: 42, Side: 'S', Shares: 300, Stock: stock("MSFT"), Price: 3_113_504}
	if m != want {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}
	if again := encode(t, want); !bytes.Equal(again, raw) {
		t.Fatalf("Append wrote % x\n         want % x", again, raw)
	}
}

func TestOtherTypesKeepTheirHeader(t *testing.T) {
	raw := []byte{0x00, 0x0C, 'S', 0x00, 0x00, 0x00, 0x00, 0, 0, 0, 0, 0, 5, 'O'} // system event
	var m Message
	if err := NewReader(bytes.NewReader(raw)).Next(&m); err != nil {
		t.Fatal(err)
	}
	if m.Type != 'S' || m.Timestamp != 5 || Decoded('S') {
		t.Fatalf("got %+v", m)
	}
}

func TestMalformedInputIsAnError(t *testing.T) {
	good := encode(t, Message{Type: 'D', Locate: 1, Ref: 9})
	cases := map[string][]byte{
		"a known type at the wrong length": append([]byte{0x00, 0x14}, append(good[2:], 0)...),
		"a frame shorter than the header":  {0x00, 0x03, 'D', 0, 1},
		"input ends inside a frame":        good[:len(good)-3],
		"input ends inside a length":       append(append([]byte{}, good...), 0x00),
		"an add with side Z": func() []byte {
			b := encode(t, Message{Type: 'A', Ref: 1, Side: 'B', Shares: 1, Price: 1})
			b[2+19] = 'Z'
			return b
		}(),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewReader(bytes.NewReader(b))
			var m Message
			var err error
			for err == nil {
				err = r.Next(&m)
			}
			if err == io.EOF {
				t.Fatal("read to a clean end")
			}
		})
	}
	var m Message
	err := NewReader(bytes.NewReader(good[:len(good)-1])).Next(&m)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncation: %v does not wrap ErrTruncated", err)
	}
}

// --- the book against a model ---------------------------------------------------

type modelOrder struct {
	locate    uint16
	sell      bool
	price     int64
	remaining int64
	seq       int
}

type model struct {
	orders                map[uint64]*modelOrder
	seq                   int
	notHeld, overExecuted uint64
}

func (md *model) apply(m *Message) {
	switch m.Type {
	case 'A', 'F':
		md.seq++
		md.orders[m.Ref] = &modelOrder{m.Locate, m.Side == 'S', int64(m.Price), int64(m.Shares), md.seq}
	case 'E', 'C', 'X':
		o := md.orders[m.Ref]
		if o == nil || o.locate != m.Locate {
			md.notHeld++
			return
		}
		if n := int64(m.Shares); n >= o.remaining {
			if n > o.remaining {
				md.overExecuted++
			}
			delete(md.orders, m.Ref)
		} else {
			o.remaining -= n
		}
	case 'D':
		if o := md.orders[m.Ref]; o == nil || o.locate != m.Locate {
			md.notHeld++
		} else {
			delete(md.orders, m.Ref)
		}
	case 'U':
		o := md.orders[m.Ref]
		if o == nil || o.locate != m.Locate {
			md.notHeld++
			return
		}
		delete(md.orders, m.Ref)
		md.seq++
		md.orders[m.NewRef] = &modelOrder{m.Locate, o.sell, int64(m.Price), int64(m.Shares), md.seq}
	}
}

// book is the model's view of one stock: bids best-first, then asks best-first,
// oldest first in a level, as (ref, remaining).
func (md *model) book(locate uint16) [][3]int64 {
	type row struct {
		ref uint64
		o   *modelOrder
	}
	var rows []row
	for ref, o := range md.orders {
		if o.locate == locate {
			rows = append(rows, row{ref, o})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].o, rows[j].o
		if a.sell != b.sell {
			return !a.sell
		}
		if a.price != b.price {
			return (a.price > b.price) != a.sell
		}
		return a.seq < b.seq
	})
	out := make([][3]int64, len(rows))
	for i, r := range rows {
		out[i] = [3]int64{int64(r.ref), r.o.price, r.o.remaining}
	}
	return out
}

// stream generates n messages over three stocks: adds, partial and full
// executions, partial cancels, deletes, replaces, references never added and
// over-executions. Bids sit below 1000 and asks at or above, as on a real book.
func stream(seed int64, n int) []Message {
	rng := rand.New(rand.NewSource(seed))
	syms := []string{"AAPL", "MSFT", "SPY"}
	var live []uint64
	locOf := map[uint64]uint16{}
	next := uint64(1)
	var out []Message
	pick := func() (uint64, uint16) {
		if len(live) == 0 || rng.Intn(25) == 0 {
			return 1_000_000 + uint64(rng.Intn(1000)), uint16(1 + rng.Intn(3)) // never added
		}
		i := rng.Intn(len(live))
		return live[i], locOf[live[i]]
	}
	for len(out) < n {
		var m Message
		switch x := rng.Intn(100); {
		case x < 50 || len(live) == 0:
			loc := uint16(1 + rng.Intn(3))
			side := byte('B')
			price := uint32(990 - rng.Intn(10))
			if rng.Intn(2) == 1 {
				side, price = 'S', uint32(1000+rng.Intn(10))
			}
			m = Message{Type: "AF"[rng.Intn(2)], Locate: loc, Ref: next, Side: side, Shares: uint32(1 + rng.Intn(500)), Stock: stock(syms[loc-1]), Price: price}
			live = append(live, next)
			locOf[next] = loc
			next++
		case x < 65:
			ref, loc := pick()
			m = Message{Type: "EC"[rng.Intn(2)], Locate: loc, Ref: ref, Shares: uint32(1 + rng.Intn(600)), Match: uint64(len(out)), Printable: 'Y', Price: 995}
		case x < 78:
			ref, loc := pick()
			m = Message{Type: 'X', Locate: loc, Ref: ref, Shares: uint32(1 + rng.Intn(300))}
		case x < 89:
			ref, loc := pick()
			m = Message{Type: 'D', Locate: loc, Ref: ref}
		default:
			ref, loc := pick()
			price := uint32(990 - rng.Intn(10))
			if o, ok := locOf[ref]; ok && o == loc && rng.Intn(2) == 1 {
				price = uint32(1000 + rng.Intn(10))
			}
			m = Message{Type: 'U', Locate: loc, Ref: ref, NewRef: next, Shares: uint32(1 + rng.Intn(500)), Price: price}
			live = append(live, next)
			locOf[next] = loc
			next++
		}
		out = append(out, m)
	}
	return out
}

func bookRows(t *testing.T, b *Books, sym string) [][3]int64 {
	t.Helper()
	book := b.Book(sym)
	if book == nil {
		return [][3]int64{}
	}
	out := [][3]int64{}
	for _, o := range book.Orders() {
		out = append(out, [3]int64{o.ID, o.Price, o.RemainingQty})
	}
	return out
}

func TestBooksAgreeWithAModel(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		msgs := stream(seed, 20_000)
		// Replace targets were drawn among live refs whatever their side; the model
		// keeps a replaced order's side, and so must the books, so side mismatches
		// in the generated price band are harmless for the comparison.
		b := NewBooks(Config{})
		md := &model{orders: map[uint64]*modelOrder{}}
		r := NewReader(bytes.NewReader(encode(t, msgs...)))
		for i := range msgs {
			var m Message
			if err := r.Next(&m); err != nil {
				t.Fatal(err)
			}
			b.Apply(&m)
			md.apply(&m)
			if i%997 == 0 || i == len(msgs)-1 {
				for loc, sym := range []string{"AAPL", "MSFT", "SPY"} {
					got, want := bookRows(t, b, sym), md.book(uint16(loc+1))
					if len(got) != len(want) {
						t.Fatalf("seed %d, message %d, %s: %d resting, model %d", seed, i, sym, len(got), len(want))
					}
					for j := range want {
						if got[j] != want[j] {
							t.Fatalf("seed %d, message %d, %s, row %d: book %v, model %v", seed, i, sym, j, got[j], want[j])
						}
					}
					if book := b.Book(sym); book != nil {
						checkLevelTotals(t, book.Orders(), book.GetBidLevels(1<<20), book.GetAskLevels(1<<20))
					}
				}
			}
		}
		if b.Stats.NotHeld != md.notHeld || b.Stats.OverExecuted != md.overExecuted {
			t.Fatalf("seed %d: not held %d / over-executed %d, model %d / %d",
				seed, b.Stats.NotHeld, b.Stats.OverExecuted, md.notHeld, md.overExecuted)
		}
		if md.notHeld == 0 || md.overExecuted == 0 {
			t.Fatalf("seed %d: the stream reached no anomaly (%d / %d); it tests less than it claims", seed, md.notHeld, md.overExecuted)
		}
	}
}

// checkLevelTotals requires every level's aggregate to equal the sum of its orders'
// remaining shares: an execution that reduced an order and not its level would
// otherwise pass the order-by-order comparison.
func checkLevelTotals(t *testing.T, orders []*types.Order, bids, asks []*orderbook.PriceLevel) {
	t.Helper()
	type key struct {
		side  types.Side
		price int64
	}
	sum := map[key]int64{}
	for _, o := range orders {
		sum[key{o.Side, o.Price}] += o.RemainingQty
	}
	for side, ls := range map[types.Side][]*orderbook.PriceLevel{types.SideBuy: bids, types.SideSell: asks} {
		for _, l := range ls {
			if want := sum[key{side, l.Price}]; l.TotalQty != want {
				t.Fatalf("%v level %d: total %d, orders sum to %d", side, l.Price, l.TotalQty, want)
			}
		}
	}
}

func TestSelectedSymbolsOnly(t *testing.T) {
	msgs := stream(4, 2000)
	b := NewBooks(Config{Symbols: []string{"MSFT"}})
	for i := range msgs {
		b.Apply(&msgs[i])
	}
	if got := b.Symbols(); len(got) != 1 || got[0] != "MSFT" {
		t.Fatalf("books built for %v", got)
	}
	if b.Stats.Skipped == 0 {
		t.Fatal("nothing counted as skipped")
	}
}

func BenchmarkApply(b *testing.B) {
	msgs := stream(5, 1_000_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; {
		books := NewBooks(Config{})
		for j := range msgs {
			books.Apply(&msgs[j])
			if i++; i == b.N {
				break
			}
		}
	}
}

// TestSteadyStateDoesNotAllocate: once a book holds its working set, an add and a
// delete per step reuse the removed order instead of allocating one.
func TestSteadyStateDoesNotAllocate(t *testing.T) {
	b := NewBooks(Config{})
	add := func(ref uint64) {
		m := Message{Type: 'A', Locate: 1, Ref: ref, Side: "BS"[ref%2], Shares: 100, Stock: stock("AAPL"), Price: 990 + uint32(ref%2)*20 - uint32(ref%7)}
		b.Apply(&m)
	}
	const window = 1000
	for ref := uint64(1); ref <= 2*window; ref++ {
		add(ref)
		if ref > window {
			b.Apply(&Message{Type: 'D', Locate: 1, Ref: ref - window})
		}
	}
	ref := uint64(2*window + 1)
	allocs := testing.AllocsPerRun(5000, func() {
		add(ref)
		b.Apply(&Message{Type: 'D', Locate: 1, Ref: ref - window})
		ref++
	})
	if allocs > 0.01 {
		t.Fatalf("%.3f allocations per add and delete at steady state", allocs)
	}
}
