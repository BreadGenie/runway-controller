package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
)

const servicePeer = "[fdaa:1:0:2::7]:1234"

func serviceFixture(t *testing.T, edit func(*cacheServiceLease), status int, stall bool) (*cacheService, *authorityCase) {
	t.Helper()
	test := newAuthorityCase(t)
	gh, _ := test.authority(t)
	lease := cacheServiceLease{Lease: "lease-1", Team: "team", TenantID: 2, Region: "blr-1", Repository: fixtureRepository, VMID: "vm-7", MeshAddress: "fdaa:1:0:2::7", RunnerID: 219, RunnerName: test.binding.RunnerName, Deadline: time.Now().Add(time.Hour), Job: &cacheServiceJob{JobID: test.release.JobID, RunID: 41867135947, RunAttempt: 1, DisplayName: "Integration (mariadb, 1)", WorkflowRef: fixtureWorkflow, EventName: "workflow_dispatch"}}
	if edit != nil {
		edit(&lease)
	}
	central := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token region-secret" || r.URL.Path != "/api/method/central.runway.cache_lease" || r.URL.Query().Get("address") != "fdaa:1:0:2::7" {
			t.Error("Central request contract", r.URL)
			w.WriteHeader(400)
			return
		}
		if stall {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(lease)
	}))
	t.Cleanup(central.Close)
	data := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing grant")
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(data.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	s := &cacheService{ctx: ctx, config: cacheServiceConfig{MeshRegionID: 1, RegionID: "blr-1"}, central: cacheCentralHTTPS{origin: central.URL, token: "region-secret", client: central.Client()}, github: gh.client, sessions: map[string]*cacheHost{}, slots: make(chan struct{}, 64), template: cacheHost{key: key, roots: data.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, audience: data.URL, archiveBytes: 1 << 20, log: func(map[string]any) {}, clock: func(context.Context) (grant.Clock, error) {
		now := time.Now()
		return grant.Clock{Earliest: now, Latest: now}, nil
	}}}
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, h := range s.sessions {
			h.mu.Lock()
			if h.current != nil {
				h.current.Close()
			}
			h.mu.Unlock()
		}
	})
	return s, test
}
func serviceSetup(t *testing.T, s *cacheService, request grant.SetupRequest, peer string) string {
	t.Helper()
	data, _ := json.Marshal(request)
	r := httptest.NewRequest("POST", "https://cache/setup", bytes.NewReader(append(data, '\n')))
	r.RemoteAddr = peer
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		return http.StatusText(w.Code)
	}
	var result struct{ Outcome string }
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Body.String())
	}
	return result.Outcome
}
func TestCacheServiceAttributionAndFallback(t *testing.T) {
	for _, peer := range []string{"127.0.0.1:7", "[::ffff:127.0.0.1]:7", "[2001:db8::1]:7", "[fdaa:2:0:2::7]:7", "[fdaa:1::7]:7"} {
		t.Run(peer, func(t *testing.T) {
			s, test := serviceFixture(t, nil, 200, false)
			if got := serviceSetup(t, s, test.setup, peer); got != "Forbidden" {
				t.Fatal(got)
			}
		})
	}
	for _, status := range []int{404, 409, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, test := serviceFixture(t, nil, status, false)
			if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
				t.Fatal(got)
			}
		})
	}
	for _, edit := range []func(*cacheServiceLease){func(l *cacheServiceLease) { l.TenantID = 3 }, func(l *cacheServiceLease) { l.MeshAddress = "fdaa:1:0:2::8" }, func(l *cacheServiceLease) { l.Job = nil }, func(l *cacheServiceLease) { l.Region = "other" }} {
		s, test := serviceFixture(t, edit, 200, false)
		if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
			t.Fatal(got)
		}
	}
}

// The controller's scale-set messages carry no run attempt: REST supplies it.
func TestCacheServiceUnknownRunAttempt(t *testing.T) {
	s, test := serviceFixture(t, func(l *cacheServiceLease) { l.Job.RunAttempt = 0 }, 200, false)
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "enabled" {
		t.Fatal("unknown attempt not resolved by REST", got)
	}
	s, test = serviceFixture(t, func(l *cacheServiceLease) { l.Job.RunAttempt = 2 }, 200, false)
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
		t.Fatal("known attempt disagreeing with REST enabled", got)
	}
}

func TestCacheServiceGrantTranslatorReplay(t *testing.T) {
	s, test := serviceFixture(t, nil, 200, false)
	bad := test.setup
	wrong := "refs/heads/wrong"
	bad.Context.Ref = &wrong
	if got := serviceSetup(t, s, bad, servicePeer); got != "disabled" {
		t.Fatal(got)
	}
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
		t.Fatal("replayed nonce", got)
	}
	test.setup.Nonce = "8c0f8e4a-2f7d-4e1b-8a3c-5d6e7f8a9b0c"
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "enabled" {
		t.Fatal(got)
	}
	test.setup.Nonce = "7c0f8e4a-2f7d-4e1b-8a3c-5d6e7f8a9b0c"
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
		t.Fatal("second enabled", got)
	}
	r := httptest.NewRequest("GET", "/_apis/artifactcache/cache?keys=x&version=v", nil)
	r.RemoteAddr = servicePeer
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestCacheServiceCentralBudget(t *testing.T) {
	s, test := serviceFixture(t, nil, 200, true)
	start := time.Now()
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "disabled" {
		t.Fatal(got)
	}
	if elapsed := time.Since(start); elapsed > cacheHostSetupBudget+time.Second {
		t.Fatal(elapsed)
	}
}
func TestCacheCentralExactContract(t *testing.T) {
	for _, body := range []string{`{}`, `{"lease":"x","lease":"y"}`, `{"unknown":1}`, `null`} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		client := cacheCentralHTTPS{origin: server.URL, token: "x", client: server.Client()}
		if _, err := client.lease(t.Context(), netip.MustParseAddr("fdaa:1:0:2::7")); err == nil {
			t.Fatal(body)
		}
		server.Close()
	}
}

func TestCacheCentralRejectsContractMutations(t *testing.T) {
	s, _ := serviceFixture(t, nil, 200, false)
	original, err := s.central.lease(t.Context(), netip.MustParseAddr("fdaa:1:0:2::7"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(original)
	for name, mutate := range map[string]func(string) string{
		"unknown":        func(v string) string { return strings.Replace(v, `"team":`, `"extra":1,"team":`, 1) },
		"missing":        func(v string) string { return strings.Replace(v, `"tenant_id":2,`, "", 1) },
		"duplicate":      func(v string) string { return strings.Replace(v, `"tenant_id":2`, `"tenant_id":3,"tenant_id":2`, 1) },
		"nested unknown": func(v string) string { return strings.Replace(v, `"started":false`, `"started":false,"extra":1`, 1) },
		"nested missing": func(v string) string { return strings.Replace(v, `,"started":false`, "", 1) },
		"nested duplicate": func(v string) string {
			return strings.Replace(v, `"started":false`, `"started":true,"started":false`, 1)
		},
		"null required":  func(v string) string { return strings.Replace(v, `"tenant_id":2`, `"tenant_id":null`, 1) },
		"extra document": func(v string) string { return v + `{}` },
	} {
		t.Run(name, func(t *testing.T) {
			body := mutate(string(data))
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client := cacheCentralHTTPS{origin: server.URL, token: "x", client: server.Client()}
			if _, err := client.lease(t.Context(), netip.MustParseAddr("fdaa:1:0:2::7")); err == nil {
				t.Fatal("accepted", body)
			}
		})
	}
}

func TestCacheServiceConfig(t *testing.T) {
	c := cacheServiceConfig{Version: 1, RegionID: "blr-1", MeshRegionID: 1, Listen: "[fdaa:1::10]:8443", Audience: "https://cache.example", CertificateFile: "/etc/cache.crt", PrivateKeyFile: "/etc/cache.key", SigningKeyFile: "/var/lib/cache/signing.key", CacheDataCAFile: "/etc/cache-ca.crt", GitHubTokenFile: "/etc/github", CentralURL: "https://central.example", CentralTokenFile: "/etc/central", ArchiveBytes: 1 << 30}
	data, _ := json.Marshal(c)
	if _, err := decodeCacheServiceConfig(data); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*cacheServiceConfig){func(c *cacheServiceConfig) { c.Listen = "127.0.0.1:8443" }, func(c *cacheServiceConfig) { c.Listen = "[fdaa:2::10]:8443" }, func(c *cacheServiceConfig) { c.CentralURL = "http://central.example" }, func(c *cacheServiceConfig) { c.PrivateKeyFile = "relative" }, func(c *cacheServiceConfig) { c.ArchiveBytes = 0 }} {
		other := c
		edit(&other)
		data, _ := json.Marshal(other)
		if _, err := decodeCacheServiceConfig(data); err == nil {
			t.Fatal(other)
		}
	}
	data, _ = json.Marshal(c)
	for _, bad := range []string{strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(data), `"version":1`, `"version":1,"extra":1`, 1)} {
		if _, err := decodeCacheServiceConfig([]byte(bad)); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestCacheServiceRenewalRechecksGitHub(t *testing.T) {
	s, test := serviceFixture(t, nil, 200, false)
	if got := serviceSetup(t, s, test.setup, servicePeer); got != "enabled" {
		t.Fatal(got)
	}
	host := s.sessions["fdaa:1:0:2::7"]
	host.mu.Lock()
	release := *host.release
	confirmation := *release.Confirmed
	host.mu.Unlock()
	confirmation.ConfirmedAt = time.Now().Add(-cacheConfirmationFresh - time.Second)
	release.Confirmed = &confirmation
	a := host.authority.(*cacheServiceAuthority)
	github := s.github.Transport.(*githubFixture)
	github.mu.Lock()
	for path, bodies := range github.responses {
		if strings.Contains(path, "/actions/jobs/") {
			github.responses[path] = [][]byte{bytes.ReplaceAll(bodies[0], []byte(`"status":"in_progress"`), []byte(`"status":"completed"`))}
		}
	}
	github.mu.Unlock()
	binding, _ := host.authenticateService()
	if _, err := a.authorize(t.Context(), binding, release, test.setup); err == nil {
		t.Fatal("completed job renewed")
	}
}
