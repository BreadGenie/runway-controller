package grant

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixture(t *testing.T) (ed25519.PrivateKey, Claims, Clock, *Verifier) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	clock := Clock{Earliest: now, Latest: now.Add(time.Second)}
	c := Claims{Version: 1, KeyID: KeyID(public), Audience: "https://cache.example.test", RegionID: "blr", JobID: "f9941b7c-2575-4d5b-81b7-d699446bbff3", RequestID: 0, RunID: 123, RunAttempt: 1, RepositoryID: 456, ReadRefs: []string{"refs/heads/feature", "refs/heads/main"}, WriteRef: "refs/heads/feature", IssuedAt: now.Unix(), ExpiresAt: now.Add(20 * time.Minute).Unix()}
	v, err := NewVerifier(map[string]ed25519.PublicKey{KeyID(public): public}, c.Audience, c.RegionID)
	if err != nil {
		t.Fatal(err)
	}
	return private, c, clock, v
}

func TestRoundTripAndReadOnly(t *testing.T) {
	key, c, clock, v := fixture(t)
	for _, write := range []string{c.WriteRef, ""} {
		c.WriteRef = write
		token, err := sign(key, c, clock)
		if err != nil {
			t.Fatal(err)
		}
		got, err := v.Verify(token, clock)
		if err != nil || got.RepositoryID != c.RepositoryID || got.WriteRef != write || strings.Join(got.ReadRefs, ",") != strings.Join(c.ReadRefs, ",") {
			t.Fatal("scope changed", err)
		}
	}
}

func TestRejectTamperedUnknownIssuerAndWrongAudience(t *testing.T) {
	key, c, clock, v := fixture(t)
	token, _ := sign(key, c, clock)
	for _, change := range []func(*Claims){
		func(c *Claims) { c.RepositoryID++ },
		func(c *Claims) { c.Audience = "https://other.example.test" },
		func(c *Claims) { c.RegionID = "other" },
		func(c *Claims) { c.KeyID = strings.Repeat("a", 64) },
	} {
		mutated := c
		change(&mutated)
		payload := mustJSON(t, mutated)
		_, signature, _ := strings.Cut(token, ".")
		bad := base64.RawURLEncoding.EncodeToString(payload) + "." + signature
		if _, err := v.Verify(bad, clock); err == nil {
			t.Fatal("tampered token accepted")
		}
	}
	for _, field := range []string{"audience", "region"} {
		changed := c
		if field == "audience" {
			changed.Audience = "https://other.example.test"
		} else {
			changed.RegionID = "other"
		}
		signed, err := sign(key, changed, clock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(signed, clock); err == nil {
			t.Fatal("valid signature with wrong destination accepted")
		}
	}
}

func TestTimeBoundsNeverAddUncertaintyToTTL(t *testing.T) {
	key, c, clock, v := fixture(t)
	token, _ := sign(key, c, clock)
	for _, bad := range []Clock{
		{},
		{Earliest: clock.Earliest, Latest: clock.Earliest.Add(6 * time.Second)},
		{Earliest: clock.Earliest.Add(-time.Second), Latest: clock.Latest},
		{Earliest: time.Unix(c.ExpiresAt-1, 0), Latest: time.Unix(c.ExpiresAt, 0)},
		{Earliest: clock.Latest, Latest: clock.Earliest},
	} {
		if _, err := v.Verify(token, bad); err == nil {
			t.Fatal("invalid clock interval accepted")
		}
	}
	c.ExpiresAt = c.IssuedAt + 1801
	if _, err := sign(key, c, clock); err == nil {
		t.Fatal("excess lifetime accepted")
	}
}

func TestRejectAmbiguousSignedPayloads(t *testing.T) {
	key, c, clock, v := fixture(t)
	canonical := string(mustJSON(t, c))
	for _, payload := range []string{
		strings.Replace(canonical, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(canonical, `"version":1`, `"Version":1`, 1),
		strings.Replace(canonical, `"version":1`, `"version":1,"admin":true`, 1),
		canonical + "{}", canonical + " ",
	} {
		sig := ed25519.Sign(key, append([]byte(domain), []byte(payload)...))
		token := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
		if _, err := v.Verify(token, clock); err == nil {
			t.Fatal("ambiguous signed payload accepted")
		}
	}
	if _, err := v.Verify(strings.Repeat("x", maxTokenBytes+1), clock); err == nil {
		t.Fatal("unbounded token accepted")
	}
}

func TestInvalidScopeAndKeyCustody(t *testing.T) {
	key, c, clock, _ := fixture(t)
	for _, change := range []func(*Claims){
		func(c *Claims) { c.ReadRefs = []string{"refs/heads/main", "refs/heads/main"} },
		func(c *Claims) { c.ReadRefs = nil; c.WriteRef = "" },
		func(c *Claims) { c.ReadRefs = []string{"main"} },
		func(c *Claims) { c.RepositoryID = 0 },
		func(c *Claims) { c.RequestID = -1 },
		func(c *Claims) { c.JobID = "not-a-job" },
		func(c *Claims) { c.Audience = "http://cache.example.test" },
	} {
		bad := c
		change(&bad)
		if _, err := sign(key, bad, clock); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
	public := append(ed25519.PublicKey(nil), key.Public().(ed25519.PublicKey)...)
	keys := map[string]ed25519.PublicKey{c.KeyID: public}
	v, err := NewVerifier(keys, c.Audience, c.RegionID)
	if err != nil {
		t.Fatal(err)
	}
	public[0] ^= 1
	delete(keys, c.KeyID)
	token, _ := sign(key, c, clock)
	if _, err := v.Verify(token, clock); err != nil {
		t.Fatal("caller mutated verifier keys")
	}
	key[63] ^= 1
	if _, err := sign(key, c, clock); err == nil {
		t.Fatal("inconsistent private key accepted")
	}
}
