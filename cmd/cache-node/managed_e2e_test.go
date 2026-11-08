package main

import (
	"crypto/x509"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestManagedNodeProcess is a subprocess entry point for the managed E2E.
// Only filesystem locations and the loopback service name are injected. The
// production allowlist, per-job decision, setup wire and environment routing run
// unchanged, without needing root-owned /etc/runway and /run/runway on CI hosts.
func TestManagedNodeProcess(t *testing.T) {
	if os.Getenv("RUNWAY_MANAGED_NODE_PROCESS") != "1" {
		t.Skip("E2E subprocess only")
	}
	index := 0
	for i, arg := range os.Args {
		if arg == "--" {
			index = i + 1
			break
		}
	}
	if index == 0 || index >= len(os.Args) {
		t.Fatal("missing action arguments")
	}
	args := append([]string{os.Getenv("RUNWAY_REAL_NODE")}, os.Args[index:]...)
	roots := x509.NewCertPool()
	ca, err := os.ReadFile(os.Getenv("RUNWAY_TEST_CA"))
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		t.Fatal("test CA")
	}
	env := environment(args, os.Environ(), func() bool {
		return selectRoute(os.Getenv("RUNWAY_TEST_ROUTES"), os.Getenv, func(deadline time.Time) bool {
			return setupServiceWithRoots(&serviceConfig{Service: os.Getenv("RUNWAY_TEST_SERVICE")}, roots, deadline, os.Getenv)
		})
	}, func(line string) { os.Stderr.WriteString(line + "\n") })
	// The action receives no harness setup controls or service locations.
	clean := env[:0]
	for _, value := range env {
		if !strings.HasPrefix(value, "RUNWAY_TEST_") && !strings.HasPrefix(value, "RUNWAY_MANAGED_NODE_PROCESS=") {
			clean = append(clean, value)
		}
	}
	if path := os.Getenv("RUNWAY_TEST_ENV_AUDIT"); path != "" {
		raw, _ := json.Marshal(clean)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Exec(args[0], args, clean); err != nil {
		t.Fatal(err)
	}
}
