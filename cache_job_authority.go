package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BreadGenie/runway-controller/cache/grant"
)

type cacheJobAuthority interface {
	resolve(context.Context, cacheGuestBinding, int64) (cacheJobRelease, error)
	authorize(context.Context, cacheGuestBinding, cacheJobRelease, grant.SetupRequest) (cacheJobGrant, error)
}

// cacheJobGrant is verified input to grant.IssueSetup.
type cacheJobGrant struct {
	Authorization grant.Authorization
	Policy        grant.RepositoryPolicy
}

// cacheGuestBinding is written only by the trusted root launcher after it
// registers the guest's JIT runner. The guest never supplies or reads it.
type cacheGuestBinding struct {
	service        bool
	TenantID       uint32    `json:"-"`
	Version        int       `json:"version"`
	VMID           string    `json:"vm_id"`
	VsockUDS       string    `json:"vsock_uds"`
	FirecrackerPID int       `json:"firecracker_pid"`
	JailUID        uint32    `json:"jail_uid"`
	JailGID        uint32    `json:"jail_gid"`
	GuestCID       uint32    `json:"guest_cid"`
	RunnerID       int64     `json:"runner_id"`
	RunnerName     string    `json:"runner_name"`
	Repository     string    `json:"repository"`
	ExpiresAt      time.Time `json:"expires_at"`
}

var cacheVMIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (binding cacheGuestBinding) validate() error {
	if binding.service {
		parts := strings.Split(binding.Repository, "/")
		if binding.TenantID == 0 || binding.VMID == "" || binding.RunnerID <= 0 || binding.RunnerName == "" || len(parts) != 2 || !validCacheGitHubPart(parts[0]) || !validCacheGitHubPart(parts[1]) || binding.ExpiresAt.IsZero() {
			return grant.ErrInvalid
		}
		return nil
	}
	parts := strings.Split(binding.Repository, "/")
	if binding.Version != 2 || !cacheVMIDPattern.MatchString(binding.VMID) || !filepath.IsAbs(binding.VsockUDS) || filepath.Clean(binding.VsockUDS) != binding.VsockUDS || binding.FirecrackerPID <= 1 || binding.GuestCID < 3 || binding.RunnerID <= 0 || binding.RunnerName == "" || len(binding.RunnerName) > 255 || strings.ContainsAny(binding.RunnerName, "\x00\r\n\t ") || len(parts) != 2 || !validCacheGitHubPart(parts[0]) || !validCacheGitHubPart(parts[1]) || binding.ExpiresAt.IsZero() {
		return grant.ErrInvalid
	}
	return nil
}

func decodeCacheGuestBinding(data []byte) (cacheGuestBinding, error) {
	var binding cacheGuestBinding
	if readCacheObject(data, []string{"version", "vm_id", "vsock_uds", "firecracker_pid", "jail_uid", "jail_gid", "guest_cid", "runner_id", "runner_name", "repository", "expires_at"}) != nil || json.Unmarshal(data, &binding) != nil || binding.validate() != nil {
		return cacheGuestBinding{}, grant.ErrInvalid
	}
	return binding, nil
}

// cacheBindingSource finds the binding for the VM whose vsock socket a request
// arrived on. errCacheUnbound means no binding exists.
type cacheBindingSource interface {
	lookup(vmID string) (cacheGuestBinding, error)
}

var errCacheUnbound = errors.New("cache guest has no launcher binding")

// cacheBindingFiles reads <dir>/<vm-id>.json. The directory and file must be
// root-owned and private, so only host root can create or change a binding.
type cacheBindingFiles struct{ dir string }

func (files cacheBindingFiles) lookup(vmID string) (cacheGuestBinding, error) {
	if !cacheVMIDPattern.MatchString(vmID) || !filepath.IsAbs(files.dir) {
		return cacheGuestBinding{}, grant.ErrInvalid
	}
	if err := checkCachePrivateDirectory(files.dir); err != nil {
		return cacheGuestBinding{}, err
	}
	path := filepath.Join(files.dir, vmID+".json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return cacheGuestBinding{}, errCacheUnbound
	}
	data, err := readProtectedCacheFile(path, true)
	if err != nil || len(data) > 4096 {
		return cacheGuestBinding{}, grant.ErrInvalid
	}
	binding, err := decodeCacheGuestBinding(data)
	if err != nil || binding.VMID != vmID {
		return cacheGuestBinding{}, grant.ErrInvalid
	}
	return binding, nil
}

func checkCachePrivateDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return grant.ErrInvalid
	}
	if metadata, ok := info.Sys().(*syscall.Stat_t); !ok || metadata.Uid != 0 {
		return grant.ErrInvalid
	}
	return nil
}

// cacheJobRelease is the job the host resolved for the bound runner (its UUID
// and request ID), the binding's stop, and the host-held source that proved it.
// Nothing in it comes from the guest.
type cacheJobRelease struct {
	JobID     string
	RequestID int64
	Stop      time.Time
	Confirmed *cacheJobConfirmation
}

// cacheJobConfirmation records which host-held source proved the assignment.
// WorkflowRef is the scale-set JobWorkflowRef ("<repo>/<path>@<ref>").
type cacheJobConfirmation struct {
	RunID, RunAttempt, RESTJobID int64
	WorkflowRef                  string
	StartedAt, ConfirmedAt       time.Time
	Source                       string // "scale_set_started" or "rest_jobs"
}

// githubCacheJobAuthority resolves the bound runner's job from two host-held
// sources, whichever answers first:
//   - the scale-set session's JobStarted record for the runner ID (job UUID,
//     request ID, run, workflow ref). It arrives about a minute into the job.
//   - the run's REST jobs list: the single in-progress job on the bound
//     runner_id/name, tied by display name to the one JobAssigned record of that
//     run. JobAssigned arrives at assignment, so early setups need this path.
//
// The guest's run ID only says which run to read; a job on another run cannot
// carry the bound runner's ID. authorize then requires the repository and the
// exact run and attempt (event, branch, SHA, same-repository head), and on
// renewal that the job is still in progress. It never trusts the guest when
// GitHub is slow.
type githubCacheJobAuthority struct {
	client      *http.Client
	assignments cacheAssignmentSource
	regionID    string
	interval    time.Duration // resolution poll interval
	now         func() time.Time
}

var (
	errCacheJobNotFound  = errors.New("bound runner's job assignment is not confirmed")
	errCacheJobCompleted = errors.New("bound runner's job is not in progress")
	errCacheJobMismatch  = errors.New("GitHub job does not match the binding or setup")
)

// cacheConfirmationFresh skips the still-running re-check right after confirmation.
const cacheConfirmationFresh = 2 * time.Minute

func (authority githubCacheJobAuthority) valid(binding cacheGuestBinding) bool {
	return authority.client != nil && authority.assignments != nil && authority.interval > 0 && authority.now != nil && authority.regionID != "" && binding.validate() == nil
}

// resolve polls until the context ends for the bound runner's job. runHint is
// the guest-reported run ID, used only to choose which run's jobs to read.
func (authority githubCacheJobAuthority) resolve(ctx context.Context, binding cacheGuestBinding, runHint int64) (cacheJobRelease, error) {
	if !authority.valid(binding) {
		return cacheJobRelease{}, grant.ErrInvalid
	}
	var attempt int64
	for {
		if started, err := authority.assignments.assignment(binding.RunnerID); err == nil {
			if started.RunnerName != binding.RunnerName || started.Repository != binding.Repository {
				return cacheJobRelease{}, errCacheJobMismatch
			}
			if started.Completed {
				return cacheJobRelease{}, errCacheJobCompleted
			}
			confirmed := cacheJobConfirmation{RunID: started.WorkflowRunID, WorkflowRef: started.JobWorkflowRef, StartedAt: started.ObservedAt, ConfirmedAt: authority.now(), Source: "scale_set_started"}
			return cacheJobRelease{JobID: started.JobID, RequestID: started.RequestID, Stop: binding.ExpiresAt, Confirmed: &confirmed}, nil
		} else if !errors.Is(err, errCacheJobNotFound) {
			return cacheJobRelease{}, err
		}
		if runHint > 0 {
			release, err := authority.resolveREST(ctx, binding, runHint, &attempt)
			if err == nil {
				return release, nil
			}
			if !errors.Is(err, errCacheJobNotFound) {
				return cacheJobRelease{}, err
			}
		}
		timer := time.NewTimer(authority.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cacheJobRelease{}, errors.Join(errCacheJobNotFound, ctx.Err())
		case <-timer.C:
		}
	}
}

// resolveREST maps the bound runner's in-progress REST job to its job UUID
// through the run's JobAssigned records. A rerun attempt repeats display names,
// so more than one candidate refuses rather than guess.
func (authority githubCacheJobAuthority) resolveREST(ctx context.Context, binding cacheGuestBinding, runID int64, attempt *int64) (cacheJobRelease, error) {
	confirmed, job, err := authority.confirmREST(ctx, binding, runID, attempt)
	if err != nil {
		return cacheJobRelease{}, err
	}
	records, err := authority.assignments.jobs(runID)
	if err != nil {
		return cacheJobRelease{}, err
	}
	var matched []cacheJobRecord
	for _, record := range records {
		if record.Repository == binding.Repository && record.JobDisplayName != "" && record.JobDisplayName == job.Name {
			matched = append(matched, record)
		}
	}
	switch len(matched) {
	case 0:
		return cacheJobRelease{}, errCacheJobNotFound // JobAssigned not recorded yet
	case 1:
	default:
		return cacheJobRelease{}, errCacheJobMismatch
	}
	confirmed.WorkflowRef = matched[0].JobWorkflowRef
	return cacheJobRelease{JobID: matched[0].JobID, RequestID: matched[0].RequestID, Stop: binding.ExpiresAt, Confirmed: &confirmed}, nil
}

// confirmREST looks for the bound runner's in-progress job in the run's current
// attempt. A missing or still-queued job is errCacheJobNotFound (keep polling).
func (authority githubCacheJobAuthority) confirmREST(ctx context.Context, binding cacheGuestBinding, runID int64, attempt *int64, matchName ...bool) (cacheJobConfirmation, cacheGitHubJob, error) {
	if *attempt == 0 {
		data, err := readCacheGitHubMetadata(ctx, authority.client, "/repos/"+binding.Repository+"/actions/runs/"+strconv.FormatInt(runID, 10))
		if err != nil {
			return cacheJobConfirmation{}, cacheGitHubJob{}, errors.Join(errCacheJobNotFound, err)
		}
		var run struct {
			RunAttempt int64 `json:"run_attempt"`
		}
		if json.Unmarshal(data, &run) != nil || run.RunAttempt <= 0 {
			return cacheJobConfirmation{}, cacheGitHubJob{}, errCacheRepository
		}
		*attempt = run.RunAttempt
	}
	var names []string
	if len(matchName) > 0 && matchName[0] {
		names = []string{binding.RunnerName}
	}
	job, err := readCacheBoundJob(ctx, authority.client, binding.Repository, runID, *attempt, binding.RunnerID, names...)
	switch {
	case errors.Is(err, errCacheJobNotFound), errors.Is(err, errCacheJobMismatch):
		return cacheJobConfirmation{}, cacheGitHubJob{}, err
	case err != nil:
		return cacheJobConfirmation{}, cacheGitHubJob{}, errors.Join(errCacheJobNotFound, err) // transient: keep polling
	case cacheJobPending(job.Status):
		return cacheJobConfirmation{}, cacheGitHubJob{}, errCacheJobNotFound
	case job.Status != "in_progress":
		return cacheJobConfirmation{}, cacheGitHubJob{}, errCacheJobCompleted
	case job.RunnerName != binding.RunnerName || job.RunID != runID || job.RunAttempt != *attempt || job.StartedAt.IsZero():
		return cacheJobConfirmation{}, cacheGitHubJob{}, errCacheJobMismatch
	}
	return cacheJobConfirmation{RunID: runID, RunAttempt: *attempt, RESTJobID: job.ID, StartedAt: job.StartedAt, ConfirmedAt: authority.now(), Source: "rest_jobs"}, job, nil
}

func (authority githubCacheJobAuthority) authorize(ctx context.Context, binding cacheGuestBinding, release cacheJobRelease, setup grant.SetupRequest) (cacheJobGrant, error) {
	if !authority.valid(binding) {
		return cacheJobGrant{}, grant.ErrInvalid
	}
	// resolve supplies job identity; setup supplies only untrusted context.
	if !canonicalCacheUUID(release.JobID) || release.Stop.IsZero() {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	confirmed := release.Confirmed
	if confirmed == nil {
		return cacheJobGrant{}, errCacheJobNotFound
	}
	c := setup.Context
	if c.Repository == nil || *c.Repository != binding.Repository || c.RunID == nil || c.RunAttempt == nil || !cacheCanonicalPositive(*c.RunID) || !cacheCanonicalPositive(*c.RunAttempt) || c.Ref == nil || c.SHA == nil {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	// For a tag run GitHub reports the tag name as head_branch, so the run alone
	// cannot tell refs/tags/v1 from refs/heads/v1. The host-held workflow ref can.
	if !strings.HasSuffix(confirmed.WorkflowRef, "@"+*c.Ref) {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	runID, _ := strconv.ParseInt(*c.RunID, 10, 64)
	attempt, _ := strconv.ParseInt(*c.RunAttempt, 10, 64)
	if runID != confirmed.RunID || (confirmed.RunAttempt != 0 && attempt != confirmed.RunAttempt) {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	policy, err := readCacheRepositoryPolicy(ctx, authority.client, binding.Repository)
	if err != nil {
		return cacheJobGrant{}, err
	}
	// Run event, branch, SHA, attempt and same-repository head (no fork) must
	// equal the guest context; PR, tag and fork runs are refused here.
	if err := verifyCacheWorkflowRun(ctx, authority.client, policy, runID, c); err != nil {
		return cacheJobGrant{}, err
	}
	if authority.now().Sub(confirmed.ConfirmedAt) > cacheConfirmationFresh {
		if err := authority.stillRunning(ctx, binding, *confirmed, attempt); err != nil {
			return cacheJobGrant{}, err
		}
	}
	deadline := release.Stop
	if binding.ExpiresAt.Before(deadline) {
		deadline = binding.ExpiresAt
	}
	authorization := grant.Authorization{
		TenantID: int64(binding.TenantID), RegionID: authority.regionID, JobID: release.JobID, RequestID: release.RequestID,
		RunID: runID, RunAttempt: attempt, Deadline: deadline,
	}
	return cacheJobGrant{Authorization: authorization, Policy: policy}, nil
}

// stillRunning re-checks a stale confirmation before a renewed grant. A
// completion record refuses. A REST confirmation re-reads its single job. A
// scale-set confirmation trusts its running record, and falls back to the
// run's REST jobs when that record is missing or unreadable.
func (authority githubCacheJobAuthority) stillRunning(ctx context.Context, binding cacheGuestBinding, confirmed cacheJobConfirmation, attempt int64) error {
	started, recordErr := authority.assignments.assignment(binding.RunnerID)
	if recordErr == nil && started.Completed {
		return errCacheJobCompleted
	}
	var job cacheGitHubJob
	var err error
	switch {
	case confirmed.RESTJobID > 0:
		var data []byte
		if data, err = readCacheGitHubMetadata(ctx, authority.client, "/repos/"+binding.Repository+"/actions/jobs/"+strconv.FormatInt(confirmed.RESTJobID, 10)); err == nil {
			job, err = decodeCacheGitHubJob(data)
		}
	case recordErr == nil:
		return nil
	default:
		job, err = readCacheBoundJob(ctx, authority.client, binding.Repository, confirmed.RunID, attempt, binding.RunnerID)
	}
	if err != nil {
		return err
	}
	if job.Status != "in_progress" || job.RunnerID != binding.RunnerID || job.RunnerName != binding.RunnerName || job.RunID != confirmed.RunID {
		return errCacheJobCompleted
	}
	return nil
}

type cacheGitHubJob struct {
	ID, RunID, RunAttempt, RunnerID int64
	Name, Status, RunnerName        string
	StartedAt                       time.Time
}

// cacheNullable is a GitHub field that must be present but may be null.
type cacheNullable[T any] struct {
	Present bool
	Value   *T
}

func (field *cacheNullable[T]) UnmarshalJSON(data []byte) error {
	field.Present = true
	return json.Unmarshal(data, &field.Value)
}

type cacheGitHubJobWire struct {
	ID         int64                    `json:"id"`
	Name       string                   `json:"name"`
	RunID      int64                    `json:"run_id"`
	RunAttempt int64                    `json:"run_attempt"`
	Status     string                   `json:"status"`
	RunnerID   cacheNullable[int64]     `json:"runner_id"`
	RunnerName cacheNullable[string]    `json:"runner_name"`
	StartedAt  cacheNullable[time.Time] `json:"started_at"`
}

// job requires id, run, attempt and status, and the runner and start fields to
// be present; they are null while a job is queued.
func (wire cacheGitHubJobWire) job() (cacheGitHubJob, error) {
	if wire.ID <= 0 || wire.RunID <= 0 || wire.RunAttempt <= 0 || wire.Status == "" || !wire.RunnerID.Present || !wire.RunnerName.Present || !wire.StartedAt.Present {
		return cacheGitHubJob{}, errCacheRepository
	}
	job := cacheGitHubJob{ID: wire.ID, Name: wire.Name, RunID: wire.RunID, RunAttempt: wire.RunAttempt, Status: wire.Status}
	if wire.RunnerID.Value != nil {
		job.RunnerID = *wire.RunnerID.Value
	}
	if wire.RunnerName.Value != nil {
		job.RunnerName = *wire.RunnerName.Value
	}
	if wire.StartedAt.Value != nil {
		job.StartedAt = *wire.StartedAt.Value
	}
	return job, nil
}

// decodeCacheGitHubJob decodes one /actions/jobs/{id} response.
func decodeCacheGitHubJob(data []byte) (cacheGitHubJob, error) {
	var wire cacheGitHubJobWire
	if json.Unmarshal(data, &wire) != nil {
		return cacheGitHubJob{}, errCacheRepository
	}
	return wire.job()
}

// cacheJobPending reports statuses GitHub shows before a job starts running.
func cacheJobPending(status string) bool {
	return status == "queued" || status == "waiting" || status == "pending" || status == "requested"
}

// readCacheBoundJob pages through one attempt's jobs (at most 1000) and returns
// the single job assigned to runnerID, or errCacheJobNotFound.
func readCacheBoundJob(ctx context.Context, client *http.Client, repository string, runID, attempt, runnerID int64, runnerName ...string) (cacheGitHubJob, error) {
	var found []cacheGitHubJob
	seen := 0
	for page := 1; page <= 10; page++ {
		path := "/repos/" + repository + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/attempts/" + strconv.FormatInt(attempt, 10) + "/jobs?per_page=100&page=" + strconv.Itoa(page)
		data, err := readCacheGitHubBounded(ctx, client, path, 1<<20)
		if err != nil {
			return cacheGitHubJob{}, err
		}
		total, jobs, err := decodeCacheGitHubJobPage(data)
		if err != nil {
			return cacheGitHubJob{}, err
		}
		for _, job := range jobs {
			matches := job.RunnerID == runnerID
			if len(runnerName) > 0 {
				matches = job.Status == "in_progress" && job.RunnerName == runnerName[0] && (job.RunnerID == 0 || job.RunnerID == runnerID)
			}
			if matches {
				found = append(found, job)
			}
		}
		seen += len(jobs)
		if len(jobs) < 100 || seen >= total {
			break
		}
		if page == 10 {
			return cacheGitHubJob{}, errCacheRepository
		}
	}
	switch len(found) {
	case 0:
		return cacheGitHubJob{}, errCacheJobNotFound
	case 1:
		return found[0], nil
	default:
		return cacheGitHubJob{}, errCacheJobMismatch
	}
}

func decodeCacheGitHubJobPage(data []byte) (int, []cacheGitHubJob, error) {
	var page struct {
		TotalCount *int                  `json:"total_count"`
		Jobs       *[]cacheGitHubJobWire `json:"jobs"`
	}
	if json.Unmarshal(data, &page) != nil || page.TotalCount == nil || *page.TotalCount < 0 || page.Jobs == nil || len(*page.Jobs) > 100 {
		return 0, nil, errCacheRepository
	}
	jobs := make([]cacheGitHubJob, 0, len(*page.Jobs))
	for _, wire := range *page.Jobs {
		job, err := wire.job()
		if err != nil {
			return 0, nil, err
		}
		jobs = append(jobs, job)
	}
	return *page.TotalCount, jobs, nil
}
