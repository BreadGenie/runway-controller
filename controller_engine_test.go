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
	// contendEnsureVM refuses the first ensure_vm the way Central does under row-lock
	// contention, so a test can see whether the engine retries or gives up.
	contendEnsureVM bool
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
	// Keep this adapter strict; the contract tests also execute the actual
	// Central patch so these assumptions cannot silently diverge again.
	l := *s.find(id)
	if fields.RequestID < 0 || (fields.RequestID > 0 && l.RequestID > 0 && fields.RequestID != l.RequestID) {
		return pilotLease{}, errors.New("request identity changed")
	}
	for _, identity := range [][2]string{{l.JobID, fields.JobID}, {l.WorkflowRunID, fields.WorkflowRunID}, {l.Repository, fields.Repository}, {l.RequestLabels, fields.RequestLabels}} {
		if identity[0] != "" && identity[1] != "" && identity[0] != identity[1] {
			return pilotLease{}, errors.New("job identity changed")
		}
	}
	if fields.RunnerID != 0 && l.RunnerID != 0 && fields.RunnerID != l.RunnerID {
		return pilotLease{}, errors.New("runner identity changed")
	}
	switch event {
	case "acquire_requested":
		if fields.RequestID <= 0 || l.Phase == "deleting" || l.Phase == "deleted" {
			return pilotLease{}, errors.New("invalid acquisition intent")
		}
		if l.AcquireState != "acquired" {
			l.AcquireState = "pending"
		}
	case "acquired":
		if l.RequestID <= 0 || (l.AcquireState != "pending" && l.AcquireState != "acquired") {
			return pilotLease{}, errors.New("missing acquisition intent")
		}
		l.AcquireState = "acquired"
	case "runner_removed":
		l.JITState = "removed"
	case "completed", "failed":
		if l.Phase != "deleted" {
			l.Phase = "deleting"
		}
	default:
		transitions := map[string][2]string{
			"jit_requested": {"vm_ready", "jit_pending"},
			"jit_ready":     {"jit_pending", "jit_ready"},
			"bootstrapped":  {"jit_ready", "running"},
			"started":       {"running", "running"},
		}
		phases, ok := transitions[event]
		if !ok || (l.Phase != phases[0] && l.Phase != phases[1]) {
			return pilotLease{}, errors.New("invalid lease transition")
		}
		l.Phase = phases[1]
		if event == "jit_requested" {
			l.JITState = "pending"
		}
		if event == "jit_ready" {
			l.JITState = "ready"
		}
	}
	if fields.RunnerID != 0 {
		l.RunnerID = fields.RunnerID
	}
	if fields.RequestID != 0 {
		l.RequestID = fields.RequestID
	}
	if fields.JobID != "" {
		l.JobID = fields.JobID
	}
	if fields.WorkflowRunID != "" {
		l.WorkflowRunID = fields.WorkflowRunID
	}
	if fields.Repository != "" {
		l.Repository = fields.Repository
	}
	if fields.RequestLabels != "" {
		l.RequestLabels = fields.RequestLabels
	}
	if fields.Error != "" {
		l.LastError = fields.Error
	}
	*s.find(id) = l
	s.events = append(s.events, event)
	return l, nil
}

func (s *memoryPilotStore) EnsureVM(_ context.Context, id string) (pilotLease, error) {
	s.ensureCalls++
	if s.contendEnsureVM {
		s.contendEnsureVM = false
		return pilotLease{}, errCentralContended
	}
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
func prepareVM(t *testing.T, s *memoryPilotStore) *pilotLease {
	t.Helper()
	l, err := s.Reserve(context.Background(), "test-lease")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EnsureVM(context.Background(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s.find(l.ID)
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
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	if e.Reconcile(context.Background()) == nil {
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
	// Offered job evidence must survive acquisition. Other jobs are dispatched
	// directly and reveal their identity only after a runner exists.
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

// A Central HTTP 508 means another transaction held a row past the lock wait timeout. The
// request was refused before it changed anything, so the lease is intact and waiting one tick
// is the whole recovery. Returning the error instead killed the GitHub message session, which
// unregistered the scale set and stalled every queued job behind it.
func TestAContendedCentralLeaseIsRetriedNotDropped(t *testing.T) {
	s := newPilotStore()
	s.contendEnsureVM = true
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.acquirer = &fakeSession{acquire: func(ids []int64) ([]int64, error) { return ids, nil }}
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	// First reconcile hits the contention and must not surface it.
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatalf("a contended Central call must be a delay, not a failure: %v", err)
	}
	// The lease is still there, still owned, and the next pass succeeds.
	if len(e.snapshot.Leases) != 1 {
		t.Fatal("contention discarded a durable lease")
	}
	if got := e.snapshot.Leases[0].Phase; got != "reserved" {
		t.Fatalf("contention advanced the lease to %q", got)
	}
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatalf("recovery pass failed: %v", err)
	}
	if s.ensureCalls != 2 {
		t.Fatalf("expected a retry, got %d ensure_vm calls", s.ensureCalls)
	}
	if got := e.snapshot.Leases[0].Phase; got != "vm_ready" {
		t.Fatalf("after the retry the lease is %q, want vm_ready", got)
	}
}

// GitHub offers a job only once a runner exists to take it, and a runner is registered from
// the VM the lease builds. So the offer arrives when the lease is already provisioning or
// later, and keying the recording on "reserved" left every lease with no repository, no
// labels and no request ID -- which is what the pairing check needs in order to refuse a
// pool whose image cannot serve the job's repository.
func TestAJobOfferedAfterProvisioningStillRecordsWhoItIsFor(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.acquirer = &fakeSession{acquire: func(ids []int64) ([]int64, error) { return ids, nil }}

	// Demand alone: a lease is reserved and provisioned, as the controller does.
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{MessageID: 1,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.snapshot.Leases[0].Phase; got != "vm_ready" {
		t.Fatalf("test needs a provisioned lease, got %q", got)
	}

	// The job arrives now, and must still be recorded against the lease.
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 2, JobAvailableMessages: []*scaleset.JobAvailable{
		{JobMessageBase: scaleset.JobMessageBase{
			RunnerRequestID: 512, JobID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
			OwnerName: "BreadGenie", RepositoryName: "frappe",
			RequestLabels: []string{"runway-2vcpu-6gb"},
		}},
	}}
	if err := e.Handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	l := s.snapshot.Leases[0]
	if l.Repository != "BreadGenie/frappe" {
		t.Fatalf("repository not recorded on a provisioned lease: %q", l.Repository)
	}
	if l.RequestLabels != "runway-2vcpu-6gb" {
		t.Fatalf("labels not recorded on a provisioned lease: %q", l.RequestLabels)
	}
	if l.RequestID != 512 {
		t.Fatalf("request ID not recorded on a provisioned lease: %d", l.RequestID)
	}
	// The engine goes on to acquire against the recorded request ID, so the state moves past
	// "pending". What matters is that it is not "none", which is what a lease with no job
	// carries, and what every live lease carried before this was fixed.
	if l.AcquireState == "none" || l.AcquireState == "" {
		t.Fatalf("acquisition intent not recorded, got %q", l.AcquireState)
	}
}

// Direct dispatch can omit JobAvailable. Job identity must still be recorded
// from a start event after runner registration, and remain stable on replay.
func TestTheStartEventRecordsTheJobItWasSentFor(t *testing.T) {
	s := newPilotStore()
	g := &fakePilotGitHub{store: s}
	guest := &fakeGuestTransport{}
	e := newPilotEngine(s, g, guest)
	ctx := context.Background()
	if err := e.Handle(ctx, &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runner := s.snapshot.Leases[0]
	if runner.Phase != "running" || runner.JobID != "" {
		t.Fatal("runner must exist before direct job identity arrives")
	}
	msg := &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{
		RunnerID: runner.RunnerID, RunnerName: runner.RunnerName,
		JobMessageBase: scaleset.JobMessageBase{
			JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", WorkflowRunID: 4242,
			RunnerRequestID: 0, OwnerName: "BreadGenie", RepositoryName: "frappe",
			RequestLabels: []string{"runway-2vcpu-6gb"},
		},
	}}}
	if err := e.Handle(ctx, msg); err != nil {
		t.Fatal(err)
	}
	// Replay after restart must retain evidence without repeating registration.
	e = newPilotEngine(s, g, guest)
	if err := e.Handle(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got := s.find(runner.ID)
	if got.Repository != "BreadGenie/frappe" || got.RequestLabels != "runway-2vcpu-6gb" || got.WorkflowRunID != "4242" || got.RequestID != 0 || got.JobID != msg.JobStartedMessages[0].JobID {
		t.Fatalf("direct job evidence not retained: %+v", got)
	}
	if g.jitCalls != 1 || guest.bootstrapCalls != 1 {
		t.Fatal("replay repeated runner registration")
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
