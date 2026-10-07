package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLadder(t *testing.T) {
	bids, asks := ladder(1000, 5, 2, 3)
	if want := []int64{995, 993, 991}; !equal(bids, want) {
		t.Fatalf("bids %v, want %v", bids, want)
	}
	if want := []int64{1005, 1007, 1009}; !equal(asks, want) {
		t.Fatalf("asks %v, want %v", asks, want)
	}
	if b, a := ladder(1000, 0, 1, 1); b[0] >= a[0] {
		t.Fatalf("a zero half-spread crossed: bid %d ask %d", b[0], a[0])
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func metric(t *testing.T, url, name string) float64 {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[0] == name {
			v, _ := strconv.ParseFloat(f[1], 64)
			return v
		}
	}
	return -1
}

// TestQuotesRestOnARealGateway runs obgw, quotes into it for a few refreshes, and
// reads the venue's own count of resting orders: exactly one ladder, so every
// refresh cancelled the one before it.
func TestQuotesRestOnARealGateway(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs obgw")
	}
	dir := t.TempDir()
	gw := filepath.Join(dir, "obgw")
	if out, err := exec.Command("go", "build", "-o", gw, "../obgw").CombinedOutput(); err != nil {
		t.Fatalf("build obgw: %v\n%s", err, out)
	}
	addr, admin := freeAddr(t), freeAddr(t)
	cmd := exec.Command(gw, "-addr", addr, "-admin", admin, "-accounts", "mm:mm", "-symbol", "BTC-USD")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	metrics := "http://" + admin + "/metrics"
	deadline := time.Now().Add(10 * time.Second)
	for metric(t, metrics, "orderbook_resting_orders") < 0 {
		if time.Now().After(deadline) {
			t.Fatal("obgw did not come up")
		}
		time.Sleep(50 * time.Millisecond)
	}

	c := config{addr: addr, user: "mm", password: "mm", symbol: "BTC-USD", mid: 100000, half: 5, step: 2,
		levels: 4, qty: 10, walk: 3, skew: 0.1, refresh: 150 * time.Millisecond, duration: time.Second, seed: 1}
	var log strings.Builder
	if err := run(c, &log); err != nil {
		t.Fatal(err)
	}
	// The last refresh's cancels and enters have been sent; give the venue a moment
	// to apply them.
	want := float64(2 * c.levels)
	var got float64
	for i := 0; i < 50; i++ {
		if got = metric(t, metrics, "orderbook_resting_orders"); got == want {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("%v orders resting after the quoter stopped, want one ladder of %v\n%s", got, want, log.String())
	}
	if !strings.Contains(log.String(), "obquote: mid ") {
		t.Fatalf("no status line:\n%s", log.String())
	}
}
