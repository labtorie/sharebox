package security

import (
	"strings"
	"testing"
	"time"
)

func TestTokensExpireAndRevoke(t *testing.T) {
	now := time.Now()
	iss := NewIssuer(time.Hour)
	iss.SetClock(func() time.Time { return now })
	tok, _ := iss.Issue("g1")
	if c, err := iss.Verify(tok); err != nil || c.Guest != "g1" {
		t.Fatalf("valid token rejected: %v", err)
	}
	// Tampering with the payload breaks the signature.
	p, sig, _ := strings.Cut(tok, ".")
	if _, err := iss.Verify(p + "x." + sig); err == nil {
		t.Fatal("tampered token accepted")
	}
	// A token from another session (different key) is rejected.
	other, _ := NewIssuer(time.Hour).Issue("g1")
	if _, err := iss.Verify(other); err == nil {
		t.Fatal("foreign token accepted")
	}
	iss.SetClock(func() time.Time { return now.Add(2 * time.Hour) })
	if _, err := iss.Verify(tok); err == nil {
		t.Fatal("expired token accepted")
	}
	iss.SetClock(func() time.Time { return now })
	iss.RevokeGuest("g1")
	if _, err := iss.Verify(tok); err == nil {
		t.Fatal("revoked guest accepted")
	}
	tok2, _ := iss.Issue("g2")
	iss.RevokeAll()
	if _, err := iss.Verify(tok2); err == nil {
		t.Fatal("token survived RevokeAll")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Verify("hunter2") || h.Verify("hunter3") || h.Verify("") {
		t.Fatal("password verification wrong")
	}
	if h, _ := HashPassword(""); h != nil || !h.Verify("anything") {
		t.Fatal("empty password should disable protection")
	}
}

func TestSignalTokenSeparation(t *testing.T) {
	key := RandomToken(32)
	st := SignalToken(key)
	if st == key || strings.Contains(st, key) || SignalHash(st) == st {
		t.Fatal("signal token must not reveal the key")
	}
	if SignalToken(key) != st || SignalToken(RandomToken(32)) == st {
		t.Fatal("signal token not deterministic per key")
	}
	if FingerprintMAC(key, "AB:CD") != FingerprintMAC(key, "ab:cd") {
		t.Fatal("fingerprint MAC must be case-insensitive")
	}
}

func TestHMACLengthPrefix(t *testing.T) {
	k := []byte("k")
	if string(HMAC(k, "ab", "c")) == string(HMAC(k, "a", "bc")) {
		t.Fatal("HMAC parts are ambiguous")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatal("burst denied")
		}
	}
	if l.Allow("a") {
		t.Fatal("limit not enforced")
	}
	if !l.Allow("b") {
		t.Fatal("keys not independent")
	}
}
