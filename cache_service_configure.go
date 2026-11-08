package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/golang-jwt/jwt/v5"
)

// Each request retains its immutable client snapshot across credential rotation.
type cacheCentralRotating struct {
	current atomic.Pointer[cacheCentralWithBuckets]
}

func (c *cacheCentralRotating) lease(ctx context.Context, address netip.Addr) (cacheServiceLease, error) {
	client := c.current.Load()
	if client == nil {
		return cacheServiceLease{}, grant.ErrInvalid
	}
	return client.lease(ctx, address)
}

type cacheConfigure struct {
	store              func(string, string) error // nil selects protected production persistence
	config             cacheServiceConfig
	central            *cacheCentralRotating
	client             *http.Client
	mu                 sync.Mutex // Serializes persistence and publication, and bounds JWKS fetches.
	keys               map[string]ed25519.PublicKey
	expires, nextFetch time.Time
}

func cacheCentralClient(c cacheServiceConfig) (*http.Client, error) {
	var roots *x509.CertPool
	if c.CentralCAFile != "" {
		data, err := readProtectedCacheFile(c.CentralCAFile, false)
		if err != nil {
			return nil, err
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, grant.ErrInvalid
		}
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func (h *cacheConfigure) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	now := time.Now()
	if k := h.keys[kid]; k != nil && now.Before(h.expires) {
		return k, nil
	}
	if now.Before(h.nextFetch) {
		return nil, grant.ErrInvalid
	}
	h.nextFetch = now.Add(time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", h.config.CentralJWKSURL, nil)
	if err != nil {
		return nil, grant.ErrInvalid
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, grant.ErrInvalid
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 || resp.StatusCode != 200 {
		return nil, grant.ErrInvalid
	}
	var set struct {
		Keys []struct{ Kty, Crv, X, Kid, Alg, Use string }
	}
	if json.Unmarshal(data, &set) != nil || len(set.Keys) > 64 {
		return nil, grant.ErrInvalid
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		raw, e := base64.RawURLEncoding.DecodeString(k.X)
		if e != nil || len(raw) != 32 || k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" || (k.Alg != "" && k.Alg != "EdDSA") || (k.Use != "" && k.Use != "sig") {
			continue
		}
		if keys[k.Kid] != nil {
			return nil, grant.ErrInvalid
		}
		keys[k.Kid] = ed25519.PublicKey(raw)
	}
	h.keys = keys
	h.expires = now.Add(5 * time.Minute)
	if keys[kid] == nil {
		return nil, grant.ErrInvalid
	}
	return keys[kid], nil
}
func (h *cacheConfigure) authenticate(r *http.Request) bool {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") || len(headers[0]) > 8192 {
		return false
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(strings.TrimPrefix(headers[0], "Bearer "), claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, grant.ErrInvalid
		}
		return h.key(r.Context(), kid)
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(h.config.CentralIssuer), jwt.WithAudience("runway-cache:"+strconv.Itoa(h.config.CentralRegionID)), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return false
	}
	aud, ok := claims["aud"].(string)
	if !ok || aud != "runway-cache:"+strconv.Itoa(h.config.CentralRegionID) || claims["scope"] != "runway-cache-configure" {
		return false
	}
	iat, e1 := claims.GetIssuedAt()
	exp, e2 := claims.GetExpirationTime()
	return e1 == nil && e2 == nil && iat != nil && exp != nil && exp.After(iat.Time) && exp.Sub(iat.Time) <= 5*time.Minute && time.Since(iat.Time) <= 5*time.Minute+30*time.Second
}
func (h *cacheConfigure) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !peer.Addr().IsLoopback() {
		http.Error(w, "configure refused", 403)
		return
	}
	if r.URL.Path != "/configure" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method refused", 405)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.authenticate(r) {
		http.Error(w, "configure refused", 401)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	var body struct {
		CentralURL string `json:"central_url"`
		Region     string `json:"region"`
		Credential string `json:"credential"`
	}
	if err != nil || readCacheObject(data, []string{"central_url", "region", "credential"}) != nil || json.Unmarshal(data, &body) != nil || body.CentralURL != h.config.CentralURL || body.Region != h.config.RegionID || body.Credential == "" || strings.ContainsAny(body.Credential, " \t\r\n") {
		http.Error(w, "configure refused", 400)
		return
	}
	persist := h.store
	if persist == nil {
		persist = storeCacheCredential
	}
	if err = persist(h.config.CentralTokenFile, body.Credential); err != nil {
		http.Error(w, "configure unavailable", 503)
		return
	}
	h.central.current.Store(&cacheCentralWithBuckets{cacheCentralHTTPS{origin: h.config.CentralURL, token: body.Credential, client: h.client}})
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"configured": true}`)
}
func storeCacheCredential(path, token string) error {
	if checkCachePrivateDirectory(filepath.Dir(path)) != nil {
		return grant.ErrInvalid
	}
	return writeCacheCredential(path, token)
}

func writeCacheCredential(path, token string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".central-token-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.WriteString(token); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
