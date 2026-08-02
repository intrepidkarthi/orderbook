package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrepidkarthi/orderbook/internal/wire"
	"github.com/intrepidkarthi/orderbook/pkg/orderentry"
)

// selfSignedTLS builds a throwaway certificate for 127.0.0.1.
func selfSignedTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "obgw-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}, pool
}

// TestTLSVenueRefusesAPlaintextClient — the point of turning TLS on is that a client
// which does not is not quietly served.
func TestTLSVenueRefusesAPlaintextClient(t *testing.T) {
	serverTLS, pool := selfSignedTLS(t)
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		TLS: serverTLS,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)

	// A plaintext login attempt. The server reads the record as a TLS handshake, fails
	// it, and hangs up; the client must not end up logged in.
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	b, _ := wire.EncodeLoginRequest(nil, wire.LoginRequest{Username: "alice", Password: "pw1"})
	_ = wire.WritePacket(conn, wire.PacketLoginRequest, b)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if pkt, err := wire.ReadPacket(conn, make([]byte, wire.MaxPayload)); err == nil && pkt.Type == wire.PacketLoginAccepted {
		t.Fatal("a plaintext client was accepted by a TLS venue")
	}

	// And a TLS client is served normally.
	tlsConn, err := tls.Dial("tcp", srv.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer tlsConn.Close()
	if err := wire.WritePacket(tlsConn, wire.PacketLoginRequest, b); err != nil {
		t.Fatalf("write over TLS: %v", err)
	}
	_ = tlsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	pkt, err := wire.ReadPacket(tlsConn, make([]byte, wire.MaxPayload))
	if err != nil {
		t.Fatalf("read over TLS: %v", err)
	}
	if pkt.Type != wire.PacketLoginAccepted {
		t.Fatalf("TLS login refused (packet %q)", pkt.Type)
	}
}

// TestASlowHandshakeDoesNotHoldUpTheAcceptLoop — the handshake happens on the
// connection's own goroutine, so one peer that completes a TCP connection and then says
// nothing must not delay anybody else. Connect-and-stall is the cheapest denial there
// is, and doing the handshake on the accept loop would make it work.
func TestASlowHandshakeDoesNotHoldUpTheAcceptLoop(t *testing.T) {
	serverTLS, pool := selfSignedTLS(t)
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		Accounts:      map[string]string{"alice": "pw1"},
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
		TLS: serverTLS,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)

	// Several peers that connect and never speak.
	for i := 0; i < 5; i++ {
		stalled, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer stalled.Close()
	}

	done := make(chan error, 1)
	go func() {
		c, err := tls.Dial("tcp", srv.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		b, _ := wire.EncodeLoginRequest(nil, wire.LoginRequest{Username: "alice", Password: "pw1"})
		if err := wire.WritePacket(c, wire.PacketLoginRequest, b); err != nil {
			done <- err
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = wire.ReadPacket(c, make([]byte, wire.MaxPayload))
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a healthy client could not log in behind stalled peers: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stalled handshakes blocked the accept loop")
	}
}

// TestCredentialsNeverReachTheLog is a drill for the failure that has no runbook,
// because by the time you need one it is already in your log aggregator.
//
// The obvious version of the credential parser reported malformed entries by printing
// them, which meant a single typo wrote a password to disk. Unlike a process list, a
// log is kept, shipped and indexed.
func TestCredentialsNeverReachTheLog(t *testing.T) {
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	const secret = "hunter2-do-not-log-me"

	// Malformed on the command line: a missing colon, with the secret still present.
	parseAccounts("alice" + secret + ",bob:pw")

	// Malformed in a file, same shape.
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts")
	if err := os.WriteFile(path, []byte("alice"+secret+"\ncarol:pw\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadAccounts("", path); err != nil {
		t.Fatalf("loadAccounts: %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("a secret reached the log:\n%s", buf.String())
	}
	// The operator still has to be able to find the bad line, or the safe behaviour
	// gets reverted by whoever next has to debug a credential file at 2am.
	if !strings.Contains(buf.String(), "line 1") {
		t.Errorf("the malformed line was not located for the operator:\n%s", buf.String())
	}
}

// TestAWorldReadableCredentialFileIsCalledOut — a credential file the whole host can
// read is the same exposure as putting it on the command line, arrived at more
// carefully.
func TestAWorldReadableCredentialFileIsCalledOut(t *testing.T) {
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	path := filepath.Join(t.TempDir(), "accounts")
	if err := os.WriteFile(path, []byte("alice:pw\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	accts, err := loadAccounts("", path)
	if err != nil {
		t.Fatalf("loadAccounts: %v", err)
	}
	if len(accts) != 1 {
		t.Fatalf("read %d accounts, want 1", len(accts))
	}
	if !strings.Contains(buf.String(), "readable by other users") {
		t.Errorf("a world-readable credential file drew no warning:\n%s", buf.String())
	}
}

// TestAPreHashedEntryAuthenticates — the round trip that makes -hash-secret worth
// having: an entry written in digest form admits the password it was made from, and
// mixes with plaintext entries in the same file, because a migration that requires
// rewriting every line at once is a migration that gets scheduled and never run.
func TestAPreHashedEntryAuthenticates(t *testing.T) {
	var entry strings.Builder
	if err := printHashedSecret(strings.NewReader("s3cret\n"), &entry); err != nil {
		t.Fatalf("printHashedSecret: %v", err)
	}
	path := filepath.Join(t.TempDir(), "accounts")
	content := "alice:" + entry.String() + "bob:plainpw\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	accts, err := loadAccounts("", path)
	if err != nil {
		t.Fatalf("loadAccounts: %v", err)
	}
	auth := orderentry.NewHashedAccounts(accts)
	if !auth.Authenticate("alice", "s3cret") {
		t.Error("a pre-hashed entry refused the password it was generated from")
	}
	if !auth.Authenticate("bob", "plainpw") {
		t.Error("a plaintext entry in the same file was refused")
	}
	if auth.Authenticate("alice", "wrong") {
		t.Error("a wrong password authenticated against a pre-hashed entry")
	}
}

// TestAMalformedDigestIsRefusedByLineNumber — one mistyped hex digit must not fall
// back to "treat it as a very strange password", because that manufactures an account
// whose real secret nobody knows. The line is located for the operator, and the entry
// content — which for the plaintext-misfiled-as-digest case IS a secret — stays out
// of the log.
func TestAMalformedDigestIsRefusedByLineNumber(t *testing.T) {
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	const almostDigest = "sha256:not-hex-do-not-log-me"
	path := filepath.Join(t.TempDir(), "accounts")
	if err := os.WriteFile(path, []byte("alice:"+almostDigest+"\ncarol:pw\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	accts, err := loadAccounts("", path)
	if err != nil {
		t.Fatalf("loadAccounts: %v", err)
	}
	if _, ok := accts["alice"]; ok {
		t.Error("a malformed digest produced an account")
	}
	if _, ok := accts["carol"]; !ok {
		t.Error("the valid line after a malformed one was lost")
	}
	if strings.Contains(buf.String(), "not-hex-do-not-log-me") {
		t.Errorf("a refused entry reached the log by content:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "line 1") {
		t.Errorf("the malformed digest was not located for the operator:\n%s", buf.String())
	}
}

// TestPlaintextEntriesAreCalledOut — hashing at load fixes what the process holds,
// not what the file does. The count is logged so an operator can watch it reach zero;
// the secrets themselves, as everywhere in this file, do not appear.
func TestPlaintextEntriesAreCalledOut(t *testing.T) {
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	path := filepath.Join(t.TempDir(), "accounts")
	if err := os.WriteFile(path, []byte("alice:plain-one\nbob:plain-two\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadAccounts("", path); err != nil {
		t.Fatalf("loadAccounts: %v", err)
	}
	if !strings.Contains(buf.String(), "2 plaintext secret(s)") {
		t.Errorf("plaintext entries drew no call-out:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "plain-one") || strings.Contains(buf.String(), "plain-two") {
		t.Errorf("a secret reached the log:\n%s", buf.String())
	}
}

// TestHashSecretRefusesEmptyInput — piping nothing into -hash-secret must be an
// error, not the digest of "", because that digest is exactly the entry
// NewHashedAccounts refuses, and producing it here would teach the operator a form
// the loader then silently drops.
func TestHashSecretRefusesEmptyInput(t *testing.T) {
	var out strings.Builder
	if err := printHashedSecret(strings.NewReader(""), &out); err == nil {
		t.Error("an empty stdin produced a credential entry")
	}
	if err := printHashedSecret(strings.NewReader("\n"), &out); err == nil {
		t.Error("a bare newline produced a credential entry")
	}
}

// TestTwoCredentialSourcesAreRefused — two sources of truth for who may trade is a
// question nobody wants to be answering during an incident.
func TestTwoCredentialSourcesAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts")
	if err := os.WriteFile(path, []byte("alice:pw\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadAccounts("bob:pw", path); err == nil {
		t.Error("-accounts and -accounts-file were both accepted")
	}
}

// TestTheAuthSeamIsUsed — Config.Auth must actually replace the built-in map, or the
// interface is documentation rather than a seam.
func TestTheAuthSeamIsUsed(t *testing.T) {
	srv := mustServer(t, Config{
		Addr: "127.0.0.1:0", Symbol: "X", Incarnation: "INC1",
		// Accounts says alice/pw1; Auth says otherwise and must win.
		Accounts:      map[string]string{"alice": "pw1"},
		Auth:          orderentry.NewStaticAccounts(map[string]string{"zoe": "other"}),
		OutboundDepth: 64, StreamRing: 4096, RatePerSec: 1e6, Burst: 1e6,
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)

	c := dial(t, srv)
	if pkt, err := c.login("alice", "pw1", "", 0); err == nil && pkt.Type == wire.PacketLoginAccepted {
		t.Error("Config.Accounts was consulted even though Config.Auth was set")
	}

	c2 := dial(t, srv)
	c2.mustLogin("zoe", "other")
}
