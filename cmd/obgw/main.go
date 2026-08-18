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
	"strconv"
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

// parseSemanticsList reads -wal-accept-semantics.
//
// It refuses anything it does not understand rather than skipping it, because the one
// thing worse than a venue that will not start is a venue that starts having silently
// dropped half of what an operator typed during an incident.
func parseSemanticsList(list string) ([]int, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		v, err := strconv.Atoi(f)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("%q is not a semantics version; it is a comma-separated list of "+
				"non-negative integers, and 0 means a log written before the stamp existed", f)
		}
		out = append(out, v)
	}
	return out, nil
}

func main() {
	var (
		addr          = flag.String("addr", "127.0.0.1:9000", "order-entry listen address")
		mdAddr        = flag.String("mdaddr", "", "market-data listen address (empty = no market-data feed)")
		adminAddr     = flag.String("admin", "", "admin HTTP listen address for /metrics, /healthz and /readyz (empty = unobserved)")
		symbol        = flag.String("symbol", "BTC-USD", "the instrument this gateway serves (one book)")
		symbols       = flag.String("symbols", "", "comma-separated instruments; overrides -symbol and requires -datadir")
		dataDir       = flag.String("datadir", "", "directory for the venue manifest and one log/snapshot per instrument (multi-symbol only)")
		accounts      = flag.String("accounts", "", "comma-separated user:password pairs (VISIBLE IN ps OUTPUT — prefer -accounts-file)")
		accountsFile  = flag.String("accounts-file", "", "file of user:password or user:sha256:<64 hex> lines; # comments allowed")
		hashSecret    = flag.Bool("hash-secret", false, "read a secret on stdin, print its sha256: credential-file form, and exit")
		tlsCert       = flag.String("tls-cert", "", "PEM certificate; with -tls-key, wraps every listener in TLS")
		tlsKey        = flag.String("tls-key", "", "PEM private key")
		rate          = flag.Float64("rate", 1000, "per-account orders/second")
		burst         = flag.Float64("burst", 200, "per-account burst allowance")
		walPath       = flag.String("wal", "", "write-ahead log path (empty = no durability)")
		snapPath      = flag.String("snapshot", "", "snapshot path, used with -wal to bound how much log a restart replays and parses")
		ckpt          = flag.Duration("checkpoint", 30*time.Second, "checkpoint interval")
		segBytes      = flag.Int64("wal-segment-bytes", 0, "rotate the log into a new segment at this size (0 = 128MiB, negative = never rotate)")
		retainBytes   = flag.Int64("wal-retain", 0, "byte budget for the retained log; older segments are deleted once a verified snapshot covers them (0 = keep everything). A budget, not a bound: -wal-retain-segments floors it at (n+1) x -wal-segment-bytes, 640MiB at the defaults")
		retainSegs    = flag.Int("wal-retain-segments", 0, "sealed segments to keep regardless of coverage (0 = 4). Checked after -wal-retain and wins, so it decides the smallest the retained set can be")
		archiveDir    = flag.String("wal-archive", "", "copy each segment here before deleting it; without this, retention makes the newest snapshot the recovery point")
		minFree       = flag.Int64("wal-min-free", 0, "low-water mark in bytes: below it, warn and run retention immediately (0 = 2GiB)")
		minFreeStop   = flag.Int64("wal-min-free-stop", 0, "stop-water mark in bytes: below it, every book goes cancel-only (0 = 256MiB)")
		profiling     = flag.Bool("pprof", false, "mount net/http/pprof on the admin listener (operator-only; a heap dump exposes everything the venue holds)")
		syncEvery     = flag.Bool("sync-every-command", false, "fsync each command before applying it, so durability precedes acknowledgement (correct, and ~210x slower than the 20ms group commit)")
		acceptSem     = flag.String("wal-accept-semantics", "", "comma-separated matching semantics versions whose records this recovery may replay besides this build's; 0 means a log written before the stamp existed. Use it only after reading the refusal, and remove it after the next checkpoint — see docs/RUNBOOKS.md \"Upgrading across a semantics change\"")
		acceptIceLoss = flag.Int("wal-accept-iceberg-loss", 0, "number of iceberg records this recovery may replay that cannot state their hidden reserve — records written before the total was journalled. It must EQUAL the number the refusal names; a stale count refuses. The orders it lets through come back at their display size and have to be cancelled — see docs/RUNBOOKS.md \"An iceberg whose reserve was never journalled\"")
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
		Addr:              *addr,
		MDAddr:            *mdAddr,
		AdminAddr:         *adminAddr,
		Symbol:            *symbol,
		Symbols:           splitSymbols(*symbols),
		DataDir:           *dataDir,
		Auth:              auth,
		TLS:               tlsCfg,
		RatePerSec:        *rate,
		Burst:             *burst,
		WALPath:           *walPath,
		SnapshotPath:      *snapPath,
		CheckpointEvery:   *ckpt,
		WALSegmentBytes:   *segBytes,
		WALRetainBytes:    *retainBytes,
		WALRetainSegments: *retainSegs,
		WALArchiveDir:     *archiveDir,
		WALMinFree:        *minFree,
		WALMinFreeStop:    *minFreeStop,
		SyncEveryCommand:  *syncEvery,
		Profiling:         *profiling,
	}
	if cfg.WALAcceptSemantics, err = parseSemanticsList(*acceptSem); err != nil {
		log.Fatalf("obgw: -wal-accept-semantics: %v", err)
	}
	if *acceptIceLoss < 0 {
		log.Fatalf("obgw: -wal-accept-iceberg-loss %d: it is a COUNT of records, not a switch", *acceptIceLoss)
	}
	cfg.WALAcceptIcebergLoss = *acceptIceLoss
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
	// Rotation is on by default; deletion is not, and a venue that never deletes is
	// a venue that eventually cannot be restarted. Said at every start rather than
	// buried in a document, because the failure arrives on a schedule.
	if cfg.WALPath != "" && cfg.WALRetainBytes <= 0 {
		log.Println("obgw: -wal-retain is unset — the log will grow without bound (about 44 GiB a day at 2,500 msg/s) " +
			"and restart time grows with it. Set -wal-retain and -wal-archive.")
	}
	if cfg.WALRetainBytes > 0 && cfg.WALArchiveDir == "" {
		log.Println("obgw: -wal-retain without -wal-archive — deleted segments are gone, so this venue's recovery point " +
			"is its newest snapshot and one corrupt snapshot away from nothing.")
	}
	if len(cfg.WALAcceptSemantics) > 0 {
		// Said at every start, because the whole value of naming a version rather than
		// setting a boolean is that the decision goes stale and has to be re-made. A
		// flag that lives quietly in a unit file for three releases is the failure
		// docs/SEMANTICS-VERSION.md §3.5 is written to prevent.
		log.Printf("obgw: -wal-accept-semantics %v — recovery will replay records written by a build whose "+
			"matching behaviour is not this one's. Remove it once a checkpoint under this build has landed; "+
			"until then every restart is replaying somebody else's rules.", cfg.WALAcceptSemantics)
	}
	if cfg.WALAcceptIcebergLoss > 0 {
		// Said at every start, for the same reason the semantics one is: a count that
		// lives quietly in a unit file is a count that will silently accept the NEXT
		// such record — and this build cannot write one, so a next one means a foreign
		// writer, a downgrade, or a hand-edited log.
		log.Printf("obgw: -wal-accept-iceberg-loss %d — recovery will replay up to %d iceberg records "+
			"whose hidden reserve was never journalled, rebuilding each as an ordinary order of its "+
			"DISPLAY size. Cancel those orders, tell their owners to re-enter, and remove this flag "+
			"once a checkpoint under this build has landed.", cfg.WALAcceptIcebergLoss, cfg.WALAcceptIcebergLoss)
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
