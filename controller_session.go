package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/hashicorp/go-retryablehttp"
)

// controllerSession is an internal seam: the GitHub adapter and the test adapter
// expose the same polling, capacity and acquisition behavior to the lifecycle.
type controllerSession interface {
	jobAcquirer
	Run(context.Context, listener.Scaler) error
	SetMaxRunners(int)
	Close(context.Context) error
}

var errPoolResumed = errors.New("runner pool resumed")

type githubControllerSession struct {
	*scaleset.MessageSessionClient
	*listener.Listener
}

func openGitHubSession(ctx context.Context, github *scaleset.Client, pool pilotPool) (controllerSession, error) {
	http := retryablehttp.NewClient()
	http.RetryMax = 0
	session, err := github.MessageSessionClient(ctx, pool.ScaleSetID, "runway-pool-"+pool.ID,
		scaleset.WithRetryableHTTPClint(http), scaleset.WithTimeout(70*time.Second))
	if err != nil {
		return nil, err
	}
	poller, err := listener.New(session, listener.Config{ScaleSetID: pool.ScaleSetID, MaxRunners: poolCapacity(pool)})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		return nil, errors.Join(err, session.Close(cleanup))
	}
	return githubControllerSession{session, poller}, nil
}

// Run keeps one engine alive across message sessions. Only the heartbeat runs
// concurrently with the engine; deliveries and reconciliation remain serialized.
type controllerLifecycle struct {
	engine                                     *pilotEngine
	open                                       func(context.Context, pilotPool) (controllerSession, error)
	log                                        func(string, error)
	heartbeatEvery, reconcileEvery, retryAfter time.Duration
}

func (l controllerLifecycle) Run(parent context.Context) (runErr error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if l.heartbeatEvery == 0 {
		l.heartbeatEvery = 15 * time.Second
	}
	if l.reconcileEvery == 0 {
		l.reconcileEvery = 3 * time.Second
	}
	if l.retryAfter == 0 {
		l.retryAfter = 3 * time.Second
	}
	var lastError atomic.Value
	lastError.Store("")
	var ownershipLost atomic.Bool
	heartbeatDone := make(chan error, 1)
	go func() {
		err := holdOwnership(ctx, func(ctx context.Context) error {
			_, err := l.engine.store.Renew(ctx, lastError.Load().(string))
			return err
		}, l.heartbeatEvery)
		if err != nil {
			ownershipLost.Store(true)
			cancel()
		}
		heartbeatDone <- err
	}()
	// Join the heartbeat before returning, so its failure cannot be lost in a
	// shutdown race and no renewal survives the lifecycle.
	defer func() {
		cancel()
		if err := <-heartbeatDone; err != nil {
			runErr = err
		}
	}()
	report := func(err error) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		if lastError.Load().(string) != message {
			lastError.Store(message)
			l.log("controller_health", err)
		}
	}
	for ctx.Err() == nil {
		session, err := l.open(ctx, l.engine.snapshot.Pool)
		if ctx.Err() != nil {
			if session != nil && !ownershipLost.Load() {
				l.close(session, report)
			}
			break
		}
		if err != nil {
			report(sessionUnavailableError(l.engine.snapshot.Pool.ScaleSetID, l.engine.snapshot.Pool.ID, err))
			if err := l.refresh(ctx, nil, lastError.Load().(string)); err != nil {
				if ctx.Err() == nil {
					runErr = err
				}
				break
			}
			if err := l.engine.Reconcile(ctx); err != nil {
				if errors.Is(err, errCentralFenceRejected) {
					return err
				}
				report(err)
			}
		} else {
			l.engine.acquirer = session
			l.log("controller_listening", nil)
			err := l.listen(ctx, session, &lastError, report)
			l.engine.acquirer = nil
			if errors.Is(err, errCentralFenceRejected) {
				return err
			}
			if errors.Is(err, errPoolResumed) {
				l.engine.demand, l.engine.haveDemand = 0, false
				l.log("controller_pool_resumed", nil)
			} else if ctx.Err() == nil {
				if err == nil {
					err = errors.New("listener ended without an error")
				}
				report(fmt.Errorf("GitHub listener stopped; reconnecting without discarding durable leases: %w", err))
			}
			if !ownershipLost.Load() {
				l.close(session, report)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(l.retryAfter):
		}
	}
	return runErr
}

func (l controllerLifecycle) listen(ctx context.Context, session controllerSession, lastError *atomic.Value, report func(error)) error {
	sessionCtx, stop := context.WithCancel(ctx)
	defer stop()
	deliveries := make(chan pilotDelivery)
	done := make(chan error, 1)
	go func() { done <- session.Run(sessionCtx, pilotScaler{deliveries}) }()
	ticker := time.NewTicker(l.reconcileEvery)
	defer ticker.Stop()
	refresh := func() error {
		wasEnabled := l.engine.snapshot.Pool.Enabled
		if err := l.refresh(ctx, session, lastError.Load().(string)); err != nil {
			return err
		}
		// Changing capacity only affects the next GET. Idle polls can
		// keep returning no statistics, and token refresh stays on the
		// same session. Reopen to read the new session's initial demand.
		if !wasEnabled && l.engine.snapshot.Pool.Enabled {
			return errPoolResumed
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			stop()
			return <-done
		case delivery := <-deliveries:
			err := refresh()
			if err == nil {
				err = l.engine.Handle(ctx, delivery.message)
			}
			if errors.Is(err, errPoolResumed) || errors.Is(err, errCentralFenceRejected) {
				// Leave this delivery unacknowledged for the next session.
				delivery.result <- err
				<-done
				return err
			}
			report(err)
			delivery.result <- err
		case <-ticker.C:
			err := refresh()
			if err == nil {
				err = l.engine.Reconcile(ctx)
			}
			if errors.Is(err, errPoolResumed) || errors.Is(err, errCentralFenceRejected) {
				stop()
				<-done
				return err
			}
			report(err)
		case err := <-done:
			return err
		}
	}
}

func (l controllerLifecycle) refresh(ctx context.Context, session controllerSession, health string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fresh, err := l.engine.store.Renew(ctx, health)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.engine.snapshot = fresh
	if session != nil {
		session.SetMaxRunners(poolCapacity(fresh.Pool))
	}
	return nil
}

func (l controllerLifecycle) close(session controllerSession, report func(error)) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := session.Close(ctx); err != nil {
		report(fmt.Errorf("GitHub session close unconfirmed: %w", err))
	}
}

func poolCapacity(pool pilotPool) int {
	if pool.Enabled {
		return 1
	}
	return 0
}

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

// sessionUnavailableError names the scale set a session failed for and wraps the
// underlying error, so the health line says what failed instead of only that
// something did.
//
// This call is the one place a controller can be stopped by something outside its
// own control, and the causes are indistinguishable from the outside: a scale set
// that does not exist, a token that cannot open a session, a network blip. One
// generic line for all of them cost hours of diagnosis, because the health log
// read "GitHub session unavailable" while the real fault was a pool still naming
// a scale set that had been deleted, and nothing the controller printed could
// tell those apart. Wrapping rather than formatting keeps the cause reachable
// with errors.Is and errors.As.
func sessionUnavailableError(scaleSetID int, pool string, cause error) error {
	return fmt.Errorf("GitHub session unavailable for scale set %d (pool %s): %w; lease deadlines remain enforced by Central",
		scaleSetID, pool, cause)
}
