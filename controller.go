package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/hashicorp/go-retryablehttp"
)

func runController(path string) error {
	cfg, err := loadControllerConfig(path)
	if err != nil {
		return err
	}
	if cfg.OwnerID == "" {
		cfg.OwnerID = uuid.NewString()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	central, err := newCentralRPC(cfg)
	if err != nil {
		return err
	}
	snapshot, err := central.Claim(ctx)
	if err != nil {
		return err
	}
	if snapshot.Fence <= 0 || snapshot.Pool.MaxConcurrency != 1 || !repositoryURL.MatchString(snapshot.Pool.RepositoryURL) || snapshot.Pool.ScaleSetID <= 0 {
		return errors.New("pilot requires a fenced pool, repository URL, existing scale set, and max_concurrency=1")
	}
	// Mutating GitHub calls must not be blindly retried by the SDK's HTTP
	// retry policy. Uncertain JIT outcomes are represented by Central phases.
	adminHTTP := retryablehttp.NewClient()
	adminHTTP.RetryMax = 0
	github, err := authenticatedClient(ctx, snapshot.Pool.RepositoryURL, scaleset.WithRetryableHTTPClint(adminHTTP), scaleset.WithTimeout(20*time.Second))
	if err != nil {
		return err
	}
	engine := &pilotEngine{store: central, github: github, guest: commandTransport{command: cfg.TransportCommand, timeout: time.Duration(cfg.TransportTimeoutSeconds) * time.Second}, snapshot: snapshot, now: time.Now}
	json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "controller_claimed", "pool": cfg.Pool, "owner_id": cfg.OwnerID, "fence": snapshot.Fence})
	lifecycle := controllerLifecycle{
		engine: engine,
		open: func(ctx context.Context, pool pilotPool) (controllerSession, error) {
			return openGitHubSession(ctx, github, pool)
		},
		log: func(event string, err error) {
			fields := map[string]any{"event": event, "pool": cfg.Pool}
			if event == "controller_health" {
				fields["error"] = ""
				if err != nil {
					fields["error"] = err.Error()
				}
			}
			json.NewEncoder(os.Stderr).Encode(fields)
		},
	}
	if err := lifecycle.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "controller stopped; Central retains leases and enforces their deadlines")
	return nil
}
