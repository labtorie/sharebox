// Package security provides credentials, guest tokens, password hashing and
// rate limiting for a sharing session.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/time/rate"
)

var b64 = base64.RawURLEncoding

// RandomToken returns n bytes of CSPRNG output, base64url encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return b64.EncodeToString(b)
}

// RandomID returns a short random identifier (not a secret).
func RandomID() string { return RandomToken(9) }

// HMAC computes HMAC-SHA256(key, parts...) with length-prefixed parts so
// that distinct inputs can never collide.
func HMAC(key []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, key)
	var l [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(l[:], uint32(len(p)))
		m.Write(l[:])
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// SignalToken derives the token a guest presents to the coordination server
// to join a session. The server stores only SHA-256 of it, and it cannot be
// used to authenticate to the host, so a compromised coordination server
// cannot read shared files.
func SignalToken(accessKey string) string {
	return b64.EncodeToString(HMAC([]byte(accessKey), "sharebox-signal-v1"))
}

// SignalHash is what the host registers with the coordination server.
func SignalHash(signalToken string) string {
	h := sha256.Sum256([]byte(signalToken))
	return b64.EncodeToString(h[:])
}

// FingerprintMAC binds the host's DTLS fingerprint to the access key so a
// guest can detect a coordination server that tampers with signalling.
func FingerprintMAC(accessKey, fingerprint string) string {
	return b64.EncodeToString(HMAC([]byte(accessKey), "sharebox-dtls-v1", strings.ToLower(fingerprint)))
}

// Equal compares secrets in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// --- Password hashing -------------------------------------------------------

// PasswordHash is an scrypt hash of an optional share password.
type PasswordHash struct {
	Salt []byte
	Hash []byte
}

const scryptN, scryptR, scryptP, scryptLen = 1 << 15, 8, 1, 32

// HashPassword hashes a password. An empty password yields nil.
func HashPassword(pw string) (*PasswordHash, error) {
	if pw == "" {
		return nil, nil
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	h, err := scrypt.Key([]byte(pw), salt, scryptN, scryptR, scryptP, scryptLen)
	if err != nil {
		return nil, err
	}
	return &PasswordHash{Salt: salt, Hash: h}, nil
}

// Verify checks a password against the hash.
func (p *PasswordHash) Verify(pw string) bool {
	if p == nil {
		return true
	}
	h, err := scrypt.Key([]byte(pw), p.Salt, scryptN, scryptR, scryptP, scryptLen)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(h, p.Hash) == 1
}

// --- Guest tokens -----------------------------------------------------------

// ErrBadToken is returned for missing, malformed, expired or revoked tokens.
var ErrBadToken = errors.New("invalid or expired access token")

// Claims are the contents of a guest token.
type Claims struct {
	Guest  string `json:"g"`
	Epoch  uint64 `json:"n"`
	Expiry int64  `json:"e"`
}

// Issuer signs and verifies guest tokens for one sharing session. Rotating
// the epoch (Revoke) invalidates every outstanding token at once.
type Issuer struct {
	mu      sync.RWMutex
	key     []byte
	epoch   uint64
	revoked map[string]bool
	ttl     time.Duration
	now     func() time.Time
}

// NewIssuer creates an issuer with a fresh random signing key.
func NewIssuer(ttl time.Duration) *Issuer {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return &Issuer{key: k, ttl: ttl, revoked: map[string]bool{}, now: time.Now, epoch: 1}
}

// SetClock overrides the clock (tests).
func (i *Issuer) SetClock(now func() time.Time) { i.mu.Lock(); i.now = now; i.mu.Unlock() }

// Issue creates a token for a guest.
func (i *Issuer) Issue(guest string) (string, time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	exp := i.now().Add(i.ttl)
	c := Claims{Guest: guest, Epoch: i.epoch, Expiry: exp.Unix()}
	payload, _ := json.Marshal(c)
	p := b64.EncodeToString(payload)
	sig := b64.EncodeToString(HMAC(i.key, "sharebox-guest-v1", p))
	return p + "." + sig, exp
}

// Verify validates a token and returns its claims.
func (i *Issuer) Verify(tok string) (Claims, error) {
	p, sig, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 1024 {
		return Claims{}, ErrBadToken
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	want := b64.EncodeToString(HMAC(i.key, "sharebox-guest-v1", p))
	if !Equal(sig, want) {
		return Claims{}, ErrBadToken
	}
	raw, err := b64.DecodeString(p)
	if err != nil {
		return Claims{}, ErrBadToken
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrBadToken
	}
	if c.Epoch != i.epoch || i.now().Unix() >= c.Expiry || i.revoked[c.Guest] {
		return Claims{}, ErrBadToken
	}
	return c, nil
}

// RevokeAll invalidates every token issued so far.
func (i *Issuer) RevokeAll() {
	i.mu.Lock()
	i.epoch++
	i.revoked = map[string]bool{}
	i.mu.Unlock()
}

// RevokeGuest invalidates tokens of one guest.
func (i *Issuer) RevokeGuest(guest string) {
	i.mu.Lock()
	i.revoked[guest] = true
	i.mu.Unlock()
}

// --- Rate limiting ----------------------------------------------------------

// Limiter is a keyed token-bucket rate limiter with idle eviction.
type Limiter struct {
	mu      sync.Mutex
	r       rate.Limit
	burst   int
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	l    *rate.Limiter
	seen time.Time
}

// NewLimiter allows perSecond events per key with the given burst.
func NewLimiter(perSecond float64, burst int) *Limiter {
	return &Limiter{r: rate.Limit(perSecond), burst: burst, buckets: map[string]*bucket{}}
}

// Allow reports whether an event for key may proceed.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) > 100000 {
			return false // refuse rather than grow without bound
		}
		b = &bucket{l: rate.NewLimiter(l.r, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.l.Allow()
}
