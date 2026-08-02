package orderentry

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestAuthenticationDefaultsToDeny — an empty configuration must not produce an open
// venue. This is the one default where being wrong is unrecoverable.
func TestAuthenticationDefaultsToDeny(t *testing.T) {
	a := NewStaticAccounts(nil)
	if a.Count() != 0 {
		t.Errorf("Count = %d, want 0", a.Count())
	}
	if a.Authenticate("alice", "") {
		t.Error("an empty credential set admitted an empty password")
	}
	if a.Authenticate("alice", "anything") {
		t.Error("an empty credential set admitted a login")
	}
}

// TestAnAccountWithNoSecretIsNotAnAccount — admitting one would mean a blank password
// authenticated, which is how a configuration typo becomes an open door.
func TestAnAccountWithNoSecretIsNotAnAccount(t *testing.T) {
	a := NewStaticAccounts(map[string]string{"alice": "", "": "pw", "bob": "pw2"})
	if a.Count() != 1 {
		t.Errorf("Count = %d, want only bob", a.Count())
	}
	if a.Authenticate("alice", "") {
		t.Error("an account with a blank secret authenticated")
	}
	if a.Authenticate("", "pw") {
		t.Error("an account with a blank name authenticated")
	}
	if !a.Authenticate("bob", "pw2") {
		t.Error("a valid account was refused")
	}
}

func TestWrongSecretIsRefused(t *testing.T) {
	a := NewStaticAccounts(map[string]string{"alice": "s3cret"})
	for _, secret := range []string{"", "s3cre", "s3cret ", "S3cret", "s3cretx"} {
		if a.Authenticate("alice", secret) {
			t.Errorf("secret %q was accepted", secret)
		}
	}
	if !a.Authenticate("alice", "s3cret") {
		t.Error("the correct secret was refused")
	}
}

// TestAnUnknownAccountDoesTheSameWorkAsAWrongSecret is the property the design of
// StaticAccounts turns on: if a bad username returns without doing the comparison, the
// login endpoint becomes a way to enumerate the venue's participants.
//
// The secret here is long — far longer than a real password — and that is the point.
// The first version of this test used a 64-byte secret and PASSED against a version of
// Authenticate that returned early on an unknown account, because at that length the
// comparison costs less than the map lookup that precedes it and the difference
// vanished into the noise. A test that cannot fail is decoration, so this one is sized
// so the comparison dominates and its absence is unmistakable.
//
// What that means for the real thing, stated rather than implied: at realistic secret
// lengths the timing difference a short-circuit would create is small, and probably not
// exploitable across a network. The constant-time path is kept because it costs nothing
// and because "probably not exploitable" is not a property anyone should have to
// re-derive later. This test proves the code does the work, not that the work was
// load-bearing.
func TestAnUnknownAccountDoesTheSameWorkAsAWrongSecret(t *testing.T) {
	secret := strings.Repeat("x", 1<<16)
	a := NewStaticAccounts(map[string]string{"alice": secret})
	wrong := strings.Repeat("y", 1<<16)

	const n = 2000
	measure := func(account string) time.Duration {
		start := time.Now()
		for i := 0; i < n; i++ {
			if a.Authenticate(account, wrong) {
				t.Fatal("test premise broken: a wrong secret authenticated")
			}
		}
		return time.Since(start)
	}
	measure("alice") // warm up
	measure("nobody")
	// Interleaved, so drift lands on both arms rather than whichever ran second.
	known := measure("alice") + measure("alice")
	unknown := measure("nobody") + measure("nobody")

	ratio := float64(known) / float64(unknown)
	if math.IsNaN(ratio) || ratio > 4 || ratio < 0.25 {
		t.Errorf("known-account path %v, unknown-account path %v (ratio %.2f) — one is short-circuiting, and that difference enumerates accounts",
			known, unknown, ratio)
	}
}

// TestTheDecoyIsSizedFromTheRealSecrets — comparing an unknown account against a
// constant of some unrelated length would reintroduce the timing difference the decoy
// exists to remove.
func TestTheDecoyIsSizedFromTheRealSecrets(t *testing.T) {
	a := NewStaticAccounts(map[string]string{
		"a": strings.Repeat("x", 10),
		"b": strings.Repeat("x", 20),
	})
	if got := len(a.decoy); got != 15 {
		t.Errorf("decoy length = %d, want the mean of the real secrets (15)", got)
	}
	// And it must exist even with no accounts, or the unknown path compares against
	// nothing and returns instantly.
	if got := len(NewStaticAccounts(nil).decoy); got == 0 {
		t.Error("no decoy with an empty account set; the unknown path would short-circuit")
	}
}

// TestAuthenticateIsSafeForConcurrentUse — it is called from every connection's
// goroutine, so this is the ordinary case rather than an edge one.
func TestAuthenticateIsSafeForConcurrentUse(t *testing.T) {
	a := NewStaticAccounts(map[string]string{"alice": "pw"})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 5000; j++ {
				if i%2 == 0 {
					a.Authenticate("alice", "pw")
				} else {
					a.Authenticate("mallory", "pw")
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
