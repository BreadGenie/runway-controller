package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

// Exercise the real Go HTTP adapter against the route, transition implementation
// and DocType extracted from the checked-in Central patch. VM/GitHub/guest I/O
// remains local; no Frappe site or infrastructure is touched.
func useCentralContract(t *testing.T, e *pilotEngine, s *memoryPilotStore, loseEvent string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for the checked-in Central contract tests")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]any
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		id, ok := fields["lease_id"].(string)
		if !ok {
			t.Error("missing lease id")
			w.WriteHeader(500)
			return
		}
		body, err := json.Marshal(map[string]any{"lease": s.find(id), "fields": fields})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		cmd := exec.Command(python, "integrations/central/contract_harness.py")
		cmd.Stdin = bytes.NewReader(body)
		var output, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &stderr
		if err := cmd.Run(); err != nil {
			if stderr.Len() > 0 {
				t.Errorf("Central contract harness failed: %s", &stderr)
			}
			w.WriteHeader(http.StatusBadRequest)
			w.Write(output.Bytes())
			return
		}
		var response struct {
			Message pilotLease `json:"message"`
		}
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		*s.find(id) = response.Message
		event := fields["event"].(string)
		s.events = append(s.events, event)
		if event == loseEvent {
			loseEvent = ""
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(output.Bytes())
	}))
	t.Cleanup(server.Close)
	e.store = contractPilotStore{memoryPilotStore: s, rpc: &centralRPC{baseURL: server.URL, pool: "pool", owner: "owner", fence: 1, http: server.Client()}}
}

type contractPilotStore struct {
	*memoryPilotStore
	rpc *centralRPC
}

func (s contractPilotStore) Update(ctx context.Context, id, event string, fields leaseEvent) (pilotLease, error) {
	return s.rpc.Update(ctx, id, event, fields)
}

func TestJobEvidenceAgainstCheckedInCentralContract(t *testing.T) {
	for _, scenario := range []struct {
		name                                                         string
		offer, loseBootstrap, loseStart, loseAcquire, completionOnly bool
	}{
		{name: "direct assignment"},
		{name: "offer after provisioning", offer: true},
		{name: "restart after uncertain acquisition", offer: true, loseAcquire: true},
		{name: "start confirms uncertain bootstrap", loseBootstrap: true},
		{name: "restart after lost start response", loseStart: true},
		{name: "completion without a start message", completionOnly: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			s := newPilotStore()
			g := &fakePilotGitHub{store: s}
			guest := &fakeGuestTransport{loseBootstrapResponse: scenario.loseBootstrap}
			e := newPilotEngine(s, g, guest)
			loseEvent := ""
			if scenario.loseStart {
				loseEvent = "started"
			}
			useCentralContract(t, e, s, loseEvent)
			store := e.store
			if err := e.Handle(ctx, &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}); err != nil {
				t.Fatal(err)
			}
			if err := e.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			job := scaleset.JobMessageBase{JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", WorkflowRunID: 4242, OwnerName: "BreadGenie", RepositoryName: "frappe", RequestLabels: []string{"self-hosted", "runway-2vcpu-6gb"}}
			session := &fakeSession{}
			if scenario.loseAcquire {
				session.acquire = func([]int64) ([]int64, error) {
					session.acquire = nil
					return nil, errors.New("acquisition response lost")
				}
			}
			e.acquirer = session
			if scenario.offer {
				job.RunnerRequestID = 77
				err := e.Handle(ctx, &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: job}}})
				if (err != nil) != scenario.loseAcquire {
					t.Fatalf("unexpected acquisition result: %v", err)
				}
				if scenario.loseAcquire {
					e = newPilotEngine(s, g, guest)
					e.store, e.acquirer = store, session
					if err := e.Handle(ctx, available(88)); err != nil {
						t.Fatal(err)
					}
					if len(session.calls) != 2 || session.calls[1][0] != 77 {
						t.Fatal("restart replaced uncertain job")
					}
				}
			}
			err := e.Reconcile(ctx)
			if (err != nil) != scenario.loseBootstrap {
				t.Fatalf("unexpected bootstrap result: %v", err)
			}
			runner := s.snapshot.Leases[0]
			start := &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerID: 42, RunnerName: runner.RunnerName, JobMessageBase: job}}}
			if !scenario.completionOnly {
				err = e.Handle(ctx, start)
				if (err != nil) != scenario.loseStart {
					t.Fatalf("unexpected start result: %v", err)
				}
				// Refresh from authoritative state and replay after process restart.
				e = newPilotEngine(s, g, guest)
				e.store, e.acquirer = store, session
				if err := e.Handle(ctx, start); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Handle(ctx, &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{}, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerID: 42, RunnerName: runner.RunnerName, Result: "succeeded", JobMessageBase: job}}}); err != nil {
				t.Fatal(err)
			}
			got := *s.find(runner.ID)
			if got.Phase != "deleting" || got.Repository != "BreadGenie/frappe" || got.RequestLabels != "self-hosted\nrunway-2vcpu-6gb" || got.RequestID != job.RunnerRequestID || got.JobID != job.JobID || got.WorkflowRunID != "4242" {
				t.Fatalf("job evidence lost: %+v", got)
			}
			if err := e.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if g.jitCalls != 1 || guest.bootstrapCalls != 1 || got.RunnerID != 42 || s.find(runner.ID).Phase != "deleted" || s.find(runner.ID).JITState != "removed" {
				t.Fatal("job recovery repeated bootstrap or failed cleanup")
			}
			if !scenario.offer && len(session.calls) > 0 {
				t.Fatal("direct assignment tried acquiring a job")
			}
		})
	}
}

func TestConflictingJobEvidenceHasNoSideEffects(t *testing.T) {
	for _, change := range []string{"request", "repository", "labels", "workflow", "job", "runner"} {
		t.Run(change, func(t *testing.T) {
			s := newPilotStore()
			lease := prepareVM(t, s)
			lease.Phase, lease.RunnerID, lease.JITState = "jit_ready", 42, "ready"
			lease.JobID, lease.WorkflowRunID, lease.RequestID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", "4242", 77
			lease.Repository, lease.RequestLabels = "BreadGenie/frappe", "runway-dogfood"
			guest := &fakeGuestTransport{collect: func() error { t.Fatal("collected conflicting job diagnostics"); return nil }}
			e := newPilotEngine(s, &fakePilotGitHub{store: s}, guest)
			job := scaleset.JobMessageBase{JobID: lease.JobID, WorkflowRunID: 4242, RunnerRequestID: 77, OwnerName: "BreadGenie", RepositoryName: "frappe", RequestLabels: []string{"runway-dogfood"}}
			runnerID := 42
			switch change {
			case "request":
				job.RunnerRequestID = 88
			case "repository":
				job.RepositoryName = "other"
			case "labels":
				job.RequestLabels = []string{"other"}
			case "workflow":
				job.WorkflowRunID = 999
			case "job":
				job.JobID = "65a0bf0e-2335-5536-afac-ce28104d3b47"
			case "runner":
				runnerID = 99
			}
			for _, message := range []*scaleset.RunnerScaleSetMessage{
				{JobStartedMessages: []*scaleset.JobStarted{{RunnerID: runnerID, RunnerName: lease.RunnerName, JobMessageBase: job}}},
				{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerID: runnerID, RunnerName: lease.RunnerName, JobMessageBase: job, Result: "success"}}},
			} {
				if err := e.Handle(context.Background(), message); err == nil {
					t.Fatal("accepted conflicting job evidence")
				}
			}
			if len(s.events) != 0 || lease.Phase != "jit_ready" || guest.bootstrapCalls != 0 {
				t.Fatal("conflicting evidence changed the lease")
			}
		})
	}
}

func TestUnavailableJobEvidenceDoesNotEraseIdentity(t *testing.T) {
	s := newPilotStore()
	lease := prepareVM(t, s)
	lease.Phase, lease.RunnerID = "running", 42
	lease.JobID, lease.WorkflowRunID, lease.RequestID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", "4242", 77
	lease.Repository, lease.RequestLabels = "BreadGenie/frappe", "runway-dogfood"
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	useCentralContract(t, e, s, "")
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerID: 42, RunnerName: lease.RunnerName, Result: "success", JobMessageBase: scaleset.JobMessageBase{JobID: lease.JobID}}}}); err != nil {
		t.Fatal(err)
	}
	got := s.find(lease.ID)
	if got.RequestID != 77 || got.WorkflowRunID != "4242" || got.Repository != "BreadGenie/frappe" || got.RequestLabels != "runway-dogfood" {
		t.Fatalf("missing evidence erased identity: %+v", got)
	}
}

func TestJobStartRequiresRegisteredRunner(t *testing.T) {
	for _, actual := range []bool{false, true} {
		s := newPilotStore()
		lease, err := s.Reserve(context.Background(), "early-start")
		if err != nil {
			t.Fatal(err)
		}
		e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
		if actual {
			useCentralContract(t, e, s, "")
		}
		err = e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerID: 42, RunnerName: lease.RunnerName, JobMessageBase: scaleset.JobMessageBase{JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"}}}})
		if err == nil || s.find(lease.ID).Phase != "reserved" {
			t.Fatalf("unregistered runner accepted start: %v", err)
		}
	}
}

func TestMalformedOfferIsNotAcquired(t *testing.T) {
	s := newPilotStore()
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	session := &fakeSession{acquire: func([]int64) ([]int64, error) {
		t.Fatal("malformed job was acquired")
		return nil, errors.New("unexpected")
	}}
	e.acquirer = session
	err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 77, OwnerName: "owner"}}}})
	if err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("malformed repository accepted: %v", err)
	}
}

func TestAssignedEvidenceOrderingAndRestart(t *testing.T) {
	for _, request := range []int64{0, 77} {
		for _, assignedFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("request=%d/assignedFirst=%v", request, assignedFirst), func(t *testing.T) {
				s := newPilotStore()
				lease := prepareVM(t, s)
				lease.Phase = "running"
				lease.RunnerID = 42
				lease.RequestID = request
				e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
				useCentralContract(t, e, s, "")
				store := e.store
				job := scaleset.JobMessageBase{JobID: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e", RunnerRequestID: request, WorkflowRunID: 4242, JobDisplayName: "test", JobWorkflowRef: "owner/repo/.github/workflows/ci.yml@refs/heads/main", EventName: "push"}
				assigned := &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: job}}}
				started := &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: lease.RunnerName, RunnerID: 42, JobMessageBase: job}}}
				if request > 0 {
					started.JobStartedMessages[0].JobDisplayName = ""
					started.JobStartedMessages[0].JobWorkflowRef = ""
					started.JobStartedMessages[0].EventName = ""
				}
				messages := []*scaleset.RunnerScaleSetMessage{started, assigned}
				if assignedFirst {
					messages = []*scaleset.RunnerScaleSetMessage{assigned, started}
				}
				for _, m := range messages {
					if err := e.Handle(context.Background(), m); err != nil {
						t.Fatal(err)
					}
				}
				got := *s.find(lease.ID)
				if got.JobDisplayName != job.JobDisplayName || got.JobWorkflowRef != job.JobWorkflowRef || got.EventName != "push" || !got.Started || got.WorkflowRunID != "4242" || got.RunAttempt != "" {
					t.Fatalf("lost evidence: %+v", got)
				}
				count := len(s.events)
				e = newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
				e.store = store
				for _, m := range messages {
					if err := e.Handle(context.Background(), m); err != nil {
						t.Fatal(err)
					}
				}
				if len(s.events) != count {
					t.Fatal("restart replay wrote evidence")
				}
				for _, field := range []string{"name", "ref", "run"} {
					conflict := job
					switch field {
					case "name":
						conflict.JobDisplayName = "other"
					case "ref":
						conflict.JobWorkflowRef = "other"
					case "run":
						conflict.WorkflowRunID = 999
					}
					if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: conflict}}}); err == nil {
						t.Fatal("accepted conflicting", field)
					}
				}
				if len(s.events) != count {
					t.Fatal("conflict wrote evidence")
				}
			})
		}
	}
}

func TestUnmatchedAssignmentIgnored(t *testing.T) {
	s := newPilotStore()
	prepareVM(t, s)
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	if err := e.Handle(context.Background(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 99}}}}); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 0 {
		t.Fatal("unmatched assignment changed lease")
	}
}
