// Package auth holds the pieces of authentication that need no storage:
// password hashing, signed session tokens, and a login rate limiter.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// ---- passwords ----

// PasswordParams are the Argon2id cost settings used for new hashes. Hashes
// carry their own parameters, so changing these never breaks existing ones.
type PasswordParams struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
}

// Params is deliberately costly (about 0.1 s and 64 MiB per login).
var Params = PasswordParams{Time: 3, MemoryKiB: 64 * 1024, Threads: 2}

const (
	keyLen  = 32
	saltLen = 16
)

// ValidPassword checks the rules for choosing a password.
func ValidPassword(pw string) error {
	switch {
	case len(pw) < 8:
		return errors.New("the password must have at least 8 characters")
	case len(pw) > 256:
		return errors.New("the password must have at most 256 bytes")
	}
	return nil
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidUsername reports whether name may be used as a user name.
func ValidUsername(name string) bool { return usernameRe.MatchString(name) }

// HashPassword returns an encoded Argon2id hash of pw:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, Params.Time, Params.MemoryKiB, Params.Threads, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		Params.MemoryKiB, Params.Time, Params.Threads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword reports whether pw matches an encoded hash. Malformed hashes
// never match.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 || m > 1<<21 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// BurnPassword spends the time of a real verification. Call it when the user
// does not exist, so the response time does not reveal which names are taken.
func BurnPassword(pw string) {
	dummyOnce.Do(func() { dummyHash, _ = HashPassword("not a real password") })
	VerifyPassword(pw, dummyHash)
}

// ---- tokens ----

// RandomToken returns n random bytes, URL-safe base64 encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how secret tokens are stored: only a hash is kept, so a copy of
// the database does not hand out working tokens.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Signer makes and checks short-lived access tokens and CSRF tokens.
type Signer struct{ key []byte }

// NewSigner returns a signer for the given secret (at least 16 bytes).
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < 16 {
		return nil, errors.New("signing key too short")
	}
	return &Signer{key: append([]byte(nil), key...)}, nil
}

func (s *Signer) mac(data string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// Sign returns an access token for user uid in session sid, valid until exp.
func (s *Signer) Sign(uid, sid int64, exp time.Time) string {
	payload := fmt.Sprintf("a1|%d|%d|%d", uid, sid, exp.Unix())
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload))
}

// Verify checks a token's signature and expiry.
func (s *Signer) Verify(tok string, now time.Time) (uid, sid int64, ok bool) {
	p, sig, found := strings.Cut(tok, ".")
	if !found {
		return 0, 0, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(got, s.mac(string(payload))) {
		return 0, 0, false
	}
	f := strings.Split(string(payload), "|")
	if len(f) != 4 || f[0] != "a1" {
		return 0, 0, false
	}
	u, e1 := strconv.ParseInt(f[1], 10, 64)
	sd, e2 := strconv.ParseInt(f[2], 10, 64)
	exp, e3 := strconv.ParseInt(f[3], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || now.Unix() >= exp {
		return 0, 0, false
	}
	return u, sd, true
}

// CSRF returns the anti-forgery token of a session. A page's script reads it
// from a cookie and sends it back in a header, which another site cannot do.
func (s *Signer) CSRF(sid int64) string {
	return base64.RawURLEncoding.EncodeToString(s.mac("csrf|" + strconv.FormatInt(sid, 10)))
}

// CheckCSRF compares a presented token with the session's, in constant time.
func (s *Signer) CheckCSRF(sid int64, presented string) bool {
	return presented != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(s.CSRF(sid))) == 1
}

// ---- rate limiting ----

// Limiter allows a limited number of failures per key in a time window.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
	now    func() time.Time
}

// NewLimiter returns a limiter that blocks a key after max failures within window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, hits: map[string][]time.Time{}, now: time.Now}
}

// SetClock replaces the time source (for tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	recent := l.hits[key][:0:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = recent
	}
	return recent
}

// Blocked reports whether key has used up its failures, and for how long.
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	recent := l.prune(key, now)
	if len(recent) < l.max {
		return false, 0
	}
	return true, recent[len(recent)-l.max].Add(l.window).Sub(now)
}

// Fail records a failure for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(key, now)
	l.hits[key] = append(l.hits[key], now)
	if len(l.hits) > 10000 { // bound memory under a flood of distinct keys
		for k := range l.hits {
			l.prune(k, now)
		}
	}
}

// Reset forgets key's failures (after a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
