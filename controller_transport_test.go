package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestControllerTransportKeepsCredentialsOffArgvAndEnvironment(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "control-plane-secret")
	t.Setenv("RUNWAY_GITHUB_APP_PRIVATE_KEY_FILE", "/private/app.pem")
	snapshot := pilotSnapshot{Pool: pilotPool{ID: "test", Enabled: true}, Leases: []pilotLease{{ID: "test", Phase: "running"}}}
	metrics := newControllerMetrics(snapshot, time.Now)
	transport := commandTransport{observeCache: metrics.observeGuest, command: []string{executable, "-test.run=TestControllerTransportHelper", "--", "runway-test-helper"}, timeout: time.Second * 5}
	for range 2 {
		state, err := transport.Call(context.Background(), "bootstrap", pilotLease{ID: "test", Phase: "running", VMID: "vm-0000001", RunnerName: "runway-test", Deadline: time.Now().Add(time.Minute)}, "stdin-jit-secret")
		if err != nil || state != "running" {
			t.Fatalf("invalid transport result %q %v", state, err)
		}
	}
	assertMetric(t, metrics.write,
		`cache_routed_total{pool="test",region=""} 4`,
		`cache_fallback_total{pool="test",region=""} 2`,
		`cache_relay_errors_total{pool="test",region=""} 1`,
		`cache_extraction_available_bytes{pool="test",region=""} 4096`)
}

func TestControllerTransportHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "runway-test-helper") {
		return
	}
	if os.Getenv("GH_TOKEN") != "" || os.Getenv("RUNWAY_GITHUB_APP_PRIVATE_KEY_FILE") != "" {
		os.Exit(2)
	}
	if strings.Contains(strings.Join(os.Args, " "), "stdin-jit-secret") {
		os.Exit(3)
	}
	deadlineFound := false
	for i, arg := range os.Args {
		if arg == "--deadline" && i+1 < len(os.Args) {
			deadline, err := time.Parse(time.RFC3339Nano, os.Args[i+1])
			if err != nil || !deadline.After(time.Now()) {
				os.Exit(5)
			}
			deadlineFound = true
		}
	}
	if !deadlineFound {
		os.Exit(6)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || string(data) != "stdin-jit-secret\n" {
		os.Exit(4)
	}
	fmt.Println(`{"state":"running","cache_metrics":{"routed":4,"fallback":2,"relay_errors":1,"extraction_bytes":4096}}`)
	os.Exit(0)
}

// TestControllerTransportTimingsHelper stands in for the guest program when the transport
// is asked to read a status document that carries preparation timings.
func TestControllerTransportTimingsHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "runway-timings-helper") {
		return
	}
	fmt.Println(`{"state":"installed","timings_ms":{"state":3,"kernel_publish":41}}`)
	os.Exit(0)
}

func TestControllerTransportEmitsPreparationTimingsWhenTheGuestReportsThem(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	transport := commandTransport{
		command: []string{executable, "-test.run=TestControllerTransportTimingsHelper", "--", "runway-timings-helper"},
		timeout: time.Second * 5,
	}
	// The caller only ever reads the state string, so a field nobody looks at is a
	// measurement nobody gets. Capture what the transport writes to stderr.
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = write
	state, callErr := transport.Call(context.Background(), "prepare",
		pilotLease{RunnerName: "runway-lease-timings", VMID: "vm-123"}, "")
	os.Stderr = saved
	write.Close()
	emitted, _ := io.ReadAll(read)
	read.Close()

	if callErr != nil || state != "installed" {
		t.Fatalf("a status document with timings returned %q, %v", state, callErr)
	}
	var event struct {
		Event     string         `json:"event"`
		Runner    string         `json:"runner"`
		TimingsMs map[string]int `json:"timings_ms"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(emitted), &event); err != nil {
		t.Fatalf("transport emitted %q, which is not one event: %v", emitted, err)
	}
	if event.Event != "guest_prepare_timings" || event.Runner != "runway-lease-timings" {
		t.Fatalf("emitted %+v, want the timings attributed to this runner", event)
	}
	if event.TimingsMs["kernel_publish"] != 41 || event.TimingsMs["state"] != 3 {
		t.Fatalf("timings arrived as %v, not the numbers the guest reported", event.TimingsMs)
	}
}

func TestControllerTransportRegionalCache(t *testing.T) {
	for _, pool := range []pilotPool{
		{RegionalCache: true, CacheService: "internal-cache.example.test:8443", CacheCA: "public CA"},
		{RegionalCache: true, CacheService: "internal-cache.example.test:8443"},
		{RegionalCache: true, CacheCA: "public CA"},
		{CacheService: "internal-cache.example.test:8443", CacheCA: "public CA"},
	} {
		executable, _ := os.Executable()
		record := t.TempDir() + "/ca-path"
		transport := commandTransport{command: []string{executable, "-test.run=^TestControllerTransportCacheHelper$", "--", record}, timeout: 5 * time.Second}
		state, err := transport.Call(context.Background(), "prepare", pilotLease{VMID: "vm", RunnerName: "runner", cachePool: pool}, "")
		if err != nil || state != "installed" {
			t.Fatalf("prepare: %q %v", state, err)
		}
		path, err := os.ReadFile(record)
		enabled := pool.RegionalCache && pool.CacheService != "" && pool.CacheCA != ""
		if enabled {
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(string(path)); !os.IsNotExist(err) {
				t.Fatal("temporary CA retained", err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatal("regional cache enabled without complete trust")
		}
	}
}

func TestControllerTransportCacheHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	record := os.Args[index+1]
	for i, arg := range os.Args {
		if arg == "--regional-cache" && os.Args[i+1] != "internal-cache.example.test:8443" {
			os.Exit(2)
		}
		if arg == "--regional-cache-ca" {
			path := os.Args[i+1]
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "public CA" {
				os.Exit(3)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				os.Exit(4)
			}
			if os.WriteFile(record, []byte(path), 0600) != nil {
				os.Exit(5)
			}
		}
	}
	fmt.Println(`{"state":"installed"}`)
	os.Exit(0)
}
