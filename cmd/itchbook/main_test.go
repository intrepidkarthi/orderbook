package main

import (
	"bytes"
	"os"
	"testing"
)

// TestSampleSummary pins itchbook's output for the committed fixture, whose book is
// worked out by hand in pkg/itch's TestSampleFixture.
func TestSampleSummary(t *testing.T) {
	f, err := os.Open("../../pkg/itch/testdata/sample.itch")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out, rate bytes.Buffer
	if err := run(f, &out, &rate, "AAPL", 5, 0); err != nil {
		t.Fatal(err)
	}
	want := `messages 17, 572 bytes
by type: A 8, C 1, D 1, E 2, F 1, S 1, U 1, X 2
not held 1, over-executed 1, book full 0, skipped 2
books 1
AAPL: 4 resting orders
  ask    150.0200       60  (1)
  ask    150.0100      200  (1)
  bid    150.0000       70  (1)
  bid    149.9950      250  (1)
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	if rate.Len() == 0 {
		t.Fatal("no rate line on the rate writer")
	}
}

func TestMaxStopsEarlyAndATruncatedFileFails(t *testing.T) {
	b, err := os.ReadFile("../../pkg/itch/testdata/sample.itch")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(bytes.NewReader(b), &out, &bytes.Buffer{}, "", 5, 3); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("messages 3,")) {
		t.Fatalf("-n 3 read %q", out.String())
	}
	if err := run(bytes.NewReader(b[:len(b)-4]), &bytes.Buffer{}, &bytes.Buffer{}, "", 5, 0); err == nil {
		t.Fatal("a truncated file replayed without an error")
	}
}
