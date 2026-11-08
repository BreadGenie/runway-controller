package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func configureFixture(t *testing.T) (*cacheConfigure, ed25519.PrivateKey, *atomic.Int32) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	calls := new(atomic.Int32)
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "key", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	t.Cleanup(jwks.Close)
	h := &cacheConfigure{config: cacheServiceConfig{CentralURL: jwks.URL, CentralJWKSURL: jwks.URL, CentralIssuer: "central", CentralRegionID: 1, RegionID: "blr-1", CentralTokenFile: filepath.Join(t.TempDir(), "token")}, central: &cacheCentralRotating{}, client: jwks.Client(), store: writeCacheCredential}
	return h, key, calls
}
func configureToken(t *testing.T, key ed25519.PrivateKey, kid string, edit func(jwt.MapClaims)) string {
	t.Helper()
	now := time.Now().Unix()
	claims := jwt.MapClaims{"iss": "central", "aud": "runway-cache:1", "scope": "runway-cache-configure", "iat": now, "exp": now + 300}
	if edit != nil {
		edit(claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = kid
	value, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func configureRequest(h *cacheConfigure, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://operator/configure", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:9000"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Set("X-Cargo-Access-Token", "ignored")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func configureBody(h *cacheConfigure, credential string) string {
	data, _ := json.Marshal(map[string]string{"central_url": h.config.CentralURL, "region": "blr-1", "credential": credential})
	return string(data)
}
func TestCacheConfigureAuthentication(t *testing.T) {
	for _, name := range []string{"signature", "kid", "issuer", "audience", "scope", "expired", "future", "cargo", "cargo-array", "missing-iat", "long-ttl", "header-only"} {
		t.Run(name, func(t *testing.T) {
			h, key, calls := configureFixture(t)
			kid := "key"
			edit := func(c jwt.MapClaims) {
				switch name {
				case "issuer":
					c["iss"] = "other"
				case "audience":
					c["aud"] = "runway-cache:2"
				case "scope":
					c["scope"] = "other"
				case "expired":
					c["iat"] = time.Now().Unix() - 600
					c["exp"] = time.Now().Unix() - 60
				case "future":
					c["iat"] = time.Now().Unix() + 60
				case "cargo":
					c["aud"] = "atlas-cargo:1"
				case "cargo-array":
					c["aud"] = []string{"runway-cache:1", "atlas-cargo:1"}
				case "missing-iat":
					delete(c, "iat")
				case "long-ttl":
					c["exp"] = time.Now().Unix() + 600
				}
			}
			if name == "signature" {
				_, key, _ = ed25519.GenerateKey(rand.Reader)
			}
			if name == "kid" { // Warm cache, then force one bounded unknown-kid refetch.
				if w := configureRequest(h, configureToken(t, key, kid, nil), configureBody(h, "first")); w.Code != 200 {
					t.Fatal(w.Code)
				}
				h.nextFetch = time.Time{}
				kid = "unknown"
			}
			token := configureToken(t, key, kid, edit)
			if name == "header-only" {
				token = ""
			}
			if w := configureRequest(h, token, configureBody(h, "secret")); w.Code != 401 {
				t.Fatal(w.Code, w.Body.String())
			}
			if name == "kid" {
				if calls.Load() != 2 {
					t.Fatal("unknown kid did not refetch")
				}
				configureRequest(h, token, configureBody(h, "secret"))
				if calls.Load() != 2 {
					t.Fatal("fetch not rate limited")
				}
			}
		})
	}
}
func TestCacheConfigureBodyAndListeners(t *testing.T) {
	h, key, _ := configureFixture(t)
	token := configureToken(t, key, "key", nil)
	for _, body := range []string{`{}`, `{"central_url":"wrong","region":"blr-1","credential":"secret"}`, strings.Replace(configureBody(h, "secret"), "blr-1", "other", 1), strings.TrimSuffix(configureBody(h, "secret"), "}") + `,"extra":1}`, configureBody(h, ""), `{"central_url":null,"region":"blr-1","credential":"secret"}`, strings.TrimSuffix(configureBody(h, "secret"), "}") + `,"region":"blr-1"}`, strings.Replace(configureBody(h, "secret"), `"credential":"secret",`, "", 1)} {
		if w := configureRequest(h, token, body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	s, _ := serviceFixture(t, nil, 200, false)
	r := httptest.NewRequest("POST", "https://guest/configure", nil)
	r.RemoteAddr = servicePeer
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	r.RemoteAddr = servicePeer
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("mesh peer accepted")
	}
}
func TestCacheConfigureAtomicRotation(t *testing.T) {
	h, key, _ := configureFixture(t)
	token := configureToken(t, key, "key", nil)
	for _, value := range []string{"first", "second"} {
		w := configureRequest(h, token, configureBody(h, value))
		if w.Code != 200 || w.Body.String() != `{"configured": true}` {
			t.Fatal(w.Code, w.Body.String())
		}
		raw, err := os.ReadFile(h.config.CentralTokenFile)
		if err != nil || string(raw) != value || h.central.current.Load().token != value {
			t.Fatal("credential not published")
		}
		info, _ := os.Stat(h.config.CentralTokenFile)
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	// A request that took its old client snapshot completes while rotation publishes.
	entered, release := make(chan struct{}), make(chan struct{})
	central := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token second" {
			t.Error("in-flight token changed")
		}
		close(entered)
		<-release
		w.WriteHeader(404)
	}))
	defer central.Close()
	h.central.current.Store(&cacheCentralWithBuckets{cacheCentralHTTPS{origin: central.URL, token: "second", client: central.Client()}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.central.lease(context.Background(), netip.MustParseAddr("fdaa:1:0:2::7"))
	}()
	<-entered
	if w := configureRequest(h, token, configureBody(h, "third")); w.Code != 200 {
		t.Fatal(w.Code)
	}
	close(release)
	<-done
	// Persistence failure must keep the previous client.
	h.config.CentralTokenFile = filepath.Join(t.TempDir(), "absent", "token")
	if w := configureRequest(h, token, configureBody(h, "fourth")); w.Code != 503 || h.central.current.Load().token != "third" {
		t.Fatal("failed persistence published")
	}
}
func TestCacheServiceUnenrolled(t *testing.T) {
	s, test := serviceFixture(t, nil, 200, false)
	s.central = &cacheCentralRotating{}
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
		t.Fatal(got)
	}
}

func TestCacheConfigureJWKSCache(t *testing.T) {
	h, key, calls := configureFixture(t)
	token := configureToken(t, key, "key", nil)
	for range 2 {
		if w := configureRequest(h, token, configureBody(h, "secret")); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("JWKS not cached")
	}
	h.expires = time.Now().Add(-time.Second)
	h.nextFetch = time.Time{}
	if w := configureRequest(h, token, configureBody(h, "rotated")); w.Code != 200 || calls.Load() != 2 {
		t.Fatal("expired JWKS not refreshed", w.Code, calls.Load())
	}
}
func TestCacheCredentialProtectedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if os.Geteuid() != 0 {
		if storeCacheCredential(path, "secret") == nil {
			t.Fatal("non-root directory accepted")
		}
		return
	}
	if err := storeCacheCredential(path, "secret"); err != nil {
		t.Fatal(err)
	}
	raw, err := readProtectedCacheFile(path, true)
	if err != nil || string(raw) != "secret" {
		t.Fatal("protected credential unreadable")
	}
}
