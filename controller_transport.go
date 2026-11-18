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
	observeCache func(pilotLease, guestCacheMetrics)
	command      []string
	timeout      time.Duration
}

func (t commandTransport) Call(ctx context.Context, action string, lease pilotLease, jit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	args := append([]string{}, t.command[1:]...)
	args = append(args, action, "--vm-id", lease.VMID, "--runner-name", lease.RunnerName)
	if action == "bootstrap" {
		args = append(args, "--deadline", lease.Deadline.UTC().Format(time.RFC3339Nano))
	}
	if action == "prepare" && lease.cachePool.RegionalCache && lease.cachePool.CacheService != "" && lease.cachePool.CacheCA != "" {
		file, err := os.CreateTemp("", "runway-cache-ca-*.pem")
		if err != nil {
			return "unknown", err
		}
		defer os.Remove(file.Name())
		_, writeErr := file.WriteString(lease.cachePool.CacheCA)
		closeErr := file.Close()
		if writeErr != nil {
			return "unknown", writeErr
		}
		if closeErr != nil {
			return "unknown", closeErr
		}
		args = append(args, "--regional-cache", lease.cachePool.CacheService, "--regional-cache-ca", file.Name())
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
		Cache     *guestCacheMetrics `json:"cache_metrics"`
		State     string             `json:"state"`
		TimingsMs map[string]int     `json:"timings_ms"`
	}
	if output.Len() > 4096 || json.Unmarshal(output.Bytes(), &status) != nil {
		return "unknown", errors.New("guest transport returned invalid status")
	}
	// Preparation is the largest part of what a job waits for, and the guest reports what
	// each step cost when an operator asks. Emit it here, because the caller only ever
	// sees the state string and a field nothing reads is a measurement nobody gets.
	if len(status.TimingsMs) > 0 {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{
			"event": "guest_prepare_timings", "runner": lease.RunnerName,
			"timings_ms": status.TimingsMs,
		})
	}
	switch status.State {
	case "installed", "running", "completed", "unknown":
		if t.observeCache != nil && status.Cache != nil {
			t.observeCache(lease, *status.Cache)
		}
		return status.State, nil
	default:
		return "unknown", errors.New("guest transport returned unsupported state")
	}
}
