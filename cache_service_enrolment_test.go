package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCacheCredentialLifecycle(t *testing.T) {
	var calls atomic.Int32
	var current atomic.Pointer[string]
	var dropReply atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		candidate, _ := body["credential"].(string)
		if !validCacheCredential(candidate) {
			t.Error("invalid credential candidate")
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/api/method/runway.api.cache_enrol":
			if calls.Add(1) != 1 || r.Header.Get("Authorization") != "" || body["token"] != "one-use" ||
				body["address"] != "fdaa:1::7" || body["port"] != float64(8443) || body["region"] != "blr-1" || body["ca"] != "public-test-ca" {
				t.Error("invalid enrolment request")
				w.WriteHeader(400)
				return
			}
		case "/api/method/runway.api.cache_rotate":
			if token := current.Load(); token == nil || r.Header.Get("Authorization") != "token "+*token {
				w.WriteHeader(401)
				return
			}
		default:
			w.WriteHeader(404)
			return
		}
		current.Store(&candidate)
		if dropReply.Swap(false) {
			w.WriteHeader(502)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"credential": candidate})
	}))
	defer server.Close()
	dir := t.TempDir()
	c := cacheServiceConfig{
		RegionID: "blr-1", Listen: "[fdaa:1::7]:8443", CentralURL: server.URL,
		CacheDataCAFile: filepath.Join(dir, "ca"), CentralTokenFile: filepath.Join(dir, "credential"),
	}
	if err := os.WriteFile(c.CacheDataCAFile, []byte("public-test-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(rotate bool) error {
		return exchangeCacheCredential(t.Context(), c, server.Client(), rotate, func() (string, error) { return "one-use", nil })
	}
	checkLocal := func() string {
		t.Helper()
		raw, err := os.ReadFile(c.CentralTokenFile)
		if err != nil || current.Load() == nil || string(raw) != *current.Load() {
			t.Fatal("accepted credential was not persisted")
		}
		return string(raw)
	}
	dropReply.Store(true)
	if err := run(false); err == nil {
		t.Fatal("expected interrupted enrolment")
	}
	if err := run(false); err != nil {
		t.Fatal(err)
	}
	first := checkLocal()
	dropReply.Store(true)
	if err := run(true); err == nil {
		t.Fatal("expected interrupted rotation")
	}
	if err := run(true); err != nil {
		t.Fatal(err)
	}
	if checkLocal() == first {
		t.Fatal("rotation reused previous credential")
	}
	current.Store(nil)
	if err := run(true); !errors.Is(err, errCacheCredentialRejected) {
		t.Fatal("revoked credential was accepted", err)
	}
	if calls.Load() != 1 {
		t.Fatal("rotation attempted to enrol")
	}
}

func TestCacheServiceHasNoConfigureRoute(t *testing.T) {
	s := &cacheService{}
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, "/configure", nil)
		r.RemoteAddr = "[fdaa:1::7]:443"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d", method, w.Code)
		}
	}
}
