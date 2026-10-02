package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type guestTransport interface {
	Call(context.Context, string, pilotLease, string) (string, error)
}

type commandTransport struct {
	command []string
	timeout time.Duration
}

func (t commandTransport) Call(ctx context.Context, action string, lease pilotLease, jit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	args := append([]string{}, t.command[1:]...)
	args = append(args, action, "--vm-id", lease.VMID, "--runner-name", lease.RunnerName)
	if action == "bootstrap" {
		args = append(args, "--deadline", lease.Deadline.UTC().Format(time.RFC3339Nano))
	}
	cmd := exec.CommandContext(ctx, t.command[0], args...)
	// Do not inherit GitHub/App or control-plane credentials into transport.
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "SSH_AUTH_SOCK"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if action == "bootstrap" {
		cmd.Stdin = strings.NewReader(jit + "\n")
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "unknown", errors.New("guest " + action + " failed or timed out")
	}
	var status struct {
		State string `json:"state"`
	}
	if output.Len() > 4096 || json.Unmarshal(output.Bytes(), &status) != nil {
		return "unknown", errors.New("guest transport returned invalid status")
	}
	switch status.State {
	case "installed", "running", "completed", "unknown":
		return status.State, nil
	default:
		return "unknown", errors.New("guest transport returned unsupported state")
	}
}
