// Command obgw is a reference order-entry gateway: a real TCP server speaking the
// binary protocol in internal/wire, in front of one matching engine.
//
// It exists to demonstrate the seam, not to be deployed. What it shows is that
// the library's pieces — the Runner's fire-and-forget enqueue, the gateway's
// admission control, the event stream, and per-account outbound streams that
// survive a disconnect — compose into a working venue edge. What it deliberately
// omits is everything a real deployment must decide for itself: TLS, credential
// storage, multi-symbol routing, and an HA topology.
//
// Authentication defaults to deny. With no accounts configured, every login is
// rejected — an empty configuration must not produce an open venue.
//
//	obgw -addr :9000 -symbol BTC-USD -accounts alice:s3cret,bob:hunter2
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:9000", "order-entry listen address")
		mdAddr   = flag.String("mdaddr", "", "market-data listen address (empty = no market-data feed)")
		symbol   = flag.String("symbol", "BTC-USD", "the single instrument this gateway serves")
		accounts = flag.String("accounts", "", "comma-separated user:password pairs")
		rate     = flag.Float64("rate", 1000, "per-account orders/second")
		burst    = flag.Float64("burst", 200, "per-account burst allowance")
		walPath  = flag.String("wal", "", "write-ahead log path (empty = no durability)")
		snapPath = flag.String("snapshot", "", "snapshot path, used with -wal to bound restart time")
		ckpt     = flag.Duration("checkpoint", 30*time.Second, "checkpoint interval")
	)
	flag.Parse()

	cfg := Config{
		Addr:            *addr,
		MDAddr:          *mdAddr,
		Symbol:          *symbol,
		Accounts:        parseAccounts(*accounts),
		RatePerSec:      *rate,
		Burst:           *burst,
		WALPath:         *walPath,
		SnapshotPath:    *snapPath,
		CheckpointEvery: *ckpt,
	}
	if len(cfg.Accounts) == 0 {
		log.Println("obgw: no accounts configured — every login will be rejected")
	}
	if cfg.WALPath == "" {
		log.Println("obgw: no -wal path — running WITHOUT durability; a crash loses the book")
	}

	srv, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("obgw: %v", err)
	}
	if err := srv.Listen(); err != nil {
		log.Fatalf("obgw: listen: %v", err)
	}
	log.Printf("obgw: serving %s on %s (incarnation %s)", cfg.Symbol, srv.Addr(), cfg.Incarnation)

	// Drain on SIGTERM rather than dying mid-command: the Runner's fence lets
	// in-flight producers finish instead of panicking them.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("obgw: draining")
		srv.Close()
	}()

	if err := srv.Serve(); err != nil {
		log.Fatalf("obgw: serve: %v", err)
	}
	log.Println("obgw: stopped")
}

// parseAccounts reads user:password pairs. A malformed entry is skipped with a
// warning rather than silently ignored — a typo in a credential list must not
// quietly leave an account unusable.
func parseAccounts(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		user, pass, ok := strings.Cut(pair, ":")
		if !ok || user == "" || pass == "" {
			log.Printf("obgw: ignoring malformed account entry %q (want user:password)", pair)
			continue
		}
		out[user] = pass
	}
	return out
}
