package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

func verifiedLeaseFixture(t *testing.T) (pilotLease, *http.Client) {
	t.Helper()
	a := newAuthorityCase(t)
	gh, _ := a.authority(t)
	return pilotLease{ID: "lease", Phase: "running", JobID: a.release.JobID, Repository: fixtureRepository, WorkflowRunID: "41867135947", JobWorkflowRef: fixtureWorkflow, JobDisplayName: "Integration (mariadb, 1)", EventName: "workflow_dispatch", RunnerID: 219, RunnerName: a.binding.RunnerName, Deadline: time.Now().Add(time.Hour)}, gh.client
}
func TestControllerVerification(t *testing.T) {
	for _, scenario := range []string{"success", "fork", "tag", "pr", "wrong runner", "completed", "queued", "attempt"} {
		t.Run(scenario, func(t *testing.T) {
			l, client := verifiedLeaseFixture(t)
			fixture := client.Transport.(*githubFixture)
			for path, bodies := range fixture.responses {
				for i, body := range bodies {
					switch scenario {
					case "fork":
						var run map[string]any
						if json.Unmarshal(body, &run) == nil && run["head_repository"] != nil {
							run["head_repository"].(map[string]any)["id"] = float64(1)
							body, _ = json.Marshal(run)
						}
					case "pr":
						var run map[string]any
						if json.Unmarshal(body, &run) == nil && run["event"] != nil {
							run["event"] = "pull_request"
							body, _ = json.Marshal(run)
						}
					case "completed":
						body = bytes.ReplaceAll(body, []byte(`"status":"in_progress"`), []byte(`"status":"completed"`))
					case "queued":
						body = bytes.ReplaceAll(body, []byte(`"status":"in_progress"`), []byte(`"status":"queued"`))
					}
					fixture.responses[path][i] = body
				}
			}
			if scenario == "tag" {
				l.JobWorkflowRef = "BreadGenie/frappe/.github/workflows/server-tests.yml@refs/tags/v1"
			}
			if scenario == "wrong runner" {
				l.RunnerID++
			}
			if scenario == "attempt" {
				l.RunAttempt = "2"
			}
			f, _ := verifyControllerJob(t.Context(), client, l)
			if scenario == "success" {
				if !f.Verified || f.Ref != fixtureRef || f.SHA != fixtureSHA || f.RepositoryID != 580439734 || f.DefaultBranch != "develop" || f.RunAttempt != "1" {
					t.Fatalf("missing evidence: %+v", f)
				}
			} else if f.Verified || f.VerifyReason == "" {
				t.Fatalf("unsafe verification: %+v", f)
			}
		})
	}
}
func drainVerification(t *testing.T, e *pilotEngine) {
	t.Helper()
	select {
	case r := <-e.verificationResults:
		e.verificationResults <- r
	case <-time.After(2 * time.Second):
		t.Fatal("verification worker stalled")
	}
	if err := e.verifyJobs(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}
func TestControllerVerificationRetryAndCentralImmutability(t *testing.T) {
	l, client := verifiedLeaseFixture(t)
	s := newPilotStore()
	prepareVM(t, s)
	l.ID = s.snapshot.Leases[0].ID
	*s.find(l.ID) = l
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.assignmentREST = client
	useCentralContract(t, e, s, "")
	fixture := client.Transport.(*githubFixture)
	for path, bodies := range fixture.responses {
		if bytes.Contains([]byte(path), []byte("/jobs")) {
			fixture.responses[path] = append([][]byte{[]byte(`{"total_count":0,"jobs":[]}`)}, bodies...)
		}
	}
	if err := e.verifyJobs(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	drainVerification(t, e)
	if s.snapshot.Leases[0].Verified || s.snapshot.Leases[0].VerifyReason == "" {
		t.Fatal("REST lag enabled cache")
	}
	e.verificationRetries[l.ID].next = time.Time{}
	if err := e.verifyJobs(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	drainVerification(t, e)
	got := s.snapshot.Leases[0]
	if !got.Verified || got.Ref != fixtureRef || got.RepositoryID != 580439734 {
		t.Fatalf("verification not persisted: %+v", got)
	}
	for _, f := range []leaseEvent{{Ref: "refs/heads/other"}, {SHA: "other"}, {RepositoryID: 1}, {DefaultBranch: "other"}, {VerifyReason: "changed"}} {
		if _, err := e.store.Update(context.Background(), got.ID, "verified", f); err == nil {
			t.Fatal("conflicting verified evidence accepted")
		}
	}
}

func TestControllerVerifiesAssignmentWithoutReconcileTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease, client := verifiedLeaseFixture(t)
		lease, job := unassignedVerificationLease(lease)
		store := newPilotStore()
		store.snapshot.Pool.RepositoryURL = "https://github.com/" + fixtureRepository
		store.snapshot.Leases = []pilotLease{lease}
		engine := newPilotEngine(store, &fakePilotGitHub{store: store}, &fakeGuestTransport{})
		engine.assignmentREST = client
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		engine.logTrace = func(trace controllerTrace) {
			if trace.Event == "controller_verification_applied" {
				cancel()
			}
		}
		session := testControllerSession{run: func(ctx context.Context, scaler listener.Scaler) error {
			msg := &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: job}}}
			if err := scaler.Scale(ctx, msg); err != nil && ctx.Err() == nil {
				return err
			}
			<-ctx.Done()
			return nil
		}}
		lifecycle := controllerLifecycle{engine: engine, reconcileEvery: 3 * time.Second}
		var health atomic.Value
		health.Store("")
		if err := lifecycle.listen(ctx, session, &health, func(err error) {
			if err != nil {
				t.Error(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		got := store.snapshot.Leases[0]
		if !got.Verified || got.Started || got.JobID != job.JobID || time.Since(start) != 0 {
			t.Fatalf("expected verified assignment at the initial fake time, got %+v after %v", got, time.Since(start))
		}
		fixture := client.Transport.(*githubFixture)
		if fixture.calls[fixtureJobsPath] != 2 {
			t.Fatalf("expected independent assignment and verification job reads, got %v", fixture.calls)
		}
	})
}

func unassignedVerificationLease(lease pilotLease) (pilotLease, scaleset.JobMessageBase) {
	job := scaleset.JobMessageBase{
		JobID: lease.JobID, WorkflowRunID: 41867135947,
		OwnerName: "BreadGenie", RepositoryName: "frappe",
		JobDisplayName: lease.JobDisplayName, JobWorkflowRef: lease.JobWorkflowRef, EventName: lease.EventName,
	}
	lease.JobID, lease.WorkflowRunID, lease.JobDisplayName, lease.JobWorkflowRef, lease.EventName = "", "", "", "", ""
	return lease, job
}

func TestControllerReplacementWaitsForItsOwnVerification(t *testing.T) {
	replacement, client := verifiedLeaseFixture(t)
	first := replacement
	first.ID, first.JobID, first.RunnerID, first.RunnerName = "first", uuid.NewString(), 218, "canceled-runner"
	replacement, job := unassignedVerificationLease(replacement)
	store := newPilotStore()
	store.snapshot.Pool.RepositoryURL = "https://github.com/" + fixtureRepository
	store.snapshot.Leases = []pilotLease{first}
	engine := newPilotEngine(store, &fakePilotGitHub{store: store}, &fakeGuestTransport{})
	if _, err := engine.update(t.Context(), first, "completed", leaseEvent{Result: "canceled"}); err != nil {
		t.Fatal(err)
	}
	store.snapshot.Leases = append(store.snapshot.Leases, replacement)
	engine.remember(replacement)
	engine.assignmentREST = client
	engine.verificationResults = make(chan controllerVerificationResult, 1)
	engine.verificationRetries = map[string]*controllerVerificationRetry{}
	engine.verificationResults <- controllerVerificationResult{lease: first, fields: leaseEvent{Verified: true}}
	// GitHub still reports the first runner when the replacement's assignment arrives.
	fixture := client.Transport.(*githubFixture)
	jobs := fixture.responses[fixtureJobsPath][0]
	oldRunnerJobs := bytes.ReplaceAll(jobs, []byte(replacement.RunnerName), []byte(first.RunnerName))
	oldRunnerJobs = bytes.ReplaceAll(oldRunnerJobs, []byte(`"runner_id":219`), []byte(`"runner_id":218`))
	fixture.responses[fixtureJobsPath] = [][]byte{oldRunnerJobs, jobs}
	if err := engine.Handle(t.Context(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: job}}}); err != nil {
		t.Fatal(err)
	}
	drainAssignment(t, engine)
	if store.find(first.ID).Verified || store.find(replacement.ID).JobID != "" {
		t.Fatal("canceled runner's result or REST job crossed lease identities")
	}
	checkCache := func(want string) {
		t.Helper()
		got := *store.find(replacement.ID)
		service, test := serviceFixture(t, func(lease *cacheServiceLease) {
			lease.Lease = got.ID
			lease.Job.Verified = got.Verified
			lease.Job.JobID = got.JobID
		}, http.StatusOK, false)
		if outcome := serviceSetup(t, service, test.setup, servicePeer); outcome != want {
			t.Fatalf("cache setup = %q, want %q", outcome, want)
		}
	}
	checkCache(http.StatusText(http.StatusTooEarly))
	engine.pendingAssignments[job.WorkflowRunID].next = time.Time{}
	if err := engine.resolveAssignments(t.Context()); err != nil {
		t.Fatal(err)
	}
	drainAssignment(t, engine)
	if got := store.find(replacement.ID); got.JobID != job.JobID || got.Started || got.Verified {
		t.Fatalf("expected replacement assignment alone, got %+v", got)
	}
	checkCache(http.StatusText(http.StatusTooEarly))
	if err := engine.verifyJobs(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	drainVerification(t, engine)
	checkCache("enabled")
	started := &scaleset.JobStarted{JobMessageBase: job, RunnerID: replacement.RunnerID, RunnerName: replacement.RunnerName}
	if err := engine.Handle(t.Context(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{started}}); err != nil {
		t.Fatal(err)
	}
	if got := store.find(replacement.ID); !got.Started || !got.Verified || got.JobID == first.JobID {
		t.Fatalf("late start changed replacement verification: %+v", got)
	}
}
