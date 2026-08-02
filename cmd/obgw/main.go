// Command obgw is a reference order-entry gateway: a real TCP server speaking the
// binary protocol in internal/wire, in front of one matching engine.
//
// It exists to demonstrate the seam, not to be deployed. What it shows is that
// the library's pieces — the Runner's fire-and-forget enqueue, the gateway's
// admission control, the event stream, and per-account outbound streams that
// survive a disconnect — compose into a working venue edge. What it deliberately
// omits is what a real deployment must decide for itself: multi-symbol routing and
// an HA topology.
//
// Authentication defaults to deny. With no accounts configured, every login is
// rejected — an empty configuration must not produce an open venue.
//
// # Credentials and transport
//
// -tls-cert and -tls-key wrap every listener. Without them the venue speaks plaintext
// and sends passwords in the clear, which it says on startup — a thing to do on a
// loopback interface during development and nowhere else.
//
// Credentials come from -accounts-file in preference to -accounts, because anything on
// a command line is in the host's process list for every user on the box. The file's
// permissions are checked and a world-readable one draws a warning. Neither path ever
// logs a secret: a malformed entry is reported by line number, never by content.
//
// Where credentials LIVE is still yours. Config.Auth takes an orderentry.Authenticator,
// and the built-in one holds plaintext in memory with no hashing, rotation or
// revocation — correct about constant-time comparison and deny-by-default, and
// unsuitable for production for reasons its own documentation lists.
//
//	obgw -addr :9000 -symbol BTC-USD -accounts alice:s3cret,bob:hunter2 -admin 127.0.0.1:9100
//
// The admin listener is a separate port on purpose: it is for whoever runs the venue,
// and it should be reachable from a monitoring network that participants cannot reach
// at all.
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

func main() {
	var (
		addr         = flag.String("addr", "127.0.0.1:9000", "order-entry listen address")
		mdAddr       = flag.String("mdaddr", "", "market-data listen address (empty = no market-data feed)")
		adminAddr    = flag.String("admin", "", "admin HTTP listen address for /metrics, /healthz and /readyz (empty = unobserved)")
		symbol       = flag.String("symbol", "BTC-USD", "the single instrument this gateway serves")
		accounts     = flag.String("accounts", "", "comma-separated user:password pairs (VISIBLE IN ps OUTPUT — prefer -accounts-file)")
		accountsFile = flag.String("accounts-file", "", "file of user:password lines; # comments allowed")
		tlsCert      = flag.String("tls-cert", "", "PEM certificate; with -tls-key, wraps every listener in TLS")
		tlsKey       = flag.String("tls-key", "", "PEM private key")
		rate         = flag.Float64("rate", 1000, "per-account orders/second")
		burst        = flag.Float64("burst", 200, "per-account burst allowance")
		walPath      = flag.String("wal", "", "write-ahead log path (empty = no durability)")
		snapPath     = flag.String("snapshot", "", "snapshot path, used with -wal to bound restart time")
		ckpt         = flag.Duration("checkpoint", 30*time.Second, "checkpoint interval")
	)
	flag.Parse()

	accts, err := loadAccounts(*accounts, *accountsFile)
	if err != nil {
		log.Fatalf("obgw: %v", err)
	}
	auth := orderentry.NewStaticAccounts(accts)

	tlsCfg, err := loadTLS(*tlsCert, *tlsKey)
	if err != nil {
		log.Fatalf("obgw: %v", err)
	}

	cfg := Config{
		Addr:            *addr,
		MDAddr:          *mdAddr,
		AdminAddr:       *adminAddr,
		Symbol:          *symbol,
		Auth:            auth,
		TLS:             tlsCfg,
		RatePerSec:      *rate,
		Burst:           *burst,
		WALPath:         *walPath,
		SnapshotPath:    *snapPath,
		CheckpointEvery: *ckpt,
	}
	if auth.Count() == 0 {
		log.Println("obgw: no accounts configured — every login will be rejected")
	}
	if tlsCfg == nil {
		log.Println("obgw: no -tls-cert — running WITHOUT transport security; every login sends its password in the clear")
	}
	if *accounts != "" {
		log.Println("obgw: -accounts puts every password in this host's process list; use -accounts-file")
	}
	if cfg.WALPath == "" {
		log.Println("obgw: no -wal path — running WITHOUT durability; a crash loses the book")
	}
	if cfg.AdminAddr == "" {
		log.Println("obgw: no -admin address — running unobserved; nothing reports queue depth, book size or a stalled matcher")
	}

	srv, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("obgw: %v", err)
	}
	if err := srv.Listen(); err != nil {
		log.Fatalf("obgw: listen: %v", err)
	}
	log.Printf("obgw: serving %s on %s (incarnation %s)", cfg.Symbol, srv.Addr(), cfg.Incarnation)
	if a := srv.AdminAddr(); a != nil {
		log.Printf("obgw: admin on %s (/metrics, /healthz, /readyz)", a)
	}

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

// loadAccounts reads credentials from a file when one is given, and from the command
// line otherwise. Both may not be set: two sources of truth for who may trade is a
// question nobody wants to be answering during an incident.
func loadAccounts(inline, path string) (map[string]string, error) {
	if inline != "" && path != "" {
		return nil, errors.New("-accounts and -accounts-file are mutually exclusive")
	}
	if path == "" {
		return parseAccounts(inline), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	// Permissions checked rather than assumed. A credential file the whole host can
	// read is the same exposure as putting it on the command line, arrived at more
	// carefully.
	if fi, statErr := os.Stat(path); statErr == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Printf("obgw: %s is readable by other users (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, pass, ok := strings.Cut(line, ":")
		if !ok || user == "" || pass == "" {
			// The LINE NUMBER, never the line. This is exactly where a helpful
			// "ignoring bad entry %q" writes a password into a log — and unlike a
			// process list, a log is kept, shipped and indexed.
			log.Printf("obgw: %s line %d is malformed (want user:password); ignoring it", path, i+1)
			continue
		}
		out[user] = pass
	}
	return out, nil
}

// loadTLS builds a TLS configuration, or nil when the venue is to run in the clear.
func loadTLS(certPath, keyPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("-tls-cert and -tls-key must be given together")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	// A TLS 1.2 floor. Below it are versions with known attacks, and a venue that
	// negotiates one because a client asked has made that client's problem into
	// everybody's.
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// parseAccounts reads user:password pairs from a command line.
//
// A malformed entry is reported by POSITION, not by content. The obvious version of
// this logged the offending pair, which meant one typo in a credential list wrote a
// password to the log.
func parseAccounts(s string) map[string]string {
	out := map[string]string{}
	for i, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		user, pass, ok := strings.Cut(pair, ":")
		if !ok || user == "" || pass == "" {
			log.Printf("obgw: ignoring malformed entry %d of -accounts (want user:password)", i+1)
			continue
		}
		out[user] = pass
	}
	return out
}
