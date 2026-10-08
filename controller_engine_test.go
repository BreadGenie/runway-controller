package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type memoryPilotStore struct {
	snapshot                               pilotSnapshot
	events                                 []string
	reserveCalls, ensureCalls, deleteCalls int
	rejectUpdates                          bool
	loseCreateResponse                     bool
}

func newPilotStore() *memoryPilotStore {
	return &memoryPilotStore{snapshot: pilotSnapshot{Fence: 1, Pool: pilotPool{ID: "pool", RepositoryURL: "https://github.com/test/repo", ScaleSetID: 7, MaxConcurrency: 1, Enabled: true}}}
}
func (s *memoryPilotStore) Renew(context.Context, string) (pilotSnapshot, error) {
	result := s.snapshot
	result.Leases = append([]pilotLease(nil), s.snapshot.Leases...)
	return result, nil
}
func (s *memoryPilotStore) Reserve(_ context.Context, id string) (pilotLease, error) {
	s.reserveCalls++
	for _, lease := range s.snapshot.Leases {
		if lease.Phase != "deleted" || lease.JITState != "removed" {
			return pilotLease{}, errors.New("capacity occupied")
		}
	}
	lease := pilotLease{ID: id, RunnerName: "runway-" + id, Phase: "reserved", JITState: "none", Deadline: time.Now().Add(time.Hour)}
	s.snapshot.Leases = append(s.snapshot.Leases, lease)
	return lease, nil
}
func (s *memoryPilotStore) find(id string) *pilotLease {
	for i := range s.snapshot.Leases {
		if s.snapshot.Leases[i].ID == id {
			return &s.snapshot.Leases[i]
		}
	}
	panic("unknown lease")
}
func (s *memoryPilotStore) Update(_ context.Context, id, event string, fields leaseEvent) (pilotLease, error) {
	if s.rejectUpdates {
		return pilotLease{}, errors.New("fence rejected")
	}
	l := s.find(id)
	s.events = append(s.events, event)
	switch event {
	case "jit_requested":
		if l.Phase != "vm_ready" {
			return pilotLease{}, errors.New("invalid JIT phase")
		}
		l.Phase = "jit_pending"
		l.JITState = "pending"
	case "jit_ready":
		l.Phase = "jit_ready"
		l.JITState = "ready"
		l.RunnerID = fields.RunnerID
	case "bootstrapped":
		l.Phase = "running"
	case "started":
		l.Phase = "running"
		l.JobID = fields.JobID
		l.WorkflowRunID = fields.WorkflowRunID
	case "completed", "failed":
		l.Phase = "deleting"
		l.LastError = fields.Error
	case "runner_removed":
		l.JITState = "removed"
	case "acquire_requested":
		l.RequestID = fields.RequestID
		l.AcquireState = "pending"
		l.JobID = fields.JobID
		l.Repository = fields.Repository
		l.RequestLabels = fields.RequestLabels
	case "acquired":
		l.AcquireState = "acquired"
	default:
		panic(event)
	}
	return *l, nil
}
func (s *memoryPilotStore) EnsureVM(_ context.Context, id string) (pilotLease, error) {
	s.ensureCalls++
	l := s.find(id)
	l.VMID = "vm-0000001"
	if s.loseCreateResponse {
		s.loseCreateResponse = false
		l.Phase = "uncertain"
		return pilotLease{}, errors.New("response lost")
	}
	l.Phase = "vm_ready"
	l.VMState = "running"
	return *l, nil
}
func (s *memoryPilotStore) DeleteVM(_ context.Context, id string) (pilotLease, error) {
	s.deleteCalls++
	l := s.find(id)
	l.Phase = "deleted"
	return *l, nil
}

type fakePilotGitHub struct {
	store                          *memoryPilotStore
	runner                         *scaleset.RunnerReference
	jitCalls, removeCalls          int
	loseJITResponse, lookupFailure bool
}

func (g *fakePilotGitHub) GenerateJitRunnerConfig(_ context.Context, settings *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	g.jitCalls++
	found := false
	for _, l := range g.store.snapshot.Leases {
		if l.RunnerName == settings.Name && l.Phase == "jit_pending" {
			found = true
		}
	}
	if !found {
		panic("registration attempted without durable intent")
	}
	g.runner = &scaleset.RunnerReference{ID: 42, Name: settings.Name, RunnerScaleSetID: id}
	if g.loseJITResponse {
		return nil, errors.New("response lost with sensitive body")
	}
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: g.runner, EncodedJITConfig: "secret-jit-only-transport"}, nil
}
func (g *fakePilotGitHub) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	if g.lookupFailure {
		return nil, errors.New("GitHub offline")
	}
	return g.runner, nil
}
func (g *fakePilotGitHub) RemoveRunner(context.Context, int64) error {
	g.removeCalls++
	g.runner = nil
	return nil
}

type fakeGuestTransport struct {
	state                 string
	bootstrapCalls        int
	loseBootstrapResponse bool
	collect               func() error
}

func (g *fakeGuestTransport) Call(_ context.Context, action string, _ pilotLease, jit string) (string, error) {
	switch action {
	case "collect":
		if g.collect != nil {
			return g.state, g.collect()
		}
	case "prepare":
		return "installed", nil
	case "bootstrap":
		g.bootstrapCalls++
		if jit != "secret-jit-only-transport" {
			panic("bad bootstrap secret")
		}
		g.state = "running"
		if g.loseBootstrapResponse {
			return "unknown", errors.New("SSH response lost")
		}
	case "stop":
		g.state = "completed"
	}
	return g.state, nil
}

func TestControllerDiagnosticFailureDoesNotBlockDurableCompletion(t *testing.T) {
	s := newPilotStore()
	l := prepareVM(t, s)
	l.Phase = "running"
	l.RunnerID = 42
	l.JITState = "ready"
	collected := false
	guest := &fakeGuestTransport{state: "completed", collect: func() error {
		collected = true
		if s.find(l.ID).Phase != "running" {
			t.Fatal("diagnostics attempted after deletion became eligible")
		}
		return errors.New("untrusted guest error with secret content")
	}}
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, guest)
	err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: l.RunnerName, RunnerID: 42, Result: "succeeded", JobMessageBase: scaleset.JobMessageBase{JobID: "65a0bf0e-2335-5536-afac-ce28104d3b47", WorkflowRunID: 123}}}})
	if err != nil || !collected || s.find(l.ID).Phase != "deleting" {
		t.Fatal("diagnostics failure blocked completion")
	}
	if s.find(l.ID).LastError != "guest diagnostics unavailable; completion and cleanup continue" {
		t.Fatal("diagnostics error not safely recorded")
	}
}
func newPilotEngine(s *memoryPilotStore, g *fakePilotGitHub, t *fakeGuestTransport) *pilotEngine {
	snap, _ := s.Renew(context.Background(), "")
	return &pilotEngine{store: s, github: g, guest: t, snapshot: snap, now: time.Now, haveDemand: true, demand: 1}
}

// prepareVM reserves a lease, records the job it is for, and provisions it, which is the
// order the controller now requires: a VM is created only once a job has been recorded, so
// a helper that provisioned a job-less lease would no longer describe anything real.
// offerJob records a job on the engine's lease, which must happen before a VM is created.
// It writes through the store rather than the engine so these tests stay about provisioning;
// the engine's own acquisition path is covered where it is the subject.
func offerJob(t *testing.T, s *memoryPilotStore, e *pilotEngine) {
	t.Helper()
	if len(e.snapshot.Leases) == 0 {
		t.Fatal("no lease to offer a job to")
	}
	id := e.snapshot.Leases[0].ID
	if _, err := s.Update(context.Background(), id, "acquire_requested", leaseEvent{
		RequestID: 4242, JobID: "65a0bf0e-2335-5536-afac-ce28104d3b47",
		Repository: "BreadGenie/frappe", RequestLabels: "runway-2vcpu-6gb",
	}); err != nil {
		t.Fatal(err)
	}
	l := s.find(id)
	l.AcquireState = "acquired"
	e.remember(*l)
}

func prepareVM(t *testing.T, s *memoryPilotStore) *pilotLease {
	t.Helper()
	reserved, err := s.Reserve(context.Background(), "test-lease")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.Update(context.Background(), reserved.ID, "acquire_requested", leaseEvent{
		RequestID: 4242, JobID: "65a0bf0e-2335-5536-afac-ce28104d3b47",
		Repository: "BreadGenie/frappe", RequestLabels: "runway-2vcpu-6gb",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := s.find(updated.ID)
	lease.AcquireState = "acquired"
	if _, err := s.EnsureVM(context.Background(), lease.ID); err != nil {
		t.Fatal(err)
	}
	return s.find(lease.ID)
}

func TestControllerResumesActiveGuestAndCleansCompletedJob(t *testing.T) {
	ctx := context.Background()
	s := newPilotStore()
	g := &fakePilotGitHub{store: s}
	transport := &fakeGuestTransport{}
	e := newPilotEngine(s, g, transport)
	if err := e.Handle(ctx, &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 19}}); err != nil {
		t.Fatal(err)
	}
	if s.reserveCalls != 1 {
		t.Fatal("statistics must admit only one lease")
	}
	offerJob(t, s, e)
	for range 2 {
		if err := e.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A completely new engine obtains authoritative state from Central.
	e = newPilotEngine(s, g, transport)
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if g.jitCalls != 1 || transport.bootstrapCalls != 1 {
		t.Fatal("restart repeated registration/bootstrap")
	}
	l := s.snapshot.Leases[0]
	job := scaleset.JobMessageBase{JobID: "65a0bf0e-2335-5536-afac-ce28104d3b47", WorkflowRunID: 123}
	msg := &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{}, JobStartedMessages: []*scaleset.JobStarted{{RunnerName: l.RunnerName, RunnerID: 42, JobMessageBase: job}}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: l.RunnerName, RunnerID: 42, Result: "succeeded", JobMessageBase: job}}}
	if err := e.Handle(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if s.find(l.ID).Phase != "deleting" {
		t.Fatal("completion returned before durable intent")
	}
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s.find(l.ID).Phase != "deleted" || s.find(l.ID).JITState != "removed" || g.removeCalls != 1 {
		t.Fatal("resources not confirmed removed")
	}
}

func TestControllerUncertainJITNeverGeneratesTwice(t *testing.T) {
	s := newPilotStore()
	l := prepareVM(t, s)
	g := &fakePilotGitHub{store: s, loseJITResponse: true}
	guest := &fakeGuestTransport{}
	e := newPilotEngine(s, g, guest)
	if e.Reconcile(context.Background()) == nil {
		t.Fatal("expected uncertain JIT result")
	}
	e = newPilotEngine(s, g, guest)
	e.demand = 0
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.jitCalls != 1 || guest.bootstrapCalls != 0 || g.removeCalls != 1 || s.find(l.ID).Phase != "deleted" {
		t.Fatal("uncertain registration leaked or was retried")
	}
}

func TestControllerInterruptedJITPendingRetiresByName(t *testing.T) {
	s := newPilotStore()
	l := prepareVM(t, s)
	l.Phase = "jit_pending"
	l.JITState = "pending"
	g := &fakePilotGitHub{store: s, runner: &scaleset.RunnerReference{ID: 42, Name: l.RunnerName, RunnerScaleSetID: 7}}
	e := newPilotEngine(s, g, &fakeGuestTransport{})
	e.demand = 0
	for range 2 {
		if err := e.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if g.jitCalls != 0 || g.removeCalls != 1 {
		t.Fatal("pending restart did not retire existing registration")
	}
}

func TestControllerObservesUncertainBootstrapWithoutResendingSecret(t *testing.T) {
	s := newPilotStore()
	prepareVM(t, s)
	g := &fakePilotGitHub{store: s}
	guest := &fakeGuestTransport{loseBootstrapResponse: true}
	e := newPilotEngine(s, g, guest)
	if e.Reconcile(context.Background()) == nil {
		t.Fatal("expected uncertain bootstrap")
	}
	if s.snapshot.Leases[0].Phase != "jit_ready" {
		t.Fatal("bootstrap intent missing")
	}
	e = newPilotEngine(s, g, guest)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.snapshot.Leases[0].Phase != "running" || guest.bootstrapCalls != 1 || g.jitCalls != 1 {
		t.Fatal("replayed bootstrap instead of observing")
	}
}

func TestControllerUncertainVMCreateReusesDurableLease(t *testing.T) {
	s := newPilotStore()
	s.loseCreateResponse = true
	g := &fakePilotGitHub{store: s}
	guest := &fakeGuestTransport{}
	e := newPilotEngine(s, g, guest)
	// Reserve, record the job, then reconcile: the create response is lost on this call.
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
	}); err != nil {
		t.Fatal(err)
	}
	offerJob(t, s, e)
	if err := e.Reconcile(context.Background()); err == nil {
		t.Fatal("expected create response loss")
	}
	e = newPilotEngine(s, g, guest)
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.reserveCalls != 1 || len(s.snapshot.Leases) != 1 || s.snapshot.Leases[0].VMID != "vm-0000001" {
		t.Fatal("created replacement lease after uncertain outcome")
	}
}

func TestControllerFenceRejectionPreventsJITMutation(t *testing.T) {
	s := newPilotStore()
	prepareVM(t, s)
	s.rejectUpdates = true
	g := &fakePilotGitHub{store: s}
	guest := &fakeGuestTransport{}
	e := newPilotEngine(s, g, guest)
	if e.Reconcile(context.Background()) == nil {
		t.Fatal("expected stale fence rejection")
	}
	if g.jitCalls != 0 || guest.bootstrapCalls != 0 {
		t.Fatal("side effect escaped rejected fence")
	}
}

func TestControllerDeadlineDeletesVMEvenWhenGitHubIsUnavailable(t *testing.T) {
	s := newPilotStore()
	l := prepareVM(t, s)
	l.Phase = "running"
	l.JITState = "ready"
	l.Deadline = time.Now().Add(-time.Second)
	g := &fakePilotGitHub{store: s, lookupFailure: true}
	e := newPilotEngine(s, g, &fakeGuestTransport{state: "running"})
	if e.Reconcile(context.Background()) == nil {
		t.Fatal("unconfirmed GitHub cleanup must report unhealthy")
	}
	if s.deleteCalls != 1 || s.find(l.ID).Phase != "deleted" || !e.occupied() {
		t.Fatal("deadline left VM alive or admitted work before GitHub cleanup")
	}
}

func TestControllerAcquireCarriesRepositoryAndLabels(t *testing.T) {
	// The job's own repository and labels travel with the acquisition, because that is
	// the only point at which refusing a mismatched image is still free. A test that
	// sends a bare request ID would pass while the pairing check starves, which is the
	// shape of failure this exists to prevent.
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.acquirer = &fakeSession{acquire: func(ids []int64) ([]int64, error) { return ids, nil }}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 4, JobAvailableMessages: []*scaleset.JobAvailable{
		{JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 10, OwnerName: "BreadGenie", RepositoryName: "frappe",
			RequestLabels: []string{"self-hosted", "runway-dogfood"},
		}},
	}}
	if err := e.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	got := s.snapshot.Leases[0]
	if got.Repository != "BreadGenie/frappe" {
		t.Fatalf("repository not carried: %q", got.Repository)
	}
	if got.RequestLabels != "self-hosted\nrunway-dogfood" {
		t.Fatalf("labels not carried: %q", got.RequestLabels)
	}
}

// The reconciler provisions a lease on demand, before any job has been offered, and Central
// accepts that. But a lease that already has a job recorded is never provisioned again, and
// a lease with no job must not be provisioned at all: provisioning on demand alone made the
// controller create and destroy a VM per minute whenever statistics reported assigned jobs
// that this scale set never received.
func TestAProvisionedLeaseStillRecordsTheJobItIsGiven(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.acquirer = &fakeSession{acquire: func(ids []int64) ([]int64, error) { return ids, nil }}

	// Demand exists but no job has been offered: a lease is reserved.
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	// Reconcile the way the controller does between messages: demand is still reported, and
	// the lease still has no job.
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, l := range e.snapshot.Leases {
		if l.VMID != "" {
			t.Fatalf("a lease with no job was provisioned as %q; demand is not evidence of a job", l.VMID)
		}
	}
}

// A job offered while the lease is still reserved records who it is for, which is what the
// pairing check needs.
func TestAJobOnAReservedLeaseRecordsItsRepositoryAndLabels(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.acquirer = &fakeSession{acquire: func(ids []int64) ([]int64, error) { return ids, nil }}
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 2, JobAvailableMessages: []*scaleset.JobAvailable{
		{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 31, JobID: "j",
			OwnerName: "BreadGenie", RepositoryName: "frappe",
			RequestLabels: []string{"runway-2vcpu-6gb"}}},
	}}
	if err := e.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	l := e.snapshot.Leases[0]
	if l.Repository != "BreadGenie/frappe" || l.RequestLabels != "runway-2vcpu-6gb" {
		t.Fatalf("job not recorded: repo=%q labels=%q", l.Repository, l.RequestLabels)
	}
}

func TestControllerAcquireIntentSurvivesResponseLoss(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.acquirer = &fakeSession{acquire: func(ids []int64) ([]int64, error) {
		if s.snapshot.Leases[0].RequestID != ids[0] {
			t.Fatal("acquire before durable intent")
		}
		return nil, errors.New("lost")
	}}
	if e.Handle(context.Background(), available(10, 11)) == nil {
		t.Fatal("expected uncertain acquire")
	}
	e = newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	session := &fakeSession{}
	e.acquirer = session
	if err := e.Handle(context.Background(), available(12)); err != nil {
		t.Fatal(err)
	}
	if len(session.calls) != 1 || session.calls[0][0] != 10 || s.snapshot.Leases[0].AcquireState != "acquired" {
		t.Fatal("replaced reserved request after restart")
	}
}
