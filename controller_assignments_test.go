package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

func assignmentFixture(t *testing.T, status *atomic.Value, duplicate bool) (*pilotEngine, *memoryPilotStore, scaleset.JobMessageBase, *atomic.Int32) {
	t.Helper()
	s := newPilotStore()
	lease := prepareVM(t, s)
	lease.Phase = "running"
	lease.RunnerID = 42
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	calls := &atomic.Int32{}
	e.assignmentREST = &http.Client{Transport: controllerRESTTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `{"run_attempt":1}`
		if strings.Contains(r.URL.Path, "/jobs") {
			body = fmt.Sprintf(`{"total_count":1,"jobs":[{"id":100,"name":"test","run_id":4242,"run_attempt":1,"status":%q,"runner_id":42,"runner_name":%q,"started_at":"2026-11-08T00:00:00Z"}]}`, status.Load().(string), lease.RunnerName)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	job := scaleset.JobMessageBase{JobID: uuid.NewString(), WorkflowRunID: 4242, OwnerName: "test", RepositoryName: "repo", JobDisplayName: "test", JobWorkflowRef: "test/repo/.github/workflows/ci.yml@refs/heads/main", EventName: "push"}
	jobs := []*scaleset.JobAssigned{{JobMessageBase: job}}
	if duplicate {
		other := job
		other.JobID = uuid.NewString()
		jobs = append(jobs, &scaleset.JobAssigned{JobMessageBase: other})
	}
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: jobs}); err != nil {
		t.Fatal(err)
	}
	return e, s, job, calls
}
func drainAssignment(t *testing.T, e *pilotEngine) {
	t.Helper()
	select {
	case result := <-e.assignmentResults:
		e.assignmentResults <- result
	case <-time.After(time.Second):
		t.Fatal("REST worker did not finish")
	}
	if err := e.resolveAssignments(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestControllerAssignmentREST(t *testing.T) {
	for _, scenario := range []string{"early", "lag", "ambiguous", "no-match"} {
		t.Run(scenario, func(t *testing.T) {
			var status atomic.Value
			status.Store("in_progress")
			if scenario == "lag" || scenario == "no-match" {
				status.Store("queued")
			}
			e, s, job, calls := assignmentFixture(t, &status, scenario == "ambiguous")
			drainAssignment(t, e)
			if scenario == "lag" {
				if s.snapshot.Leases[0].JobID != "" {
					t.Fatal("queued job resolved")
				}
				status.Store("in_progress")
				e.pendingAssignments[4242].next = time.Time{}
				e.resolveAssignments(context.Background())
				drainAssignment(t, e)
			}
			got := s.snapshot.Leases[0]
			if scenario == "ambiguous" || scenario == "no-match" {
				if got.JobID != "" {
					t.Fatal("unsafe match")
				}
			} else {
				if got.JobID != job.JobID || got.Started || got.RunAttempt != "1" {
					t.Fatalf("early evidence missing: %+v", got)
				}
				count := len(s.events)
				e = newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
				if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: job}}}); err != nil {
					t.Fatal(err)
				}
				if len(s.events) != count {
					t.Fatal("restart replay wrote evidence")
				}
				conflict := job
				conflict.JobID = uuid.NewString()
				if err := e.Handle(t.Context(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: conflict, RunnerName: got.RunnerName, RunnerID: 42}}}); err == nil {
					t.Fatal("conflicting start accepted")
				}
			}
			if scenario == "no-match" {
				if err := e.Handle(t.Context(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: job, RunnerName: got.RunnerName, RunnerID: 42}}}); err != nil {
					t.Fatal(err)
				}
				if !s.snapshot.Leases[0].Started {
					t.Fatal("start did not resolve")
				}
			}
			if calls.Load() > 4 {
				t.Fatalf("unexpected REST calls: %d", calls.Load())
			}
		})
	}
}
func TestControllerAssignmentBounds(t *testing.T) {
	var status atomic.Value
	status.Store("queued")
	e, _, job, _ := assignmentFixture(t, &status, false)
	drainAssignment(t, e)
	now := e.now()
	e.now = func() time.Time { return now.Add(controllerAssignmentTTL) }
	e.resolveAssignments(context.Background())
	if len(e.pendingAssignments) != 0 {
		t.Fatal("expired hints retained")
	}
	for i := 0; i <= controllerAssignmentCap; i++ {
		job.JobID = uuid.NewString()
		e.rememberAssignment(job)
	}
	if len(e.pendingAssignments) != 0 || !e.now().Before(e.assignmentSaturated) {
		t.Fatal("cap did not fail closed")
	}
}

func TestControllerAssignmentRetryBudget(t *testing.T) {
	var status atomic.Value
	status.Store("queued")
	e, _, job, calls := assignmentFixture(t, &status, false)
	drainAssignment(t, e)
	for i := 1; i < controllerAssignmentTries; i++ {
		e.pendingAssignments[4242].next = time.Time{}
		if err := e.resolveAssignments(context.Background()); err != nil {
			t.Fatal(err)
		}
		drainAssignment(t, e)
	}
	e.rememberAssignment(job)
	e.pendingAssignments[4242].next = time.Time{}
	e.resolveAssignments(context.Background())
	if e.assignmentBusy || calls.Load() != 2*controllerAssignmentTries {
		t.Fatalf("retry budget escaped: busy=%v calls=%d", e.assignmentBusy, calls.Load())
	}
}

func TestControllerAssignmentRunnerIdentity(t *testing.T) {
	for _, identity := range []struct {
		name, id string
		want     bool
	}{{"bound", "null", true}, {"other", "42", false}, {"bound", "99", false}} {
		t.Run(identity.name+identity.id, func(t *testing.T) {
			var status atomic.Value
			status.Store("queued")
			e, s, _, _ := assignmentFixture(t, &status, false)
			drainAssignment(t, e)
			name := identity.name
			if name == "bound" {
				name = s.snapshot.Leases[0].RunnerName
			}
			e.assignmentREST = &http.Client{Transport: controllerRESTTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"run_attempt":1}`
				if strings.Contains(r.URL.Path, "/jobs") {
					body = fmt.Sprintf(`{"total_count":1,"jobs":[{"id":100,"name":"test","run_id":4242,"run_attempt":1,"status":"in_progress","runner_id":%s,"runner_name":%q,"started_at":"2026-11-08T00:00:00Z"}]}`, identity.id, name)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			e.pendingAssignments[4242].next = time.Time{}
			e.resolveAssignments(context.Background())
			drainAssignment(t, e)
			if (s.snapshot.Leases[0].JobID != "") != identity.want {
				t.Fatal("incorrect runner attribution")
			}
		})
	}
}

func TestControllerAssignmentStopsOnStarted(t *testing.T) {
	var status atomic.Value
	status.Store("queued")
	e, s, job, _ := assignmentFixture(t, &status, false)
	drainAssignment(t, e)
	entered := make(chan struct{})
	var once sync.Once
	e.assignmentREST = &http.Client{Transport: controllerRESTTransport(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	e.pendingAssignments[4242].next = time.Time{}
	if err := e.resolveAssignments(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	lease := s.snapshot.Leases[0]
	if err := e.Handle(t.Context(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: job, RunnerName: lease.RunnerName, RunnerID: 42}}}); err != nil {
		t.Fatal(err)
	}
	drainAssignment(t, e)
	if e.assignmentBusy || !s.snapshot.Leases[0].Started {
		t.Fatal("worker survived JobStarted")
	}
}
