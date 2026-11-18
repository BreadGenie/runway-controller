package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

func assertMetric(t *testing.T, write func(io.Writer), samples ...string) {
	t.Helper()
	response := httptest.NewRecorder()
	metricsHandler(write).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, sample := range samples {
		if !strings.Contains(response.Body.String(), "\nrunway_"+sample+"\n") {
			t.Errorf("missing %s in scrape:\n%s", sample, response.Body.String())
		}
	}
}

func TestControllerDemandAndOwnershipMetricsThroughSession(t *testing.T) {
	store := newPilotStore()
	engine := newPilotEngine(store, &fakePilotGitHub{store: store}, &fakeGuestTransport{})
	var seconds atomic.Int64
	seconds.Store(1000)
	engine.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	engine.metrics = newControllerMetrics(engine.snapshot, engine.now)
	lifecycle, ctx, cancel := testLifecycle(t, engine)
	lifecycle.open = func(context.Context, pilotPool) (controllerSession, error) {
		return testControllerSession{run: func(ctx context.Context, scaler listener.Scaler) error {
			deliver := func(assigned, running int) error {
				return scaler.Scale(ctx, &scaleset.RunnerScaleSetMessage{
					Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned, TotalRunningJobs: running},
				})
			}
			if err := deliver(2, 0); err != nil {
				return err
			}
			seconds.Add(75)
			assertMetric(t, engine.metrics.write,
				`controller_assigned_jobs{pool="pool",region=""} 2`,
				`controller_waiting_jobs{pool="pool",region=""} 2`,
				`controller_unserved_seconds{pool="pool",region=""} 75`,
				`controller_no_progress_seconds{pool="pool",region=""} 75`,
				`controller_ownership_renewal_age_seconds{pool="pool",region=""} 75`,
				`controller_ownership_renewal_timestamp_seconds{pool="pool",region=""} 1000`,
				`controller_ownership_fence{pool="pool",region=""} 1`)
			if err := deliver(2, 1); err != nil {
				return err
			}
			assertMetric(t, engine.metrics.write,
				`controller_waiting_jobs{pool="pool",region=""} 1`,
				`controller_unserved_seconds{pool="pool",region=""} 75`,
				`controller_no_progress_seconds{pool="pool",region=""} 0`,
				`controller_ownership_renewal_age_seconds{pool="pool",region=""} 0`,
				`controller_ownership_renewal_timestamp_seconds{pool="pool",region=""} 1075`)
			if err := deliver(0, 0); err != nil {
				return err
			}
			assertMetric(t, engine.metrics.write, `controller_unserved_seconds{pool="pool",region=""} 0`)
			cancel()
			return nil
		}}, nil
	}
	if err := lifecycle.Run(ctx); err != nil || seconds.Load() != 1075 {
		t.Fatalf("session did not finish: %v", err)
	}
}

func TestControllerCleanupMetricsFollowLeaseReconciliation(t *testing.T) {
	store := newPilotStore()
	lease := prepareVM(t, store)
	now := time.Now().Truncate(time.Second)
	lease.Phase, lease.JITState, lease.Deadline = "running", "ready", now.Add(-11*time.Minute)
	github := &fakePilotGitHub{store: store, lookupFailure: true}
	engine := newPilotEngine(store, github, &fakeGuestTransport{})
	engine.now = func() time.Time { return now }
	engine.metrics = newControllerMetrics(engine.snapshot, engine.now)
	assertMetric(t, engine.metrics.write, `controller_running_runners{pool="pool",region=""} 1`)
	if err := engine.Reconcile(t.Context()); err == nil {
		t.Fatal("expected unconfirmed GitHub cleanup")
	}
	assertMetric(t, engine.metrics.write,
		`controller_running_runners{pool="pool",region=""} 0`,
		`controller_cleanup_leases{pool="pool",region=""} 1`,
		`controller_cleanup_oldest_seconds{pool="pool",region=""} 660`)
	github.lookupFailure = false
	engine.demand = 0
	if err := engine.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertMetric(t, engine.metrics.write,
		`controller_cleanup_leases{pool="pool",region=""} 0`,
		`controller_cleanup_oldest_seconds{pool="pool",region=""} 0`)
}

func TestMetricsRequiresLoopback(t *testing.T) {
	stop, err := startMetrics("0.0.0.0:0", func(io.Writer) {})
	if err == nil {
		stop()
		t.Fatal("metrics must only listen on loopback")
	}
}
