package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCentralRPCSendsFencingAndKeepsSecretsOutOfErrors(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "token test-key:test-secret" || r.Method != "POST" {
			t.Error("missing API authentication")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		if body["pool"] != "pool" || body["owner_id"] != "owner" {
			t.Error("missing ownership identity")
		}
		if strings.HasSuffix(r.URL.Path, "claim_pool") {
			json.NewEncoder(w).Encode(map[string]any{"message": pilotSnapshot{Fence: 17, Pool: pilotPool{ID: "pool"}}})
			return
		}
		if body["fence"] != float64(17) {
			t.Error("missing current fencing token")
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"exception":"test-secret should never be logged"}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-key:test-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newCentralRPC(controllerConfig{CentralURL: server.URL, CredentialFile: path, Pool: "pool", OwnerID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = client.Update(context.Background(), "lease", "jit_requested", leaseEvent{})
	if err == nil || strings.Contains(err.Error(), "test-secret") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("bad sanitized error: %v", err)
	}
	if requests != 2 {
		t.Fatal("RPC mutations must not automatically retry")
	}
}

func TestControllerConfigRequiresHTTPSExceptLiteralLoopback(t *testing.T) {
	cases := []struct {
		name    string
		central string
		allow   bool
	}{
		{"https host", "https://central.example.com", true},
		{"https port and path", "https://central.example.com:8443/runway", true},
		{"https literal ip", "https://10.0.0.7:8443", true},
		{"https userinfo rejected", "https://key:secret@central.example.com", false},
		{"localhost", "http://localhost:8080", true},
		{"localhost upper case", "HTTP://LOCALHOST:8080", true},
		{"loopback ipv4", "http://127.0.0.1:8080", true},
		{"loopback ipv6", "http://[::1]:8080", true},
		{"http external host", "http://central.example.com", false},
		{"http external host with port", "http://central.example.com:8080", false},
		{"http private lan address", "http://10.0.0.7:8080", false},
		{"http deceptive suffix", "http://localhost.example.com", false},
		{"http deceptive ip suffix", "http://127.0.0.1.example.com", false},
		{"http deceptive prefix", "http://notlocalhost", false},
		{"http trailing dot", "http://localhost.:8080", false},
		{"http other loopback spelling", "http://127.0.0.2:8080", false},
		{"http zone scoped loopback", "http://[::1%25lo]:8080", false},
		{"http userinfo before loopback", "http://central.example.com@localhost:8080", false},
		{"http userinfo remote", "http://key:secret@central.example.com", false},
		{"http unspecified ipv4", "http://0.0.0.0:8080", false},
		{"https unspecified ipv4", "https://0.0.0.0:8443", false},
		{"https unspecified ipv6", "https://[::]:8443", false},
		{"http unspecified ipv4 mapped", "http://[::ffff:0.0.0.0]:8080", false},
		{"query rejected", "https://central.example.com?token=1", false},
		{"empty query rejected", "https://central.example.com?", false},
		{"fragment rejected", "https://central.example.com#frag", false},
		{"malformed rejected", "://central.example.com", false},
		{"scheme missing rejected", "central.example.com", false},
		{"host missing rejected", "https:///runway", false},
		{"unsupported scheme rejected", "ftp://central.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.json")
			config, err := json.Marshal(map[string]any{
				"central_url":             tc.central,
				"central_credential_file": "/srv/runway/private/central.token",
				"pool":                    "pool",
				"transport_command":       []string{"python3", "/srv/runway/scripts/metal-guest-transport.py"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, config, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = loadControllerConfig(path)
			if tc.allow && err != nil {
				t.Fatalf("refused %s: %v", tc.central, err)
			}
			if !tc.allow && err == nil {
				t.Fatalf("accepted %s", tc.central)
			}
		})
	}
}

func TestCentralRPCRefusesPlaintextRemoteEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-key:test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []string{"https://central.example.com", "http://127.0.0.1:8080"} {
		if _, err := newCentralRPC(controllerConfig{CentralURL: allowed, CredentialFile: path, Pool: "pool"}); err != nil {
			t.Fatalf("refused %s: %v", allowed, err)
		}
	}
	for _, refused := range []string{"http://central.example.com", "http://localhost.example.com", "https://0.0.0.0:8443"} {
		if _, err := newCentralRPC(controllerConfig{CentralURL: refused, CredentialFile: path, Pool: "pool"}); err == nil {
			t.Fatalf("accepted %s; the pool token would leave the process unencrypted", refused)
		}
	}
}

func TestCentralRPCDoesNotForwardTokenAcrossRedirects(t *testing.T) {
	leaked := make(chan string, 1)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case leaked <- r.Header.Get("Authorization"):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound)
	}))
	defer redirector.Close()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-key:test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newCentralRPC(controllerConfig{CentralURL: redirector.URL, CredentialFile: path, Pool: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background()); err == nil {
		t.Fatal("followed a redirect from Central")
	}
	select {
	case header := <-leaked:
		t.Fatalf("Central token reached a redirect target: %q", header)
	default:
	}
}

func TestCentralCredentialRejectsReadableFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("key:secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newCentralRPC(controllerConfig{CentralURL: "https://central.example.com", CredentialFile: path}); err == nil {
		t.Fatal("accepted world-readable credential")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newCentralRPC(controllerConfig{CentralURL: "https://central.example.com", CredentialFile: link}); err == nil {
		t.Fatal("accepted credential symlink")
	}
}

func TestCentralRenewRejectsChangedPoolIdentity(t *testing.T) {
	original := pilotSnapshot{Fence: 1, Pool: pilotPool{ID: "pool", RepositoryURL: "https://github.com/test/repo", ScaleSetID: 7, MaxConcurrency: 1, Region: "region", Team: "team"}}
	mutations := map[string]func(*pilotSnapshot){
		"repository": func(s *pilotSnapshot) { s.Pool.RepositoryURL = "https://github.com/other/repo" },
		"scale set":  func(s *pilotSnapshot) { s.Pool.ScaleSetID = 8 },
		"capacity":   func(s *pilotSnapshot) { s.Pool.MaxConcurrency = 2 },
		"region":     func(s *pilotSnapshot) { s.Pool.Region = "other" },
		"team":       func(s *pilotSnapshot) { s.Pool.Team = "other" },
		"fence":      func(s *pilotSnapshot) { s.Fence = 2 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := original
			mutate(&changed)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"message": changed})
			}))
			defer server.Close()
			client := &centralRPC{baseURL: server.URL, pool: "pool", owner: "owner", fence: 1, claimedPool: original.Pool, http: server.Client()}
			if _, err := client.Renew(context.Background(), ""); err == nil {
				t.Fatal("renew accepted identity incompatible with existing GitHub session/transport")
			}
		})
	}
}
