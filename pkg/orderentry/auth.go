package orderentry

import (
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
