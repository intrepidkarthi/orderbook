package itch

import (
	"bytes"
	"os"
	"testing"
)

const sampleFile = "testdata/sample.itch"

// sample is the committed fixture's content, written out so its final book can be
// worked out by hand: see TestSampleFixture.
func sample(t testing.TB) []byte {
	aapl, msft := stock("AAPL"), stock("MSFT")
	ts := uint64(34_200_000_000_000) // 09:30
	b := []byte{0x00, 0x0C, 'S', 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 'Q'} // system event: start of market hours
	b = append(b, encode(t,
		Message{Type: 'A', Locate: 1, Timestamp: ts + 1, Ref: 1, Side: 'B', Shares: 100, Stock: aapl, Price: 1_500_000},
		Message{Type: 'A', Locate: 1, Timestamp: ts + 2, Ref: 2, Side: 'B', Shares: 200, Stock: aapl, Price: 1_499_900},
		Message{Type: 'A', Locate: 1, Timestamp: ts + 3, Ref: 3, Side: 'B', Shares: 50, Stock: aapl, Price: 1_500_000},
		Message{Type: 'A', Locate: 1, Timestamp: ts + 4, Ref: 4, Side: 'S', Shares: 300, Stock: aapl, Price: 1_500_100},
		Message{Type: 'A', Locate: 1, Timestamp: ts + 5, Ref: 5, Side: 'S', Shares: 100, Stock: aapl, Price: 1_500_200},
		Message{Type: 'F', Locate: 1, Timestamp: ts + 6, Ref: 6, Side: 'S', Shares: 80, Stock: aapl, Price: 1_500_100, MPID: [4]byte{'N', 'S', 'D', 'Q'}},
		Message{Type: 'A', Locate: 2, Timestamp: ts + 7, Ref: 7, Side: 'B', Shares: 10, Stock: msft, Price: 3_000_000},
		Message{Type: 'A', Locate: 2, Timestamp: ts + 8, Ref: 8, Side: 'S', Shares: 20, Stock: msft, Price: 3_005_000},
		Message{Type: 'E', Locate: 1, Timestamp: ts + 9, Ref: 4, Shares: 100, Match: 1},
		Message{Type: 'C', Locate: 1, Timestamp: ts + 10, Ref: 1, Shares: 100, Match: 2, Printable: 'Y', Price: 1_500_000},
		Message{Type: 'X', Locate: 1, Timestamp: ts + 11, Ref: 5, Shares: 40},
		Message{Type: 'U', Locate: 1, Timestamp: ts + 12, Ref: 2, NewRef: 9, Shares: 250, Price: 1_499_950},
		Message{Type: 'D', Locate: 1, Timestamp: ts + 13, Ref: 6},
		Message{Type: 'E', Locate: 1, Timestamp: ts + 14, Ref: 999, Shares: 5, Match: 3},
		Message{Type: 'X', Locate: 1, Timestamp: ts + 15, Ref: 3, Shares: 60},
		Message{Type: 'A', Locate: 1, Timestamp: ts + 16, Ref: 10, Side: 'B', Shares: 70, Stock: aapl, Price: 1_500_000},
	)...)
	return b
}

// TestWriteSampleFixture writes the fixture when asked to.
func TestWriteSampleFixture(t *testing.T) {
	if os.Getenv("ITCH_WRITE_FIXTURE") != "1" {
		t.Skip("set ITCH_WRITE_FIXTURE=1 to write " + sampleFile)
	}
	if err := os.WriteFile(sampleFile, sample(t), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSampleFixture replays the committed file. By hand: ref 4 is executed down to
// 200; ref 1 fully executes; ref 5 is cancelled down to 60; ref 2 becomes ref 9 at
// 149.9950 for 250; ref 6 is deleted; ref 999 was never added; ref 3 is cancelled for
// more than it holds and goes; ref 10 joins 150.0000, now empty of ref 1 and 3.
func TestSampleFixture(t *testing.T) {
	b, err := os.ReadFile(sampleFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, sample(t)) {
		t.Fatalf("%s differs from what sample() writes; regenerate it with ITCH_WRITE_FIXTURE=1", sampleFile)
	}
	books := NewBooks(Config{})
	r := NewReader(bytes.NewReader(b))
	var m Message
	for r.Next(&m) == nil {
		books.Apply(&m)
	}
	want := map[string][][3]int64{
		"AAPL": {{10, 1_500_000, 70}, {9, 1_499_950, 250}, {4, 1_500_100, 200}, {5, 1_500_200, 60}},
		"MSFT": {{7, 3_000_000, 10}, {8, 3_005_000, 20}},
	}
	for sym, rows := range want {
		got := bookRows(t, books, sym)
		if len(got) != len(rows) {
			t.Fatalf("%s: %v, want %v", sym, got, rows)
		}
		for i := range rows {
			if got[i] != rows[i] {
				t.Fatalf("%s row %d: %v, want %v", sym, i, got[i], rows[i])
			}
		}
	}
	s := books.Stats
	if s.Messages != 17 || s.ByType['S'] != 1 || s.NotHeld != 1 || s.OverExecuted != 1 {
		t.Fatalf("stats %d messages, %d S, not held %d, over-executed %d", s.Messages, s.ByType['S'], s.NotHeld, s.OverExecuted)
	}
}
