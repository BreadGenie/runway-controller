package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

type testControllerSession struct {
	run      func(context.Context, listener.Scaler) error
	close    func(context.Context) error
	capacity func(int)
}

func (s testControllerSession) Run(ctx context.Context, scaler listener.Scaler) error {
	return s.run(ctx, scaler)
}
func (s testControllerSession) Close(ctx context.Context) error {
	if s.close != nil {
		return s.close(ctx)
	}
	return nil
}
func (s testControllerSession) SetMaxRunners(n int) {
	if s.capacity != nil {
		s.capacity(n)
	}
}
func (s testControllerSession) AcquireJobs(context.Context, []int64) ([]int64, error) {
	return nil, nil
}

type lifecycleTestStore struct {
	pilotStore
	renew func(context.Context, string) (pilotSnapshot, error)
}

func (s lifecycleTestStore) Renew(ctx context.Context, health string) (pilotSnapshot, error) {
	return s.renew(ctx, health)
}

func testLifecycle(t *testing.T, engine *pilotEngine) (controllerLifecycle, context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return controllerLifecycle{engine: engine, log: func(string, error) {},
		heartbeatEvery: time.Hour, reconcileEvery: time.Hour, retryAfter: time.Millisecond}, ctx, cancel
}

func TestControllerLifecycleReconnectsWithDurableEvidenceAndListenerCause(t *testing.T) {
	s := newPilotStore()
	lease := pilotLease{ID: "durable", RunnerName: "runway-durable", RunnerID: 42,
		Phase: "running", JITState: "ready", JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e",
		Repository: "test/repo", WorkflowRunID: "123", Deadline: time.Now().Add(time.Hour)}
	s.snapshot.Leases = []pilotLease{lease}
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	cause := errors.New("poll returned HTTP 503")
	opens, closes := 0, 0
	var reported error
	l.log = func(event string, err error) {
		if err != nil && errors.Is(err, cause) {
			reported = err
		}
	}
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		opens++
		if opens == 2 && closes != 1 {
			t.Error("opened replacement before closing prior session")
		}
		return testControllerSession{
			run: func(ctx context.Context, scaler listener.Scaler) error {
				if opens == 1 {
					return cause
				}
				if *s.find(lease.ID) != lease {
					t.Error("reconnect changed durable lease")
				}
				err := scaler.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{
					{JobMessageBase: scaleset.JobMessageBase{JobID: lease.JobID}, RunnerName: lease.RunnerName, RunnerID: 42},
				}})
				cancel()
				return err
			},
			close: func(context.Context) error { closes++; return nil },
		}, nil
	}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if opens != 2 || closes != 2 || reported == nil {
		t.Fatalf("opens=%d closes=%d cause=%v", opens, closes, reported)
	}
	if s.find(lease.ID).Repository != lease.Repository || s.find(lease.ID).WorkflowRunID != lease.WorkflowRunID {
		t.Fatal("replayed message erased evidence")
	}
}

func TestControllerLifecycleReturnsFailedPersistenceToListenerAndAcceptsReplay(t *testing.T) {
	s := newPilotStore()
	s.snapshot.Leases = []pilotLease{{ID: "lease", RunnerName: "runway-lease", RunnerID: 42, Phase: "running", Deadline: time.Now().Add(time.Hour)}}
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	message := &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{
		JobMessageBase: scaleset.JobMessageBase{JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", OwnerName: "test", RepositoryName: "repo", WorkflowRunID: 123}, RunnerName: "runway-lease", RunnerID: 42,
	}}}
	opens := 0
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		opens++
		s.rejectUpdates = opens == 1
		return testControllerSession{run: func(ctx context.Context, scaler listener.Scaler) error {
			err := scaler.Scale(ctx, message)
			if opens == 1 {
				if err == nil || s.find("lease").JobID != "" {
					t.Error("failed persistence was accepted")
				}
			} else {
				if err != nil || s.find("lease").JobID != message.JobStartedMessages[0].JobID {
					t.Error("replay did not persist evidence")
				}
				cancel()
			}
			return err
		}}, nil
	}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if opens != 2 {
		t.Fatalf("got %d sessions", opens)
	}
}

func TestControllerLifecycleResumeLeavesDeliveryForReplay(t *testing.T) {
	s := newPilotStore()
	s.snapshot.Pool.Enabled = false
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	message := &scaleset.RunnerScaleSetMessage{
		MessageID: 5, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
	}
	opens := 0
	l.open = func(_ context.Context, pool pilotPool) (controllerSession, error) {
		opens++
		return testControllerSession{run: func(ctx context.Context, scaler listener.Scaler) error {
			if !pool.Enabled {
				s.snapshot.Pool.Enabled = true
				err := scaler.Scale(ctx, message)
				if err == nil || len(s.snapshot.Leases) != 0 {
					t.Error("old session accepted the delivery on resume")
				}
				return err
			}
			err := scaler.Scale(ctx, message)
			cancel()
			return err
		}}, nil
	}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if opens != 2 || len(s.snapshot.Leases) != 1 {
		t.Fatalf("replayed delivery did not reserve a lease: sessions=%d leases=%d", opens, len(s.snapshot.Leases))
	}
}

func TestControllerLifecycleReconcilesDeadlinesWhileGitHubCannotConnect(t *testing.T) {
	s := newPilotStore()
	s.snapshot.Leases = []pilotLease{{ID: "expired", Phase: "reserved", JITState: "none", Deadline: time.Now().Add(-time.Minute)}}
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	opens := 0
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		opens++
		if s.find("expired").Phase == "deleted" {
			cancel()
		}
		return nil, errors.New("GitHub unavailable")
	}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if opens < 2 || s.find("expired").Phase != "deleted" {
		t.Fatal("offline session prevented deadline cleanup")
	}
}

func TestControllerLifecycleOwnershipLossCancelsPollAndSkipsClose(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, _ := testLifecycle(t, e)
	l.heartbeatEvery = time.Millisecond
	polling := make(chan struct{})
	exited := make(chan struct{})
	cause := errors.New("Central returned HTTP 403")
	e.store = lifecycleTestStore{pilotStore: s, renew: func(ctx context.Context, _ string) (pilotSnapshot, error) {
		select {
		case <-polling:
			return pilotSnapshot{}, cause
		case <-ctx.Done():
			return pilotSnapshot{}, ctx.Err()
		}
	}}
	var closes atomic.Int32
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		return testControllerSession{
			run: func(ctx context.Context, _ listener.Scaler) error {
				close(polling)
				<-ctx.Done()
				close(exited)
				return ctx.Err()
			},
			close: func(context.Context) error { closes.Add(1); return nil },
		}, nil
	}
	err := l.Run(ctx)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "all side effects stopped") {
		t.Fatalf("lost ownership cause: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("polling survived ownership loss")
	}
	if closes.Load() != 0 {
		t.Fatal("session mutation after ownership loss")
	}
}

func TestControllerLifecycleShutdownWaitsForBlockedDelivery(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	started := make(chan struct{})
	exited := make(chan struct{})
	e.store = lifecycleTestStore{pilotStore: s, renew: func(ctx context.Context, _ string) (pilotSnapshot, error) {
		close(started)
		<-ctx.Done()
		return pilotSnapshot{}, ctx.Err()
	}}
	closed := false
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		return testControllerSession{
			run: func(ctx context.Context, scaler listener.Scaler) error {
				defer close(exited)
				return scaler.Scale(ctx, nil)
			},
			close: func(ctx context.Context) error {
				select {
				case <-exited:
				default:
					t.Error("closed before polling exited")
				}
				if ctx.Err() != nil {
					t.Error("close did not get a live cleanup context")
				}
				closed = true
				return nil
			},
		}, nil
	}
	go func() { <-started; cancel() }()
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("graceful shutdown did not close session")
	}
}

func TestControllerLifecycleShutdownDuringOfflineRenewalIsClean(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	started := make(chan struct{})
	e.store = lifecycleTestStore{pilotStore: s, renew: func(ctx context.Context, _ string) (pilotSnapshot, error) {
		close(started)
		<-ctx.Done()
		return pilotSnapshot{}, ctx.Err()
	}}
	l.open = func(context.Context, pilotPool) (controllerSession, error) { return nil, errors.New("offline") }
	go func() { <-started; cancel() }()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("shutdown reported as failure: %v", err)
	}
}

func TestControllerLifecycleResumeReadsFreshDemand(t *testing.T) {
	s := newPilotStore()
	s.snapshot.Pool.Enabled = false
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	l.reconcileEvery = time.Millisecond
	pausedAt := time.Now()
	e.now = func() time.Time { return pausedAt }
	var resumed atomic.Bool
	e.store = lifecycleTestStore{pilotStore: s, renew: func(ctx context.Context, health string) (pilotSnapshot, error) {
		snapshot, err := s.Renew(ctx, health)
		snapshot.Pool.Enabled = resumed.Load()
		return snapshot, err
	}}
	opens, closes := 0, 0
	l.open = func(_ context.Context, pool pilotPool) (controllerSession, error) {
		opens++
		return testControllerSession{
			run: func(ctx context.Context, scaler listener.Scaler) error {
				if !pool.Enabled {
					if err := scaler.Scale(ctx, &scaleset.RunnerScaleSetMessage{
						Statistics: &scaleset.RunnerScaleSetStatistic{},
					}); err != nil {
						return err
					}
					// A queued job becomes visible to a fresh session. This
					// zero-capacity poll remains outstanding on the old one.
					pausedAt = pausedAt.Add(time.Hour)
					resumed.Store(true)
					<-ctx.Done()
					return ctx.Err()
				}
				if closes != 1 {
					t.Error("paused session still open")
				}
				err := scaler.Scale(ctx, &scaleset.RunnerScaleSetMessage{
					Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
				})
				cancel()
				return err
			},
			close: func(context.Context) error { closes++; return nil },
		}, nil
	}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if opens != 2 || len(s.snapshot.Leases) != 1 {
		t.Fatalf("queued job did not reserve a lease after resume: sessions=%d leases=%d", opens, len(s.snapshot.Leases))
	}
}

func TestControllerLifecycleReplacementOwnerAdoptsExistingRunner(t *testing.T) {
	s := newPilotStore()
	lease := prepareVM(t, s)
	github := &fakePilotGitHub{store: s}
	guest := &fakeGuestTransport{}
	ownerA := newPilotEngine(s, github, guest)
	if err := ownerA.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	leaseID := lease.ID
	runnerName := s.find(leaseID).RunnerName
	runnerID := s.find(leaseID).RunnerID
	vmID := s.find(leaseID).VMID
	s.snapshot.Fence++

	ownerB := newPilotEngine(s, github, guest)
	l, ctx, cancel := testLifecycle(t, ownerB)
	l.reconcileEvery = time.Millisecond
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		return testControllerSession{run: func(ctx context.Context, scaler listener.Scaler) error {
			completed := &scaleset.JobCompleted{
				JobMessageBase: scaleset.JobMessageBase{JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"},
				RunnerID:       runnerID, RunnerName: runnerName, Result: "success",
			}
			if err := scaler.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{completed}}); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	ownerB.store = lifecycleTestStore{pilotStore: s, renew: func(ctx context.Context, health string) (pilotSnapshot, error) {
		if s.find(leaseID).Phase == "deleted" {
			cancel()
		}
		return s.Renew(ctx, health)
	}}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := s.find(leaseID)
	if got.VMID != vmID || got.RunnerID != runnerID || got.Phase != "deleted" ||
		got.JITState != "removed" || github.jitCalls != 1 || guest.bootstrapCalls != 1 {
		t.Fatalf("replacement failed to complete existing lease: %+v", got)
	}
}

func TestControllerLifecycleStaleOwnerDoesNotCloseSessionAfterRenewalDenial(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	l, ctx, cancel := testLifecycle(t, e)
	e.store = lifecycleTestStore{pilotStore: s, renew: func(context.Context, string) (pilotSnapshot, error) {
		return pilotSnapshot{}, errCentralFenceRejected
	}}
	l.open = func(context.Context, pilotPool) (controllerSession, error) {
		return testControllerSession{
			run: func(ctx context.Context, scaler listener.Scaler) error {
				return scaler.Scale(ctx, &scaleset.RunnerScaleSetMessage{
					Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
				})
			},
			close: func(context.Context) error {
				t.Error("stale owner closed the GitHub session")
				cancel()
				return nil
			},
		}, nil
	}
	if err := l.Run(ctx); !errors.Is(err, errCentralFenceRejected) {
		t.Fatalf("expected ownership rejection, got %v", err)
	}
	if s.reserveCalls != 0 {
		t.Fatal("stale owner reserved a lease")
	}
}
