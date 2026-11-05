package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
)

// Fixtures in testdata/cache-github are trimmed recordings of the GitHub REST
// responses for BreadGenie/frappe run 41867135947 (2026-11-04). Its two
// Integration jobs ran on JIT runners 218 (cold) and 219 (warm).
const (
	fixtureRepository = "BreadGenie/frappe"
	fixtureRunPath    = "/repos/BreadGenie/frappe/actions/runs/41867135947"
	fixtureJobsPath   = "/repos/BreadGenie/frappe/actions/runs/41867135947/attempts/1/jobs?per_page=100&page=1"
	fixtureSHA        = "3684b9a92b3e23ec0213e70f815c6ee5d0605572"
	fixtureRef        = "refs/heads/runway-cache-test-20261104"
	fixtureWorkflow   = "BreadGenie/frappe/.github/workflows/server-tests.yml@" + fixtureRef
)

func loadGitHubFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/cache-github/" + name)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func encodeFixture(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// githubFixture serves recorded responses by path and query, and counts calls.
type githubFixture struct {
	mu        sync.Mutex
	responses map[string][][]byte // later calls reuse the last response
	calls     map[string]int
	stall     bool
}

func (fixture *githubFixture) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" || request.Method != http.MethodGet {
		return nil, errors.New("fixture refused route")
	}
	key := request.URL.Path
	if request.URL.RawQuery != "" {
		key += "?" + request.URL.RawQuery
	}
	fixture.mu.Lock()
	stall := fixture.stall
	call := fixture.calls[key]
	fixture.calls[key]++
	bodies := fixture.responses[key]
	fixture.mu.Unlock()
	if stall {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	if len(bodies) == 0 {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)), Request: request}, nil
	}
	body := bodies[min(call, len(bodies)-1)]
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
}

// fakeAssignments stands in for the root-only records the scale-set observer writes.
type fakeAssignments struct {
	mu      sync.Mutex
	runners map[int64]cacheAssignment
	records map[string]cacheJobRecord
}

func (fake *fakeAssignments) assignment(runnerID int64) (cacheAssignment, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	record, ok := fake.runners[runnerID]
	if !ok {
		return cacheAssignment{}, errCacheJobNotFound
	}
	return record, nil
}

func (fake *fakeAssignments) job(jobID string) (cacheJobRecord, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	record, ok := fake.records[jobID]
	if !ok {
		return cacheJobRecord{}, errCacheJobNotFound
	}
	return record, nil
}

func (fake *fakeAssignments) jobs(runID int64) ([]cacheJobRecord, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var records []cacheJobRecord
	for _, record := range fake.records {
		if record.WorkflowRunID == runID {
			records = append(records, record)
		}
	}
	return records, nil
}

type authorityCase struct {
	repo, run   map[string]any
	jobs        []map[string]any
	binding     cacheGuestBinding
	release     cacheJobRelease
	setup       grant.SetupRequest
	assignments *fakeAssignments
}

func newAuthorityCase(t *testing.T) *authorityCase {
	t.Helper()
	jobs := loadGitHubFixture(t, "jobs-attempt-1.json")
	// The recording is of a finished run; the warm job is shown while running.
	for _, job := range jobs["jobs"].([]any) {
		job := job.(map[string]any)
		if job["runner_name"] == "runway-cache-test-1104-warm" {
			job["status"], job["conclusion"], job["completed_at"] = "in_progress", nil, nil
		}
	}
	value := func(s string) *string { return &s }
	jobID, coldJobID := "6f1d3e1c-5a43-4c2f-9f3e-0d5b8f2b7a11", "0e2c4a6b-8d1f-4e3a-9b5c-7d9e1f3a5b7c"
	now := time.Now()
	return &authorityCase{
		repo: loadGitHubFixture(t, "repo.json"), run: loadGitHubFixture(t, "run.json"), jobs: []map[string]any{jobs},
		binding: cacheGuestBinding{Version: 1, VMID: "rc-warm", VsockUDS: "/run/runway-cache/vms/rc-warm/v.sock", FirecrackerPID: 4242, GuestCID: 3, RunnerID: 219, RunnerName: "runway-cache-test-1104-warm", Repository: fixtureRepository, ExpiresAt: now.Add(3 * time.Hour)},
		release: cacheJobRelease{JobID: jobID, RequestID: 0, Stop: now.Add(3 * time.Hour)},
		setup: grant.SetupRequest{Version: 2, Nonce: "9c0f8e4a-2f7d-4e1b-8a3c-5d6e7f8a9b0c", Context: grant.SetupContext{
			Repository: value(fixtureRepository), RepositoryID: value("580439734"), RunID: value("41867135947"), RunAttempt: value("1"),
			Ref: value(fixtureRef), RefType: value("branch"), SHA: value(fixtureSHA), EventName: value("workflow_dispatch"), BaseRef: value(""), HeadRef: value(""),
		}},
		// The scale-set session saw both Integration jobs assigned in run 41867135947;
		// release is the warm runner's (219) job, "Integration (mariadb, 1)".
		assignments: &fakeAssignments{runners: map[int64]cacheAssignment{}, records: map[string]cacheJobRecord{
			jobID:     {Version: 1, JobID: jobID, WorkflowRunID: 41867135947, Repository: fixtureRepository, EventName: "workflow_dispatch", JobWorkflowRef: fixtureWorkflow, JobDisplayName: "Integration (mariadb, 1)", ObservedAt: now},
			coldJobID: {Version: 1, JobID: coldJobID, WorkflowRunID: 41867135947, Repository: fixtureRepository, EventName: "workflow_dispatch", JobWorkflowRef: fixtureWorkflow, JobDisplayName: "Integration (mariadb, 2)", ObservedAt: now},
		}},
	}
}

func (test *authorityCase) authority(t *testing.T) (githubCacheJobAuthority, *githubFixture) {
	t.Helper()
	fixture := &githubFixture{calls: map[string]int{}, responses: map[string][][]byte{
		"/repos/" + fixtureRepository: {encodeFixture(t, test.repo)},
		fixtureRunPath:                {encodeFixture(t, test.run)},
	}}
	for _, jobs := range test.jobs {
		fixture.responses[fixtureJobsPath] = append(fixture.responses[fixtureJobsPath], encodeFixture(t, jobs))
		for _, job := range jobs["jobs"].([]any) {
			path := "/repos/" + fixtureRepository + "/actions/jobs/" + fmt.Sprint(job.(map[string]any)["id"])
			fixture.responses[path] = append(fixture.responses[path], encodeFixture(t, job))
		}
	}
	return githubCacheJobAuthority{client: &http.Client{Transport: fixture}, assignments: test.assignments, regionID: "blr-1", interval: 10 * time.Millisecond, now: time.Now}, fixture
}

func fixtureJob(test *authorityCase, runner string) map[string]any {
	for _, job := range test.jobs[0]["jobs"].([]any) {
		if job.(map[string]any)["runner_name"] == runner {
			return job.(map[string]any)
		}
	}
	return nil
}

// resolve runs the setup-time resolution with the setup's run as the hint.
func (test *authorityCase) resolve(t *testing.T, authority githubCacheJobAuthority, budget time.Duration) (cacheJobRelease, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	return authority.resolve(ctx, test.binding, cacheRunHint(test.setup))
}

// resolveAndAuthorize resolves the bound runner's job, then authorizes setup.
func (test *authorityCase) resolveAndAuthorize(t *testing.T, authority githubCacheJobAuthority, budget time.Duration) (cacheJobGrant, error) {
	t.Helper()
	release, err := test.resolve(t, authority, budget)
	if err != nil {
		return cacheJobGrant{}, err
	}
	if release.JobID != test.release.JobID || release.Stop != test.binding.ExpiresAt {
		t.Fatalf("resolved the wrong job %+v", release)
	}
	return authority.authorize(t.Context(), test.binding, release, test.setup)
}

func TestGitHubJobAuthorityGrantsTheBoundRunnersInProgressJob(t *testing.T) {
	test := newAuthorityCase(t)
	authority, fixture := test.authority(t)
	verified, err := test.resolveAndAuthorize(t, authority, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	clock := grant.Clock{Earliest: now.Add(-time.Second), Latest: now.Add(time.Second)}
	bearer, err := grant.IssueSetup(private, "https://127.0.0.1:18445", verified.Authorization, verified.Policy, test.setup, clock)
	if err != nil {
		t.Fatal("verified job refused by grant policy:", err)
	}
	verifier, _ := grant.NewVerifier(map[string]ed25519.PublicKey{grant.KeyID(public): public}, "https://127.0.0.1:18445", "blr-1")
	claims, err := verifier.Verify(bearer, clock)
	if err != nil {
		t.Fatal(err)
	}
	if claims.RunID != 41867135947 || claims.RunAttempt != 1 || claims.RepositoryID != 580439734 || claims.JobID != test.release.JobID || claims.WriteRef != fixtureRef || len(claims.ReadRefs) != 2 || claims.ReadRefs[1] != "refs/heads/develop" {
		t.Fatalf("wrong grant scope %+v", claims)
	}
	if fixture.calls[fixtureJobsPath] != 1 {
		t.Fatal("unexpected job reads", fixture.calls)
	}
}

func TestGitHubJobAuthorityConfirmsFromScaleSetJobStarted(t *testing.T) {
	test := newAuthorityCase(t)
	delete(test.assignments.records, test.release.JobID) // JobAssigned missed; JobStarted alone suffices
	test.assignments.runners[219] = cacheAssignment{Version: 1, RunnerID: 219, RunnerName: "runway-cache-test-1104-warm", JobID: test.release.JobID, WorkflowRunID: 41867135947, Repository: fixtureRepository, EventName: "workflow_dispatch", JobWorkflowRef: fixtureWorkflow, ObservedAt: time.Now()}
	authority, fixture := test.authority(t)
	if _, err := test.resolveAndAuthorize(t, authority, time.Second); err != nil {
		t.Fatal(err)
	}
	if fixture.calls[fixtureJobsPath] != 0 {
		t.Fatal("REST jobs list read although the session confirmed", fixture.calls)
	}
	for name, change := range map[string]func(*cacheAssignment){
		"other name":    func(r *cacheAssignment) { r.RunnerName = "someone-else" },
		"other repo":    func(r *cacheAssignment) { r.Repository = "BreadGenie/other" },
		"completed job": func(r *cacheAssignment) { r.Completed = true },
		"tag workflow": func(r *cacheAssignment) {
			r.JobWorkflowRef = "BreadGenie/frappe/.github/workflows/server-tests.yml@refs/tags/runway-cache-test-20261104"
		},
	} {
		c := newAuthorityCase(t)
		record := test.assignments.runners[219]
		change(&record)
		c.assignments.runners[219] = record
		authority, _ := c.authority(t)
		if _, err := c.resolveAndAuthorize(t, authority, time.Second); err == nil {
			t.Fatalf("%s confirmed", name)
		}
	}
}

func TestGitHubJobAuthorityPollsUntilAssignmentIsVisible(t *testing.T) {
	test := newAuthorityCase(t)
	running := test.jobs[0]
	var queued map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encodeFixture(t, running)))
	decoder.UseNumber()
	decoder.Decode(&queued)
	for _, job := range queued["jobs"].([]any) {
		job := job.(map[string]any)
		if job["runner_name"] == "runway-cache-test-1104-warm" {
			job["status"], job["runner_id"], job["runner_name"], job["started_at"] = "queued", nil, nil, nil
		}
	}
	test.jobs = []map[string]any{queued, queued, running}
	authority, fixture := test.authority(t)
	if _, err := test.resolveAndAuthorize(t, authority, time.Second); err != nil {
		t.Fatal("lagging assignment not polled", err)
	}
	if fixture.calls[fixtureJobsPath] != 3 {
		t.Fatal("wrong poll count", fixture.calls[fixtureJobsPath])
	}
	// The poll is bounded by its deadline and never trusts the guest.
	test.jobs = []map[string]any{queued}
	authority, _ = test.authority(t)
	started := time.Now()
	if _, err := test.resolveAndAuthorize(t, authority, 200*time.Millisecond); !errors.Is(err, errCacheJobNotFound) || time.Since(started) > time.Second {
		t.Fatalf("unbounded or trusting confirmation: %v after %s", err, time.Since(started))
	}
	// Setup without a confirmation is refused outright.
	if _, err := authority.authorize(t.Context(), test.binding, test.release, test.setup); !errors.Is(err, errCacheJobNotFound) {
		t.Fatal("unconfirmed setup authorized", err)
	}
}

func TestGitHubJobAuthorityRefusesUnverifiedJobs(t *testing.T) {
	value := func(s string) *string { return &s }
	for name, test := range map[string]struct {
		change func(*authorityCase)
		want   error
	}{
		"another runner's job": {func(c *authorityCase) { c.binding.RunnerID, c.binding.RunnerName = 218, "runway-cache-test-1104-cold" }, errCacheJobCompleted},
		"unregistered runner":  {func(c *authorityCase) { c.binding.RunnerID = 999 }, errCacheJobNotFound},
		"runner name differs":  {func(c *authorityCase) { fixtureJob(c, "runway-cache-test-1104-warm")["runner_name"] = "someone-else" }, errCacheJobMismatch},
		"completed job": {func(c *authorityCase) {
			fixtureJob(c, "runway-cache-test-1104-warm")["status"] = "completed"
		}, errCacheJobCompleted},
		"job not yet assigned in the session": {func(c *authorityCase) { delete(c.assignments.records, c.release.JobID) }, errCacheJobNotFound},
		"job record in another repository": {func(c *authorityCase) {
			record := c.assignments.records[c.release.JobID]
			record.Repository = "BreadGenie/other"
			c.assignments.records[c.release.JobID] = record
		}, errCacheJobNotFound},
		"rerun repeats the display name": {func(c *authorityCase) {
			record := c.assignments.records[c.release.JobID]
			record.JobID = "33333333-4444-4555-8666-777777777777"
			c.assignments.records[record.JobID] = record
		}, errCacheJobMismatch},
		"no run to read": {func(c *authorityCase) { c.setup.Context.RunID = nil }, errCacheJobNotFound},
		"session sent no display name": {func(c *authorityCase) {
			record := c.assignments.records[c.release.JobID]
			record.JobDisplayName = ""
			c.assignments.records[c.release.JobID] = record
		}, errCacheJobNotFound},
		"wrong repository in setup":   {func(c *authorityCase) { c.setup.Context.Repository = value("BreadGenie/frappe-fork") }, errCacheJobMismatch},
		"wrong repository in binding": {func(c *authorityCase) { c.binding.Repository = "BreadGenie/other" }, errCacheJobNotFound},
		"repository renamed":          {func(c *authorityCase) { c.repo["full_name"] = "BreadGenie/renamed" }, errCacheRepository},
		"forged run id":               {func(c *authorityCase) { c.setup.Context.RunID = value("41867135948") }, errCacheJobNotFound},
		"forged run attempt":          {func(c *authorityCase) { c.setup.Context.RunAttempt = value("2") }, errCacheJobMismatch},
		"forged sha":                  {func(c *authorityCase) { c.setup.Context.SHA = value(strings.Repeat("a", 40)) }, errCacheRepository},
		"pull request": {func(c *authorityCase) {
			c.run["event"] = "pull_request"
			c.setup.Context.EventName = value("pull_request")
		}, errCacheRepository},
		"claims dispatch on pull request run": {func(c *authorityCase) { c.run["event"] = "pull_request" }, errCacheRepository},
		"fork": {func(c *authorityCase) {
			c.run["head_repository"].(map[string]any)["id"] = json.Number("999")
		}, errCacheRepository},
		"tag": {func(c *authorityCase) {
			c.run["head_branch"] = "v15.0.0"
			c.setup.Context.Ref, c.setup.Context.RefType = value("refs/tags/v15.0.0"), value("tag")
		}, errCacheJobMismatch},
		// A tag run reports the tag as head_branch; the guest claims a branch of that name.
		"tag as branch": {func(c *authorityCase) {
			c.run["head_branch"] = "v15.0.0"
			c.setup.Context.Ref = value("refs/heads/v15.0.0")
			record := c.assignments.records[c.release.JobID]
			record.JobWorkflowRef = "BreadGenie/frappe/.github/workflows/server-tests.yml@refs/tags/v15.0.0"
			c.assignments.records[c.release.JobID] = record
		}, errCacheJobMismatch},
		"duplicate runner assignment": {func(c *authorityCase) {
			fixtureJob(c, "runway-cache-test-1104-cold")["runner_id"] = json.Number("219")
		}, errCacheJobMismatch},
		"invalid binding": {func(c *authorityCase) { c.binding.FirecrackerPID = 0 }, grant.ErrInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			c := newAuthorityCase(t)
			test.change(c)
			authority, _ := c.authority(t)
			_, err := c.resolveAndAuthorize(t, authority, 150*time.Millisecond)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestGitHubJobAuthorityRenewalRechecksTheSingleJob(t *testing.T) {
	test := newAuthorityCase(t)
	authority, fixture := test.authority(t)
	release, err := test.resolve(t, authority, time.Second)
	if err != nil || release.Confirmed.RESTJobID != 126704211772 || release.Confirmed.Source != "rest_jobs" {
		t.Fatal("REST confirmation lost the job id", release, err)
	}
	release.Confirmed.ConfirmedAt = time.Now().Add(-cacheConfirmationFresh - time.Second)
	if _, err := authority.authorize(t.Context(), test.binding, release, test.setup); err != nil {
		t.Fatal("running job refused at renewal", err)
	}
	if fixture.calls["/repos/"+fixtureRepository+"/actions/jobs/126704211772"] != 1 {
		t.Fatal("renewal did not read the single job", fixture.calls)
	}
	fixtureJob(test, "runway-cache-test-1104-warm")["status"] = "completed"
	authority, _ = test.authority(t)
	if _, err := authority.authorize(t.Context(), test.binding, release, test.setup); !errors.Is(err, errCacheJobCompleted) {
		t.Fatal("completed job renewed", err)
	}
}

// A stale scale-set confirmation whose record is gone must not renew on
// trust: it falls back to the run's REST jobs.
func TestGitHubJobAuthorityRenewalWithoutScaleSetRecordRechecksREST(t *testing.T) {
	test := newAuthorityCase(t)
	delete(test.assignments.records, test.release.JobID)
	test.assignments.runners[219] = cacheAssignment{Version: 1, RunnerID: 219, RunnerName: "runway-cache-test-1104-warm", JobID: test.release.JobID, WorkflowRunID: 41867135947, Repository: fixtureRepository, EventName: "workflow_dispatch", JobWorkflowRef: fixtureWorkflow, ObservedAt: time.Now()}
	authority, fixture := test.authority(t)
	release, err := test.resolve(t, authority, time.Second)
	if err != nil || release.Confirmed.Source != "scale_set_started" {
		t.Fatal("scale-set confirmation", release, err)
	}
	release.Confirmed.ConfirmedAt = time.Now().Add(-cacheConfirmationFresh - time.Second)
	if _, err := authority.authorize(t.Context(), test.binding, release, test.setup); err != nil || fixture.calls[fixtureJobsPath] != 0 {
		t.Fatal("running record should renew without REST", err, fixture.calls)
	}
	delete(test.assignments.runners, 219) // pruned or unreadable
	if _, err := authority.authorize(t.Context(), test.binding, release, test.setup); err != nil || fixture.calls[fixtureJobsPath] != 1 {
		t.Fatal("missing record did not fall back to REST", err, fixture.calls)
	}
	fixtureJob(test, "runway-cache-test-1104-warm")["status"] = "completed"
	authority, _ = test.authority(t)
	if _, err := authority.authorize(t.Context(), test.binding, release, test.setup); !errors.Is(err, errCacheJobCompleted) {
		t.Fatal("completed job renewed without a record", err)
	}
}

func TestGitHubJobAuthorityIsBoundedByItsDeadline(t *testing.T) {
	test := newAuthorityCase(t)
	authority, fixture := test.authority(t)
	fixture.stall = true
	started := time.Now()
	if _, err := test.resolveAndAuthorize(t, authority, 200*time.Millisecond); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stalled GitHub did not refuse by deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatal("GitHub call escaped its deadline", elapsed)
	}
}

func TestCacheGuestBindingIsStrict(t *testing.T) {
	test := newAuthorityCase(t)
	data := encodeFixture(t, test.binding)
	if binding, err := decodeCacheGuestBinding(data); err != nil || binding.RunnerID != 219 {
		t.Fatal("valid binding refused", err)
	}
	for name, bad := range map[string]string{
		"duplicate":   strings.Replace(string(data), `"runner_id":219`, `"runner_id":218,"runner_id":219`, 1),
		"missing":     strings.Replace(string(data), `"runner_id":219,`, ``, 1),
		"unknown":     strings.Replace(string(data), `"runner_id":219`, `"runner_id":219,"trusted":true`, 1),
		"relative":    strings.Replace(string(data), `"/run/runway-cache/vms/rc-warm/v.sock"`, `"v.sock"`, 1),
		"bad vm":      strings.Replace(string(data), `"vm_id":"rc-warm"`, `"vm_id":"../x"`, 1),
		"bad repo":    strings.Replace(string(data), `"BreadGenie/frappe"`, `"BreadGenie/frappe/x"`, 1),
		"host cid":    strings.Replace(string(data), `"guest_cid":3`, `"guest_cid":2`, 1),
		"pid one":     strings.Replace(string(data), `"firecracker_pid":4242`, `"firecracker_pid":1`, 1),
		"null expiry": strings.Replace(string(data), `"expires_at":"`, `"expires_at":null,"x":"`, 1),
	} {
		if _, err := decodeCacheGuestBinding([]byte(bad)); err == nil {
			t.Fatalf("%s binding accepted", name)
		}
	}
}

func TestCacheCredentialAdmitsOnlyTheFixedJobPageQuery(t *testing.T) {
	client := &http.Client{Transport: cacheRepositoryCredential{token: "fixture-only", transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	for raw, allowed := range map[string]bool{
		"https://api.github.com" + fixtureJobsPath: true,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runs/41867135947/attempts/1/jobs?per_page=100&page=10":              true,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runs/41867135947/attempts/1/jobs":                                   false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runs/41867135947/attempts/1/jobs?per_page=100&page=0":               false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runs/41867135947/attempts/1/jobs?per_page=100&page=1&filter=latest": false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runs/41867135947/attempts/01/jobs?per_page=100&page=1":              false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runs/41867135947?per_page=100&page=1":                               false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/runners":                                                            false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/jobs/126704211772":                                                  true,
		"https://api.github.com/repos/BreadGenie/frappe/actions/jobs/0126704211772":                                                 false,
		"https://api.github.com/repos/BreadGenie/frappe/actions/jobs/126704211772?filter=x":                                         false,
	} {
		request, _ := http.NewRequest(http.MethodGet, raw, nil)
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
		}
		if (err == nil) != allowed {
			t.Fatalf("%s allowed=%v err=%v", raw, allowed, err)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
