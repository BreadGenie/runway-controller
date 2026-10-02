package main

import (
	"context"
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
	transport := commandTransport{command: []string{executable, "-test.run=TestControllerTransportHelper", "--", "runway-test-helper"}, timeout: time.Second * 5}
	state, err := transport.Call(context.Background(), "bootstrap", pilotLease{VMID: "vm-0000001", RunnerName: "runway-test", Deadline: time.Now().Add(time.Minute)}, "stdin-jit-secret")
	if err != nil || state != "running" {
		t.Fatalf("invalid transport result %q %v", state, err)
	}
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
	fmt.Println(`{"state":"running"}`)
	os.Exit(0)
}
