// Package grant signs and verifies narrow cache capabilities. The host must
// authorize the job before signing; a signature does not authenticate the
// provenance of guest-supplied facts.
package grant

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxTokenBytes = 16384
	maxLifetime   = 30 * time.Minute
	domain        = "runway-cache-grant-v1\x00"
)

var ErrInvalid = errors.New("cache grant refused")

// Claims are host-verified facts, not a guest request schema. RepositoryID is
// GitHub's stable numeric ID. Ref order is lookup priority. WriteRef empty
// means read-only. No claim grants access to Garage directly.
type Claims struct {
	TenantID     int64    `json:"tenant_id,omitempty"`
	Version      int      `json:"version"`
	KeyID        string   `json:"key_id"`
	Audience     string   `json:"audience"`
	RegionID     string   `json:"region_id"`
	JobID        string   `json:"job_id"`
	RequestID    int64    `json:"request_id"`
	RunID        int64    `json:"run_id"`
	RunAttempt   int64    `json:"run_attempt"`
	RepositoryID int64    `json:"repository_id"`
	ReadRefs     []string `json:"read_refs"`
	WriteRef     string   `json:"write_ref"`
	IssuedAt     int64    `json:"issued_at"`
	ExpiresAt    int64    `json:"expires_at"`
}

// Clock is a UTC interval from the host's NTP-disciplined kernel clock, never a
// guest clock. The entire interval must be inside the token lifetime;
// uncertainty is not extra TTL.
type Clock struct{ Earliest, Latest time.Time }

// Valid checks the interval shape; its source must authenticate time and freshness.
func (c Clock) Valid() bool {
	return !c.Earliest.IsZero() && !c.Latest.IsZero() && !c.Latest.Before(c.Earliest) && c.Latest.Sub(c.Earliest) <= 5*time.Second
}

func KeyID(key ed25519.PublicKey) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:])
}

// sign accepts only already-authorized claims with a finite lifetime.
func sign(key ed25519.PrivateKey, claims Claims, clock Clock) (string, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])) {
		return "", ErrInvalid
	}
	if claims.KeyID != KeyID(key.Public().(ed25519.PublicKey)) || !claims.valid(clock) {
		return "", ErrInvalid
	}
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxTokenBytes/2 {
		return "", ErrInvalid
	}
	signature := ed25519.Sign(key, append([]byte(domain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verifier owns a snapshot of configured issuer keys and the regional audience.
// It has no discovery, guest-key selection, network calls or algorithm fallback.
type Verifier struct {
	keys             map[string]ed25519.PublicKey
	audience, region string
}

func NewVerifier(keys map[string]ed25519.PublicKey, audience, region string) (*Verifier, error) {
	if len(keys) == 0 || len(keys) > 32 || !validAudience(audience) || !identifier(region) {
		return nil, ErrInvalid
	}
	v := &Verifier{keys: make(map[string]ed25519.PublicKey, len(keys)), audience: audience, region: region}
	for id, key := range keys {
		if len(key) != ed25519.PublicKeySize || id != KeyID(key) {
			return nil, ErrInvalid
		}
		v.keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	return v, nil
}

// Audience returns the fixed HTTPS origin whose capabilities this verifier accepts.
func (v *Verifier) Audience() string { return v.audience }

func (v *Verifier) Verify(token string, clock Clock) (Claims, error) {
	var claims Claims
	if v == nil || len(token) > maxTokenBytes {
		return claims, ErrInvalid
	}
	p, s, ok := strings.Cut(token, ".")
	if !ok {
		return claims, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(p)
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != p {
		return claims, ErrInvalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != s {
		return claims, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil {
		return Claims{}, ErrInvalid
	}
	// One canonical encoding rejects duplicate fields, alternate casing, trailing
	// documents and ambiguous numeric forms before accepting the signature.
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(payload, canonical) {
		return Claims{}, ErrInvalid
	}
	key, found := v.keys[claims.KeyID]
	if !found || !ed25519.Verify(key, append([]byte(domain), payload...), signature) || !claims.valid(clock) || claims.Audience != v.audience || claims.RegionID != v.region {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

func (c Claims) valid(clock Clock) bool {
	if c.TenantID < 0 || c.TenantID > 4294967295 || !clock.Valid() || c.Version != 1 || len(c.KeyID) != 64 || !validAudience(c.Audience) || !identifier(c.RegionID) || !canonicalUUID(c.JobID) || c.RequestID < 0 || c.RunID <= 0 || c.RunAttempt <= 0 || c.RepositoryID <= 0 || c.IssuedAt <= 0 || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > int64(maxLifetime/time.Second) {
		return false
	}
	if clock.Earliest.Before(time.Unix(c.IssuedAt, 0)) || !clock.Latest.Before(time.Unix(c.ExpiresAt, 0)) || len(c.ReadRefs) > 8 || len(c.ReadRefs) == 0 && c.WriteRef == "" {
		return false
	}
	seen := map[string]bool{}
	for _, ref := range c.ReadRefs {
		if !validRef(ref) || seen[ref] {
			return false
		}
		seen[ref] = true
	}
	return c.WriteRef == "" || validRef(c.WriteRef)
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func identifier(s string) bool {
	return len(s) > 0 && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n\t ")
}

func validAudience(s string) bool {
	u, err := url.Parse(s)
	return err == nil && len(s) <= 1024 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.String() == s
}

func validRef(s string) bool {
	return len(s) <= 1024 && utf8.ValidString(s) && strings.HasPrefix(s, "refs/") && len(s) > len("refs/") && !strings.ContainsAny(s, "\x00\r\n\t ")
}
