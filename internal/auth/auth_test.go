package auth

import (
	"strings"
	"testing"
	"time"
)

func init() { Params = PasswordParams{Time: 1, MemoryKiB: 8, Threads: 1} } // fast tests

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8,t=1,p=1$") || strings.Contains(h, "correct") {
		t.Fatalf("encoding: %s", h)
	}
	if !VerifyPassword("correct horse", h) || VerifyPassword("Correct horse", h) || VerifyPassword("", h) {
		t.Error("verification")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Error("hashes must be salted")
	}
	// A hash made with other parameters still verifies: it carries its own.
	old := Params
	Params = PasswordParams{Time: 2, MemoryKiB: 16, Threads: 2}
	h3, _ := HashPassword("x-password")
	Params = old
	if !VerifyPassword("x-password", h3) {
		t.Error("hash with other parameters")
	}
	for _, bad := range []string{"", "plain", "$argon2id$", "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA", "$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=8,t=1,p=1$!!$AAAA", "$argon2id$v=19$m=999999999,t=1,p=1$AAAA$AAAA", "$argon2id$v=18$m=8,t=1,p=1$AAAA$AAAA"} {
		if VerifyPassword("anything", bad) {
			t.Errorf("malformed hash %q verified", bad)
		}
	}
	BurnPassword("whatever") // must not panic
}

func TestPasswordAndNameRules(t *testing.T) {
	for pw, ok := range map[string]bool{"": false, "short": false, "1234567": false, "12345678": true, strings.Repeat("x", 256): true, strings.Repeat("x", 257): false} {
		if (ValidPassword(pw) == nil) != ok {
			t.Errorf("ValidPassword(len %d) = %v", len(pw), ValidPassword(pw))
		}
	}
	for n, ok := range map[string]bool{"alice": true, "a.b-c_d": true, "A1": true, "": false, "-x": false, ".x": false, "a b": false, "a/b": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "ünï": false} {
		if ValidUsername(n) != ok {
			t.Errorf("ValidUsername(%q) = %v", n, !ok)
		}
	}
}

func TestSignedTokens(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	s, _ := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Unix(1_700_000_000, 0)
	tok := s.Sign(7, 42, now.Add(time.Minute))
	if u, sid, ok := s.Verify(tok, now); !ok || u != 7 || sid != 42 {
		t.Fatalf("verify: %d %d %v", u, sid, ok)
	}
	if _, _, ok := s.Verify(tok, now.Add(time.Minute)); ok {
		t.Error("expired token accepted")
	}
	if _, _, ok := s.Verify(tok, now.Add(-time.Hour)); !ok {
		t.Error("token not yet near expiry rejected")
	}
	// Tampering with any part fails.
	p, sig, _ := strings.Cut(tok, ".")
	for _, bad := range []string{"", "x", p, "." + sig, p + ".", p + "." + sig + "A", "A" + p + "." + sig, tok[:len(tok)-2]} {
		if _, _, ok := s.Verify(bad, now); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	other, _ := NewSigner([]byte("fedcba9876543210fedcba9876543210"))
	if _, _, ok := other.Verify(tok, now); ok {
		t.Error("token verified under another key")
	}
	// CSRF tokens are per session.
	if s.CSRF(1) == s.CSRF(2) || !s.CheckCSRF(1, s.CSRF(1)) || s.CheckCSRF(1, s.CSRF(2)) || s.CheckCSRF(1, "") {
		t.Error("csrf")
	}
	if a, b := RandomToken(32), RandomToken(32); a == b || len(a) < 40 {
		t.Error("random tokens")
	}
	if HashToken("a") == HashToken("b") || len(HashToken("a")) != 64 {
		t.Error("hash")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter(3, time.Minute)
	l.SetClock(func() time.Time { return now })
	for i := 0; i < 2; i++ {
		l.Fail("k")
	}
	if b, _ := l.Blocked("k"); b {
		t.Fatal("blocked too early")
	}
	l.Fail("k")
	b, wait := l.Blocked("k")
	if !b || wait != time.Minute {
		t.Fatalf("blocked=%v wait=%v", b, wait)
	}
	if b, _ := l.Blocked("other"); b {
		t.Error("keys must be independent")
	}
	now = now.Add(30 * time.Second)
	if b, wait := l.Blocked("k"); !b || wait != 30*time.Second {
		t.Errorf("mid-window: %v %v", b, wait)
	}
	now = now.Add(31 * time.Second)
	if b, _ := l.Blocked("k"); b {
		t.Error("still blocked after the window")
	}
	for i := 0; i < 3; i++ {
		l.Fail("r")
	}
	l.Reset("r")
	if b, _ := l.Blocked("r"); b {
		t.Error("reset did not clear")
	}
}
