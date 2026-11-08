package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvironment(t *testing.T) {
	env := []string{"A=1", "ACTIONS_CACHE_URL=upstream", "ACTIONS_CACHE_SERVICE_V2=true", "ACTIONS_RUNTIME_TOKEN=secret", "ACTIONS_RESULTS_URL=artifact"}
	for _, script := range []string{"/missing/unrelated.js", "/missing/_actions/actions/cache/v4/dist/restore/index.js"} {
		got := environment([]string{"node", script}, env, func() bool { t.Fatal("unexpected setup"); return true }, func(string) {})
		if !reflect.DeepEqual(got, env) {
			t.Fatal(got)
		}
	}
	dir := filepath.Join(t.TempDir(), "_actions/actions/cache/v4/dist/restore")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "index.js")
	if err := os.WriteFile(script, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := environment([]string{"node", script}, env, func() bool { t.Fatal("hash mismatch reached setup"); return true }, func(string) {}); !reflect.DeepEqual(got, env) {
		t.Fatal(got)
	}
	hash := sha256.Sum256([]byte("fixture"))
	key := hex.EncodeToString(hash[:])
	allowlisted("cache", key)
	bundles["cache"][key] = true
	defer delete(bundles["cache"], key)
	if got := environment([]string{"node", script}, env, func() bool { return false }, func(string) {}); !reflect.DeepEqual(got, env) {
		t.Fatal(got)
	}
	got := environment([]string{"node", script}, env, func() bool { return true }, func(string) {})
	want := []string{"A=1", "ACTIONS_RUNTIME_TOKEN=secret", "ACTIONS_RESULTS_URL=artifact", "ACTIONS_CACHE_URL=" + route}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestUnpinnedBundleIsReportedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "_actions/actions/setup-node/v6/dist/setup")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "index.js")
	if err := os.WriteFile(script, []byte("moved tag"), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("moved tag"))
	var notices []string
	notify := func(line string) { notices = append(notices, line) }
	environment([]string{"node", script}, nil, func() bool { t.Fatal("unpinned bundle reached setup"); return true }, notify)
	if len(notices) != 1 || !strings.Contains(notices[0], "actions/setup-node") || !strings.Contains(notices[0], "sha256:"+hex.EncodeToString(hash[:])[:12]) || !strings.Contains(notices[0], "GitHub's cache") {
		t.Fatal(notices)
	}
	// Unrelated actions and pinned bundles say nothing.
	notices = nil
	environment([]string{"node", "/missing/unrelated.js"}, nil, func() bool { return false }, notify)
	allowlisted("setup-node", "")
	bundles["setup-node"][hex.EncodeToString(hash[:])] = true
	defer delete(bundles["setup-node"], hex.EncodeToString(hash[:]))
	environment([]string{"node", script}, nil, func() bool { return false }, notify)
	if len(notices) != 0 {
		t.Fatal(notices)
	}
	if line := unpinned("cache", "/missing/index.js", ""); !strings.Contains(line, "unreadable") {
		t.Fatal(line)
	}
}

func TestFastPathDoesNotOpenScript(t *testing.T) {
	// An open of this FIFO would block forever.
	path := filepath.Join(t.TempDir(), "unrelated.js")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		environment([]string{"node", path}, nil, func() bool { panic("setup") }, func(string) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fast path touched filesystem")
	}
}

func TestExchange(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled", "wrong nonce", "wrong version", "extra", "duplicate", "malformed", "oversize", "unframed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			host, guest := net.Pipe()
			defer guest.Close()
			go func() {
				defer host.Close()
				host.SetDeadline(time.Now().Add(time.Second))
				var request struct {
					Version int
					Nonce   string
					Context map[string]string
				}
				line, _ := bufio.NewReader(host).ReadBytes('\n')
				json.Unmarshal(line, &request)
				if request.Version != 2 || request.Context["repository"] != "owner/repo" {
					return
				}
				response := map[string]any{"version": 2, "nonce": request.Nonce, "outcome": "enabled", "route": route}
				switch mode {
				case "disabled":
					response["outcome"] = "disabled"
					response["route"] = nil
				case "wrong nonce":
					response["nonce"] = "other"
				case "wrong version":
					response["version"] = 1
				case "extra":
					response["job_id"] = "other"
				case "duplicate":
					fmt.Fprintf(host, "{\"version\":2,\"version\":2,\"nonce\":%q,\"outcome\":\"enabled\",\"route\":%q}\n", request.Nonce, route)
					return
				case "malformed":
					fmt.Fprint(host, "{\n")
					return
				case "oversize":
					fmt.Fprint(host, strings.Repeat("x", 4097)+"\n")
					return
				case "unframed":
					fmt.Fprint(host, "{}")
					return
				case "timeout":
					var b [1]byte
					host.Read(b[:])
					return
				}
				json.NewEncoder(host).Encode(response)
			}()
			started := time.Now()
			enabled := exchange(guest, started.Add(40*time.Millisecond), func(key string) string {
				if key == "GITHUB_REPOSITORY" {
					return "owner/repo"
				}
				return ""
			})
			if enabled != (mode == "enabled") {
				t.Fatalf("enabled=%v", enabled)
			}
			if time.Since(started) > time.Second {
				t.Fatal("deadline exceeded")
			}
		})
	}
}

func TestRouteReuse(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			dir := t.TempDir()
			var calls atomic.Int32
			getenv := func(key string) string {
				return map[string]string{"GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_JOB": "build"}[key]
			}
			setup := func(time.Time) bool { calls.Add(1); time.Sleep(10 * time.Millisecond); return enabled }
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() {
					if got := selectRoute(dir, getenv, setup); got != enabled {
						t.Errorf("route=%v", got)
					}
				})
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatal("repeated setup", calls.Load())
			}
			next := func(key string) string {
				if key == "GITHUB_RUN_ATTEMPT" {
					return "2"
				}
				return getenv(key)
			}
			selectRoute(dir, next, setup)
			if calls.Load() != 2 {
				t.Fatal("reused previous job")
			}
		})
	}
}

func TestMissingInfrastructureAndContainer(t *testing.T) {
	getenv := func(string) string { return "1" }
	setup := func(time.Time) bool { t.Fatal("unexpected setup"); return true }
	if selectRoute(filepath.Join(t.TempDir(), "missing"), getenv, setup) {
		t.Fatal("missing infrastructure enabled")
	}
	args := os.Args
	os.Args = []string{"/__e/node24/bin/node", "script"}
	defer func() { os.Args = args }()
	if selectRoute(t.TempDir(), getenv, setup) {
		t.Fatal("container enabled")
	}
}
