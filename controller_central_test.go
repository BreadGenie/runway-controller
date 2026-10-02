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

func TestCentralCredentialRejectsReadableFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("key:secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newCentralRPC(controllerConfig{CredentialFile: path}); err == nil {
		t.Fatal("accepted world-readable credential")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newCentralRPC(controllerConfig{CredentialFile: link}); err == nil {
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
