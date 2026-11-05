package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// The assignment observer holds the host's own runner scale-set message session
// (host credential, Actions service) for the scale set whose JIT runners the
// launcher registers. The service pushes JobStarted when a registered runner
// takes a job; that message names the runner ID and name, the job UUID, the
// workflow run, the repository and the event. The observer writes one
// root-only record per runner. cache-host reads these records as the
// assignment authority instead of GitHub's lagging REST job view. Nothing in a
// record comes from a guest.
type cacheAssignmentsConfig struct {
	Version         int    `json:"version"`
	RepositoryURL   string `json:"repository_url"`
	ScaleSetID      int    `json:"scale_set_id"`
	GitHubTokenFile string `json:"github_token_file"`
	AssignmentDir   string `json:"assignment_dir"`
	MaxRunners      int    `json:"max_runners"`
}

func decodeCacheAssignmentsConfig(data []byte) (cacheAssignmentsConfig, error) {
	var config cacheAssignmentsConfig
	if readCacheObject(data, []string{"version", "repository_url", "scale_set_id", "github_token_file", "assignment_dir", "max_runners"}) != nil || json.Unmarshal(data, &config) != nil {
		return cacheAssignmentsConfig{}, grant.ErrInvalid
	}
	if config.Version != 1 || !repositoryURL.MatchString(config.RepositoryURL) || config.ScaleSetID <= 0 || config.MaxRunners < 1 || config.MaxRunners > 64 || !filepath.IsAbs(config.GitHubTokenFile) || !filepath.IsAbs(config.AssignmentDir) || filepath.Clean(config.AssignmentDir) != config.AssignmentDir {
		return cacheAssignmentsConfig{}, grant.ErrInvalid
	}
	return config, nil
}

// cacheAssignment is one JobStarted (and, later, JobCompleted) observation.
// JobWorkflowRef is "<repo>/<path>@<ref>": the ref GitHub runs the job at, so
// a tag run cannot pass for a branch of the same name.
type cacheAssignment struct {
	Version        int       `json:"version"`
	RunnerID       int64     `json:"runner_id"`
	RunnerName     string    `json:"runner_name"`
	JobID          string    `json:"job_id"`
	RequestID      int64     `json:"request_id"`
	WorkflowRunID  int64     `json:"workflow_run_id"`
	Repository     string    `json:"repository"`
	EventName      string    `json:"event_name"`
	JobWorkflowRef string    `json:"job_workflow_ref"`
	AssignedAt     time.Time `json:"assigned_at"`
	ObservedAt     time.Time `json:"observed_at"`
	Completed      bool      `json:"completed"`
}

var cacheAssignmentFields = []string{"version", "runner_id", "runner_name", "job_id", "request_id", "workflow_run_id", "repository", "event_name", "job_workflow_ref", "assigned_at", "observed_at", "completed"}

func (record cacheAssignment) validate() error {
	parts := strings.Split(record.Repository, "/")
	if record.Version != 1 || record.RunnerID <= 0 || record.RunnerName == "" || len(record.RunnerName) > 255 || !canonicalCacheUUID(record.JobID) || record.RequestID < 0 || record.WorkflowRunID <= 0 || len(parts) != 2 || !validCacheGitHubPart(parts[0]) || !validCacheGitHubPart(parts[1]) || record.EventName == "" || !strings.Contains(record.JobWorkflowRef, "@refs/") || len(record.JobWorkflowRef) > 2048 || record.ObservedAt.IsZero() {
		return grant.ErrInvalid
	}
	return nil
}

func decodeCacheAssignment(data []byte) (cacheAssignment, error) {
	var record cacheAssignment
	if readCacheObject(data, cacheAssignmentFields) != nil || json.Unmarshal(data, &record) != nil || record.validate() != nil {
		return cacheAssignment{}, grant.ErrInvalid
	}
	return record, nil
}

// cacheJobRecord is the scale-set view of one job (JobAssigned/JobStarted):
// which run, repository and workflow ref its UUID belongs to. It names no runner.
type cacheJobRecord struct {
	Version        int       `json:"version"`
	JobID          string    `json:"job_id"`
	RequestID      int64     `json:"request_id"`
	WorkflowRunID  int64     `json:"workflow_run_id"`
	Repository     string    `json:"repository"`
	EventName      string    `json:"event_name"`
	JobWorkflowRef string    `json:"job_workflow_ref"`
	ObservedAt     time.Time `json:"observed_at"`
}

func (record cacheJobRecord) validate() error {
	return cacheAssignment{Version: record.Version, RunnerID: 1, RunnerName: "-", JobID: record.JobID, RequestID: record.RequestID, WorkflowRunID: record.WorkflowRunID, Repository: record.Repository, EventName: record.EventName, JobWorkflowRef: record.JobWorkflowRef, ObservedAt: record.ObservedAt}.validate()
}

// cacheAssignmentSource returns host-observed scale-set records, or
// errCacheJobNotFound when none has been observed yet.
type cacheAssignmentSource interface {
	assignment(runnerID int64) (cacheAssignment, error)
	job(jobID string) (cacheJobRecord, error)
}

func (files cacheAssignmentFiles) job(jobID string) (cacheJobRecord, error) {
	if !canonicalCacheUUID(jobID) || checkCacheOwnedDirectory(files.dir) != nil {
		return cacheJobRecord{}, grant.ErrInvalid
	}
	path := filepath.Join(files.dir, "job-"+jobID+".json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return cacheJobRecord{}, errCacheJobNotFound
	}
	data, err := readCacheOwnedFile(path)
	var record cacheJobRecord
	if err != nil || readCacheObject(data, []string{"version", "job_id", "request_id", "workflow_run_id", "repository", "event_name", "job_workflow_ref", "observed_at"}) != nil || json.Unmarshal(data, &record) != nil || record.validate() != nil || record.JobID != jobID {
		return cacheJobRecord{}, grant.ErrInvalid
	}
	return record, nil
}

type cacheAssignmentFiles struct{ dir string }

func (files cacheAssignmentFiles) assignment(runnerID int64) (cacheAssignment, error) {
	if runnerID <= 0 || checkCacheOwnedDirectory(files.dir) != nil {
		return cacheAssignment{}, grant.ErrInvalid
	}
	path := filepath.Join(files.dir, strconv.FormatInt(runnerID, 10)+".json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return cacheAssignment{}, errCacheJobNotFound
	}
	data, err := readCacheOwnedFile(path)
	if err != nil {
		return cacheAssignment{}, grant.ErrInvalid
	}
	record, err := decodeCacheAssignment(data)
	if err != nil || record.RunnerID != runnerID {
		return cacheAssignment{}, grant.ErrInvalid
	}
	return record, nil
}

// Records are written and read by root services (the observer and cache-host);
// both run as the same user, so ownership is checked against this process.
func checkCacheOwnedDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return grant.ErrInvalid
	}
	if metadata, ok := info.Sys().(*syscall.Stat_t); !ok || int(metadata.Uid) != os.Geteuid() {
		return grant.ErrInvalid
	}
	return nil
}

func readCacheOwnedFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, grant.ErrInvalid
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, grant.ErrInvalid
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || int(metadata.Uid) != os.Geteuid() || metadata.Nlink != 1 || info.Size() > 4096 {
		return nil, grant.ErrInvalid
	}
	return io.ReadAll(io.LimitReader(file, 4097))
}

func writeCacheAssignment(dir string, record cacheAssignment) error {
	if record.validate() != nil {
		return grant.ErrInvalid
	}
	return writeCacheRecord(filepath.Join(dir, strconv.FormatInt(record.RunnerID, 10)+".json"), record)
}

func writeCacheRecord(path string, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path + ".tmp")
		return err
	}
	return os.Rename(path+".tmp", path)
}

// cacheAssignmentScaler acquires every job offered to the scale set (the
// launcher decides how many guests exist) and records JobStarted/JobCompleted.
type cacheAssignmentScaler struct {
	dir        string
	repository string
	client     jobAcquirer
	log        func(map[string]any)
	now        func() time.Time
}

func (scaler *cacheAssignmentScaler) Scale(ctx context.Context, message *scaleset.RunnerScaleSetMessage) error {
	if message == nil {
		return nil
	}
	received := scaler.now().UTC()
	var available []int64
	for _, job := range message.JobAvailableMessages {
		scaler.log(map[string]any{"event": "job_available", "received_at": received, "request_id": job.RunnerRequestID, "job_id": job.JobID, "run_id": job.WorkflowRunID})
		if job.RunnerRequestID > 0 {
			available = append(available, job.RunnerRequestID)
		}
	}
	for _, job := range message.JobAssignedMessages {
		err := scaler.recordJob(job.JobMessageBase, received)
		scaler.log(map[string]any{"event": "job_assigned", "received_at": received, "request_id": job.RunnerRequestID, "job_id": job.JobID, "run_id": job.WorkflowRunID, "job_workflow_ref": job.JobWorkflowRef, "recorded": err == nil})
	}
	for _, job := range message.JobStartedMessages {
		scaler.recordJob(job.JobMessageBase, received)
		err := scaler.record(scaler.assignment(job.JobMessageBase, int64(job.RunnerID), job.RunnerName, received, false))
		scaler.log(map[string]any{"event": "job_started", "received_at": received, "runner_assign_time": job.RunnerAssignTime, "runner_id": job.RunnerID, "runner_name": job.RunnerName, "job_id": job.JobID, "run_id": job.WorkflowRunID, "recorded": err == nil})
	}
	for _, job := range message.JobCompletedMessages {
		err := error(nil)
		if job.RunnerID > 0 {
			err = scaler.record(scaler.assignment(job.JobMessageBase, int64(job.RunnerID), job.RunnerName, received, true))
		}
		scaler.log(map[string]any{"event": "job_completed", "received_at": received, "runner_id": job.RunnerID, "job_id": job.JobID, "result": job.Result, "recorded": err == nil})
	}
	if len(available) > 0 {
		acquired, err := scaler.client.AcquireJobs(ctx, available)
		scaler.log(map[string]any{"event": "acquire", "requested": len(available), "acquired": len(acquired), "ok": err == nil})
		if err != nil {
			return errors.New("job acquisition failed")
		}
	}
	return nil
}

func (scaler *cacheAssignmentScaler) assignment(job scaleset.JobMessageBase, runnerID int64, runnerName string, received time.Time, completed bool) cacheAssignment {
	return cacheAssignment{Version: 1, RunnerID: runnerID, RunnerName: runnerName, JobID: job.JobID, RequestID: job.RunnerRequestID, WorkflowRunID: job.WorkflowRunID, Repository: job.OwnerName + "/" + job.RepositoryName, EventName: job.EventName, JobWorkflowRef: job.JobWorkflowRef, AssignedAt: job.RunnerAssignTime, ObservedAt: received, Completed: completed}
}

// recordJob writes the first observation of a job UUID; a later message may not
// move that UUID to another run, repository or workflow ref.
func (scaler *cacheAssignmentScaler) recordJob(job scaleset.JobMessageBase, received time.Time) error {
	record := cacheJobRecord{Version: 1, JobID: job.JobID, RequestID: job.RunnerRequestID, WorkflowRunID: job.WorkflowRunID, Repository: job.OwnerName + "/" + job.RepositoryName, EventName: job.EventName, JobWorkflowRef: job.JobWorkflowRef, ObservedAt: received}
	if record.Repository != scaler.repository || record.validate() != nil {
		return grant.ErrInvalid
	}
	if previous, err := (cacheAssignmentFiles{dir: scaler.dir}).job(job.JobID); err == nil {
		if previous.WorkflowRunID != record.WorkflowRunID || previous.Repository != record.Repository || previous.JobWorkflowRef != record.JobWorkflowRef || previous.RequestID != record.RequestID {
			return grant.ErrInvalid
		}
		return nil
	}
	return writeCacheRecord(filepath.Join(scaler.dir, "job-"+job.JobID+".json"), record)
}

// record refuses foreign repositories and never lets a later message change a
// runner's job: an ephemeral runner runs exactly one.
func (scaler *cacheAssignmentScaler) record(record cacheAssignment) error {
	if record.Repository != scaler.repository || record.validate() != nil {
		return grant.ErrInvalid
	}
	if previous, err := (cacheAssignmentFiles{dir: scaler.dir}).assignment(record.RunnerID); err == nil {
		if previous.JobID != record.JobID || previous.WorkflowRunID != record.WorkflowRunID || previous.RunnerName != record.RunnerName || previous.JobWorkflowRef != record.JobWorkflowRef {
			return grant.ErrInvalid
		}
		if previous.Completed && !record.Completed {
			return nil
		}
		if !record.Completed {
			record.ObservedAt = previous.ObservedAt
		}
	}
	return writeCacheAssignment(scaler.dir, record)
}

// cacheAssignmentRetention outlives any guest (the host unit's RuntimeMaxSec is 12 h).
const cacheAssignmentRetention = 24 * time.Hour

// pruneCacheAssignments removes records (and stray temporary files) older than maxAge.
func pruneCacheAssignments(dir string, now time.Time, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && strings.Contains(entry.Name(), ".json") && now.Sub(info.ModTime()) > maxAge {
			os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// runCacheAssignments holds the scale-set session until SIGINT/SIGTERM.
func runCacheAssignments(configPath string) error {
	data, err := readProtectedCacheFile(configPath, true)
	if err != nil {
		return errors.New("cache-assignments configuration unavailable")
	}
	config, err := decodeCacheAssignmentsConfig(data)
	if err != nil {
		return errors.New("cache-assignments configuration refused")
	}
	if err := checkCachePrivateDirectory(config.AssignmentDir); err != nil { // root-owned
		return errors.New("cache-assignments directory must be root-owned and private")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := cacheGitHubClient(config.RepositoryURL, config.GitHubTokenFile)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	session, err := client.MessageSessionClient(ctx, config.ScaleSetID, "runway-cache-"+host)
	if err != nil {
		return errors.New("cannot open GitHub scale-set message session")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		session.Close(cleanup)
	}()
	poller, err := listener.New(session, listener.Config{ScaleSetID: config.ScaleSetID, MaxRunners: config.MaxRunners})
	if err != nil {
		return err
	}
	repository := strings.TrimPrefix(config.RepositoryURL, "https://github.com/")
	scaler := &cacheAssignmentScaler{dir: config.AssignmentDir, repository: repository, client: session, log: cacheHostLogger(os.Stdout), now: time.Now}
	go func() {
		for {
			pruneCacheAssignments(config.AssignmentDir, time.Now(), cacheAssignmentRetention)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
	scaler.log(map[string]any{"event": "listening", "scale_set_id": config.ScaleSetID, "repository": repository})
	err = poller.Run(ctx, scaler)
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("cache-assignments listener stopped")
}
