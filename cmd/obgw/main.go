// Command obgw is a reference order-entry gateway: a real TCP server speaking the
// binary protocol in internal/wire, in front of one matching engine.
//
// It exists to demonstrate the seam, not to be deployed. What it shows is that
// the library's pieces — the Runner's fire-and-forget enqueue, the gateway's
// admission control, the event stream, and per-account outbound streams that
// survive a disconnect — compose into a working venue edge. What it deliberately
// omits is what a real deployment must decide for itself: multi-symbol routing and
// an HA topology (docs/REPLICATION.md and examples/replication are the reference
// for the latter).
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
// An entry is either "user:password" or "user:sha256:<64 hex>", the latter produced by
// -hash-secret. Whichever form arrives, the process's credential table holds digests:
// a plaintext entry is hashed at load and the venue says how many it had to. What that
// does not fix is the file itself — a plaintext entry is still plaintext on disk — and
// the transient copies parsing leaves behind, which are garbage to the collector, not
// zeroed. An inline -accounts string is worse: the flag package keeps it reachable for
// the life of the process, digest table or not.
//
// Where credentials LIVE is still yours. Config.Auth takes an orderentry.Authenticator;
// the wiring here is HashedAccounts, whose documentation states what a fast hash does
// and does not buy, and there is still no rotation, revocation or expiry behind it.
//
//	obgw -addr :9000 -symbol BTC-USD -accounts-file /etc/obgw/accounts -admin 127.0.0.1:9100
//
// The admin listener is a separate port on purpose: it is for whoever runs the venue,
// and it should be reachable from a monitoring network that participants cannot reach
// at all.
//
// # Durability window
//
// Commands are journalled before they are applied, so the log is never missing
// something the book did. That is ordered against APPLY, not against the
// acknowledgement a client receives: by default the log is group-committed every
// 20ms, so a process that dies inside that window can lose an order it already
// acknowledged. -sync-every-command closes the window by fsyncing each command
// before applying it, which is correct and roughly 210× more expensive because
// the fsync lands on the matching goroutine. Pick one deliberately; the default
// is throughput, and it is stated rather than hidden.
package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

// splitSymbols turns the comma-separated -symbols flag into a list. Empty means
// the venue serves the single -symbol, which is what every existing deployment
// does and what the config defaults to.
func splitSymbols(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	var out []string
	for _, sym := range strings.Split(list, ",") {
		if sym = strings.TrimSpace(sym); sym != "" {
			out = append(out, sym)
		}
	}
	return out
}

func main() {
	var (
		addr         = flag.String("addr", "127.0.0.1:9000", "order-entry listen address")
		mdAddr       = flag.String("mdaddr", "", "market-data listen address (empty = no market-data feed)")
		adminAddr    = flag.String("admin", "", "admin HTTP listen address for /metrics, /healthz and /readyz (empty = unobserved)")
		symbol       = flag.String("symbol", "BTC-USD", "the instrument this gateway serves (one book)")
		symbols      = flag.String("symbols", "", "comma-separated instruments; overrides -symbol and requires -datadir")
		dataDir      = flag.String("datadir", "", "directory for the venue manifest and one log/snapshot per instrument (multi-symbol only)")
		accounts     = flag.String("accounts", "", "comma-separated user:password pairs (VISIBLE IN ps OUTPUT — prefer -accounts-file)")
		accountsFile = flag.String("accounts-file", "", "file of user:password or user:sha256:<64 hex> lines; # comments allowed")
		hashSecret   = flag.Bool("hash-secret", false, "read a secret on stdin, print its sha256: credential-file form, and exit")
		tlsCert      = flag.String("tls-cert", "", "PEM certificate; with -tls-key, wraps every listener in TLS")
		tlsKey       = flag.String("tls-key", "", "PEM private key")
		rate         = flag.Float64("rate", 1000, "per-account orders/second")
		burst        = flag.Float64("burst", 200, "per-account burst allowance")
		walPath      = flag.String("wal", "", "write-ahead log path (empty = no durability)")
		snapPath     = flag.String("snapshot", "", "snapshot path, used with -wal to bound restart time")
		ckpt         = flag.Duration("checkpoint", 30*time.Second, "checkpoint interval")
		syncEvery    = flag.Bool("sync-every-command", false, "fsync each command before applying it, so durability precedes acknowledgement (correct, and ~210x slower than the 20ms group commit)")
	)
	flag.Parse()

	if *hashSecret {
		if err := printHashedSecret(os.Stdin, os.Stdout); err != nil {
			log.Fatalf("obgw: %v", err)
		}
		return
	}

	accts, err := loadAccounts(*accounts, *accountsFile)
	if err != nil {
		log.Fatalf("obgw: %v", err)
	}
	auth := orderentry.NewHashedAccounts(accts)

	tlsCfg, err := loadTLS(*tlsCert, *tlsKey)
	if err != nil {
		log.Fatalf("obgw: %v", err)
	}

	cfg := Config{
		Addr:             *addr,
		MDAddr:           *mdAddr,
		AdminAddr:        *adminAddr,
		Symbol:           *symbol,
		Symbols:          splitSymbols(*symbols),
		DataDir:          *dataDir,
		Auth:             auth,
		TLS:              tlsCfg,
		RatePerSec:       *rate,
		Burst:            *burst,
		WALPath:          *walPath,
		SnapshotPath:     *snapPath,
		CheckpointEvery:  *ckpt,
		SyncEveryCommand: *syncEvery,
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
	} else if !cfg.SyncEveryCommand {
		log.Println("obgw: group-committing every 20ms — an acknowledged order can be lost if the process dies inside that window; -sync-every-command closes it at ~210x the cost")
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
	log.Printf("obgw: serving %s on %s (incarnation %s)", strings.Join(cfg.Symbols, ", "), srv.Addr(), cfg.Incarnation)
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
//
// Whatever form the entries arrive in, what comes back is digests: a plaintext entry
// is hashed here, so the credential table the process holds for its lifetime never
// contains a password. The count of plaintext entries is reported — the file still
// exposes those on disk, and pretending otherwise is how a "hashed" credential file
// keeps its passwords.
func loadAccounts(inline, path string) (map[string][sha256.Size]byte, error) {
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
	out := map[string][sha256.Size]byte{}
	plain := 0
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, rest, ok := strings.Cut(line, ":")
		if !ok || user == "" || rest == "" {
			// The LINE NUMBER, never the line. This is exactly where a helpful
			// "ignoring bad entry %q" writes a password into a log — and unlike a
			// process list, a log is kept, shipped and indexed.
			log.Printf("obgw: %s line %d is malformed (want user:password or user:sha256:<64 hex>); ignoring it", path, i+1)
			continue
		}
		digest, wasPlain, ok := parseSecret(rest)
		if !ok {
			log.Printf("obgw: %s line %d has a malformed sha256: digest; ignoring it", path, i+1)
			continue
		}
		if wasPlain {
			plain++
		}
		out[user] = digest
	}
	if plain > 0 {
		log.Printf("obgw: %s holds %d plaintext secret(s), hashed at load — the file itself still exposes them; generate sha256: entries with -hash-secret", path, plain)
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
func parseAccounts(s string) map[string][sha256.Size]byte {
	out := map[string][sha256.Size]byte{}
	for i, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		user, rest, ok := strings.Cut(pair, ":")
		if !ok || user == "" || rest == "" {
			log.Printf("obgw: ignoring malformed entry %d of -accounts (want user:password)", i+1)
			continue
		}
		digest, _, ok := parseSecret(rest)
		if !ok {
			log.Printf("obgw: ignoring malformed sha256: digest at entry %d of -accounts", i+1)
			continue
		}
		out[user] = digest
	}
	return out
}

// parseSecret turns the text after the first colon of a credential entry into the
// digest the table stores. A "sha256:" prefix marks a pre-hashed entry — 64 hex
// digits, the output of -hash-secret; anything else is a plaintext secret, hashed
// here. The prefix is therefore reserved: a password that itself begins with
// "sha256:" cannot be expressed as a plaintext entry, and a digest that does not
// parse is refused rather than guessed at, because falling back to "treat it as a
// very strange password" would turn one mistyped hex digit into an account whose
// real secret nobody knows.
func parseSecret(rest string) (digest [sha256.Size]byte, plaintext, ok bool) {
	hexDigest, pre := strings.CutPrefix(rest, "sha256:")
	if !pre {
		return orderentry.HashSecret(rest), true, true
	}
	raw, err := hex.DecodeString(hexDigest)
	if err != nil || len(raw) != sha256.Size {
		return digest, false, false
	}
	copy(digest[:], raw)
	return digest, false, true
}

// printHashedSecret reads one line and prints the sha256: entry form for a credential
// file. The secret arrives on stdin rather than argv for the same reason
// -accounts-file exists: an argument is in the host's process list for every user on
// the box. Nothing here echoes, logs or retains the input.
func printHashedSecret(in io.Reader, out io.Writer) error {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read secret: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return errors.New("no secret on stdin")
	}
	digest := orderentry.HashSecret(line)
	_, err = fmt.Fprintf(out, "sha256:%s\n", hex.EncodeToString(digest[:]))
	return err
}
