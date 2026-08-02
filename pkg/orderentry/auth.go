package orderentry

import (
	"crypto/sha256"
	"crypto/subtle"
)

// Authenticator decides whether a login names a real account.
//
// It is an interface because credential storage is the one decision a venue cannot
// delegate to a library. Where your secrets live, how they are hashed, how they rotate
// and who can read them are properties of your deployment and your regulator, and a
// library that picked for you would be wrong for most of the people who used it.
//
// What the library can do is refuse to make the choice badly on your behalf, and state
// the two obligations that are easy to miss:
//
//   - **Authenticate must not reveal which half was wrong.** Not through the return
//     value, which is a bool for that reason, and not through timing. An unknown
//     account that returns faster than a wrong password turns the login endpoint into
//     a way to enumerate the venue's participants.
//   - **It must never log the secret.** Not on success, not on failure, and especially
//     not on a malformed entry — the code that parses a credential list is exactly
//     where a well-meaning "ignoring bad entry %q" ends up writing a password to disk.
//
// It must be safe for concurrent use: it is called from every connection's goroutine.
type Authenticator interface {
	Authenticate(account, secret string) bool
}

// StaticAccounts is an in-memory credential map, comparing secrets in constant time.
//
// # Not suitable for production, and specific about why
//
// The secrets are held in plaintext in the process's memory and compared verbatim, so
// this is only as good as the place you loaded them from. It has no hashing, no
// rotation, no revocation and no expiry. A core dump contains every password on the
// venue.
//
// It exists so that the reference gateway has a default that is *correct about the
// things a default can be correct about* — constant-time comparison, deny by default,
// no leaking of which half was wrong — rather than a naive one that quietly teaches
// the wrong pattern to whoever copies it.
//
// Replace it. The interface is one method.
type StaticAccounts struct {
	accounts map[string]string
	// decoy is compared against when the account is unknown, so a bad username costs
	// the same as a bad password. Its length is the mean of the real secrets, because
	// comparing against a constant of some unrelated length would reintroduce the
	// timing difference it exists to remove.
	decoy []byte
}

// NewStaticAccounts builds an authenticator over a username→password map.
//
// An empty map authenticates nobody, which is the correct default: an empty
// configuration must not produce an open venue.
func NewStaticAccounts(accounts map[string]string) *StaticAccounts {
	a := &StaticAccounts{accounts: make(map[string]string, len(accounts))}
	total := 0
	for user, secret := range accounts {
		// An account with no secret is not an account. Admitting one would mean a
		// blank password authenticated, which is how a configuration typo becomes an
		// open door.
		if user == "" || secret == "" {
			continue
		}
		a.accounts[user] = secret
		total += len(secret)
	}
	n := 16
	if len(a.accounts) > 0 {
		n = total / len(a.accounts)
	}
	a.decoy = make([]byte, n)
	return a
}

// Count reports how many usable accounts were configured, so a caller can warn about
// an empty credential list without reading the credentials.
func (a *StaticAccounts) Count() int { return len(a.accounts) }

// Authenticate reports whether the secret matches the account's.
//
// The unknown-account path still performs a comparison. Returning early would make a
// bad username measurably faster than a bad password, and that difference is a way to
// ask the venue which of its participants exist — one connection at a time, with no
// successful login and nothing in an audit trail that looks like an attack.
func (a *StaticAccounts) Authenticate(account, secret string) bool {
	want, ok := a.accounts[account]
	if !ok {
		subtle.ConstantTimeCompare([]byte(secret), a.decoy)
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(want)) == 1
}

// HashSecret is the digest form HashedAccounts stores and compares: SHA-256 over the
// raw bytes of the secret. It is exported so that provisioning tooling and the
// credential loader agree on the form by construction rather than by convention.
func HashSecret(secret string) [sha256.Size]byte {
	return sha256.Sum256([]byte(secret))
}

// HashedAccounts is an in-memory table of SHA-256 secret digests, comparing in
// constant time. It is StaticAccounts with the plaintext removed: a memory disclosure
// of a process using it yields digests, not passwords.
//
// # Why the hash is fast, and when that is wrong
//
// SHA-256 rather than a slow, memory-hard hash (argon2, scrypt, bcrypt) is a scoping
// decision, not an oversight. These are machine credentials: the venue issues them, so
// it can issue them from a CSPRNG at 128 bits or more, and against a secret like that
// a fast hash is enough — an attacker holding the digest cannot enumerate a 128-bit
// space however cheap each guess is. What a memory-hard hash would add is protection
// for low-entropy, human-chosen passwords, and it would add it in the worst possible
// place: this function runs on the pre-authentication login path, so a memory-hard
// hash there hands every unauthenticated peer a CPU-and-memory amplification
// primitive aimed at the accept loop. If your secrets are chosen by humans, the fix
// is not a slower hash here — it is implementing Authenticator over a real credential
// system, which is what the seam is for.
//
// Still absent, as with StaticAccounts: rotation, revocation and expiry. The
// difference between the two is confined to what a memory disclosure is worth.
//
// The decoy arithmetic from StaticAccounts simplifies here rather than repeating:
// every digest is sha256.Size bytes, so the unknown-account path compares against a
// zero digest of the same width and no length averaging is needed.
type HashedAccounts struct {
	accounts map[string][sha256.Size]byte
	decoy    [sha256.Size]byte
}

// NewHashedAccounts builds an authenticator over an account→digest map.
//
// An empty map authenticates nobody. An account with a blank name is refused, and so
// is an account whose digest is HashSecret(""): storing that one would mean a blank
// password authenticated, which is the same open door the blank-secret rule in
// NewStaticAccounts exists to close, arrived at in digest form.
func NewHashedAccounts(accounts map[string][sha256.Size]byte) *HashedAccounts {
	blank := HashSecret("")
	a := &HashedAccounts{accounts: make(map[string][sha256.Size]byte, len(accounts))}
	for user, digest := range accounts {
		if user == "" || digest == blank {
			continue
		}
		a.accounts[user] = digest
	}
	return a
}

// Count reports how many usable accounts were configured, so a caller can warn about
// an empty credential list without reading the credentials.
func (a *HashedAccounts) Count() int { return len(a.accounts) }

// Authenticate reports whether the secret's digest matches the account's.
//
// The presented secret is hashed BEFORE the account lookup, unconditionally. Hashing
// dominates the cost of this call at any realistic secret length, so a version that
// looked up first and skipped the hash for an unknown account would make a bad
// username measurably cheaper than a bad password — the enumeration channel from
// StaticAccounts, wearing a different implementation.
func (a *HashedAccounts) Authenticate(account, secret string) bool {
	got := HashSecret(secret)
	want, ok := a.accounts[account]
	if !ok {
		subtle.ConstantTimeCompare(got[:], a.decoy[:])
		return false
	}
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
