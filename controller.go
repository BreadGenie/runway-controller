package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/hashicorp/go-retryablehttp"
)

type pilotDelivery struct {
	message *scaleset.RunnerScaleSetMessage
	result  chan error
}
type pilotScaler struct{ deliveries chan pilotDelivery }

func (s pilotScaler) Scale(ctx context.Context, message *scaleset.RunnerScaleSetMessage) error {
	delivery := pilotDelivery{message: message, result: make(chan error, 1)}
	select {
	case s.deliveries <- delivery:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-delivery.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// holdOwnership renews the fence on an interval until renewal fails or ctx ends.
// A renewal failure is fail-closed, but it keeps the sanitized Central cause so
// an operator can tell a fencing rejection from a blocked database transaction.
func holdOwnership(ctx context.Context, renew func(context.Context) error, every time.Duration) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// A ready ticker and a cancelled context are both selectable, and the
			// runtime picks between them at random. Renewing a cancelled context
			// would report a shutdown as a lost fence, so check before renewing.
			if ctx.Err() != nil {
				return nil
			}
			if err := renew(ctx); err != nil {
				// Cancellation can still race the renewal itself. Only a failure
				// while the controller is alive loses the fence.
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("Central ownership renewal failed; all side effects stopped: %w", err)
			}
		}
	}
}

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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	var lastError atomic.Value
	lastError.Store("")
	var ownershipLost atomic.Bool
	heartbeatErrors := make(chan error, 1)
	go func() {
		renew := func(ctx context.Context) error {
			_, err := central.Renew(ctx, lastError.Load().(string))
			return err
		}
		if err := holdOwnership(ctx, renew, 15*time.Second); err != nil {
			ownershipLost.Store(true)
			select {
			case heartbeatErrors <- err:
			default:
			}
			cancel()
		}
	}()
	report := func(err error) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		if lastError.Load().(string) != message {
			lastError.Store(message)
			json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "controller_health", "pool": cfg.Pool, "error": message})
		}
	}
	json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "controller_claimed", "pool": cfg.Pool, "owner_id": cfg.OwnerID, "fence": snapshot.Fence})
	for ctx.Err() == nil {
		sessionHTTP := retryablehttp.NewClient()
		sessionHTTP.RetryMax = 0
		session, err := github.MessageSessionClient(ctx, snapshot.Pool.ScaleSetID, "runway-pool-"+snapshot.Pool.ID, scaleset.WithRetryableHTTPClint(sessionHTTP), scaleset.WithTimeout(70*time.Second))
		if err != nil {
			report(errors.New("GitHub session unavailable; lease deadlines remain enforced by Central"))
			if fresh, renewErr := central.Renew(ctx, lastError.Load().(string)); renewErr == nil {
				engine.snapshot = fresh
				_ = engine.Reconcile(ctx)
			} else {
				return renewErr
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			continue
		}
		engine.acquirer = session
		capacity := 0
		if engine.snapshot.Pool.Enabled {
			capacity = 1
		}
		poller, err := listener.New(session, listener.Config{ScaleSetID: snapshot.Pool.ScaleSetID, MaxRunners: capacity})
		if err != nil {
			return err
		}
		sessionCtx, endSession := context.WithCancel(ctx)
		deliveries := make(chan pilotDelivery)
		pollingDone := make(chan error, 1)
		go func() { pollingDone <- poller.Run(sessionCtx, pilotScaler{deliveries: deliveries}) }()
		json.NewEncoder(os.Stderr).Encode(map[string]any{"event": "controller_listening", "pool": cfg.Pool})
		ticker := time.NewTicker(3 * time.Second)
		reconnect := false
		for !reconnect && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case delivery := <-deliveries:
				fresh, err := central.Renew(ctx, lastError.Load().(string))
				if err == nil {
					engine.snapshot = fresh
					capacity := 0
					if fresh.Pool.Enabled {
						capacity = 1
					}
					poller.SetMaxRunners(capacity)
					err = engine.Handle(ctx, delivery.message)
				}
				report(err)
				delivery.result <- err
			case <-ticker.C:
				fresh, err := central.Renew(ctx, lastError.Load().(string))
				if err == nil {
					engine.snapshot = fresh
					capacity := 0
					if fresh.Pool.Enabled {
						capacity = 1
					}
					poller.SetMaxRunners(capacity)
					err = engine.Reconcile(ctx)
				}
				report(err)
			case err := <-pollingDone:
				if ctx.Err() == nil {
					report(errors.New("GitHub listener stopped; reconnecting without discarding durable leases"))
				}
				_ = err
				reconnect = true
			}
		}
		ticker.Stop()
		endSession()
		if !ownershipLost.Load() {
			cleanupCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := session.Close(cleanupCtx); err != nil {
				report(errors.New("GitHub session close unconfirmed"))
			}
			closeCancel()
		}
	}
	select {
	case err := <-heartbeatErrors:
		return err
	default:
	}
	fmt.Fprintln(os.Stderr, "controller stopped; Central retains leases and enforces their deadlines")
	return nil
}
