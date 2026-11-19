package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
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
