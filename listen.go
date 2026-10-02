package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

// receipt is a single-job admission record, not a fleet reconciliation model.
// Record intent before AcquireJobs: after an uncertain response, resume only
// that same request, never acquire another job to replace it.
type receipt struct {
	RepositoryURL string    `json:"repository_url"`
	ScaleSetID    int       `json:"scale_set_id"`
	RunnerName    string    `json:"runner_name"`
	RunnerID      int       `json:"runner_id,omitempty"`
	RequestID     int64     `json:"request_id,omitempty"`
	JobID         string    `json:"job_id,omitempty"`
	WorkflowRunID int64     `json:"workflow_run_id,omitempty"`
	Acquired      bool      `json:"acquired"`
	Assigned      bool      `json:"assigned"`
	RawResult     string    `json:"raw_result,omitempty"`
	Started       bool      `json:"started"`
	Completed     bool      `json:"completed"`
	Result        string    `json:"result,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type jobAcquirer interface {
	AcquireJobs(context.Context, []int64) ([]int64, error)
}

type oneJob struct {
	state  receipt
	client jobAcquirer
	save   func(receipt) error
	stop   context.CancelFunc
}

func (s *oneJob) persist() error {
	s.state.UpdatedAt = time.Now().UTC()
	return s.save(s.state)
}

func (s *oneJob) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if s.state.Completed {
		s.stop()
		return nil
	}
	if msg != nil {
		fmt.Fprintf(os.Stderr, "scale-set message id=%d available=%d assigned=%d started=%d completed=%d\n", msg.MessageID, len(msg.JobAvailableMessages), len(msg.JobAssignedMessages), len(msg.JobStartedMessages), len(msg.JobCompletedMessages))
		for _, job := range msg.JobAvailableMessages {
			logJobIdentity("available", "", 0, job.RunnerRequestID, job.JobID, "")
		}
		for _, job := range msg.JobAssignedMessages {
			logJobIdentity("assigned", "", 0, job.RunnerRequestID, job.JobID, "")
		}
		for _, job := range msg.JobStartedMessages {
			logJobIdentity("started", job.RunnerName, job.RunnerID, job.RunnerRequestID, job.JobID, "")
			matched, err := s.bindRunnerEvent(job.RunnerName, job.RunnerID, job.RunnerRequestID, job.JobID)
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			s.state.Started = true
			s.state.RunnerID = job.RunnerID
			s.state.Assigned = true
			s.state.JobID = job.JobID
			s.state.WorkflowRunID = job.WorkflowRunID
			if err := s.persist(); err != nil {
				return err
			}
		}
		for _, job := range msg.JobCompletedMessages {
			logJobIdentity("completed", job.RunnerName, job.RunnerID, job.RunnerRequestID, job.JobID, job.Result)
			matched, err := s.bindRunnerEvent(job.RunnerName, job.RunnerID, job.RunnerRequestID, job.JobID)
			// A reserved assignment may be canceled before selecting a runner.
			canceledBeforeStart := job.RunnerName == "" && job.Result == "canceled" && s.state.RequestID > 0 && job.RunnerRequestID == s.state.RequestID
			if err != nil && !canceledBeforeStart {
				return err
			}
			if !matched && !canceledBeforeStart {
				continue
			}
			s.state.Completed = true
			s.state.RunnerID = job.RunnerID
			s.state.RawResult = job.Result
			s.state.Result = normalizeResult(job.Result)
			s.state.JobID = job.JobID
			s.state.WorkflowRunID = job.WorkflowRunID
			if err := s.persist(); err != nil {
				return err
			}
			// The official listener acknowledges after Scale returns nil even
			// when its context has been canceled. This persists completion first.
			s.stop()
			return nil
		}
		if s.state.RequestID == 0 && !s.state.Assigned && len(msg.JobAvailableMessages) > 0 {
			job := msg.JobAvailableMessages[0]
			if job.RunnerRequestID <= 0 {
				return errors.New("invalid available job request ID")
			}
			s.state.RequestID = job.RunnerRequestID
			s.state.JobID = job.JobID
			s.state.WorkflowRunID = job.WorkflowRunID
			if err := s.persist(); err != nil {
				return err
			}
		}
	}
	if s.state.RequestID != 0 && !s.state.Acquired && !s.state.Assigned {
		ids, err := s.client.AcquireJobs(ctx, []int64{s.state.RequestID})
		if err != nil {
			return errors.New("job acquisition failed; resume with the same journal")
		}
		found := false
		for _, id := range ids {
			if id == s.state.RequestID {
				found = true
			}
		}
		if !found {
			// An empty result does not prove whether a prior uncertain request
			// acquired this job. Retain its reservation and keep polling for
			// start/completion; never spend this slot on another request.
			return nil
		}
		s.state.Acquired = true
		if err := s.persist(); err != nil {
			return err
		}
	}
	return nil
}

// Match the prepared ephemeral runner and stable job UUID. Live GitHub events
// legitimately used request ID zero, even when no runner existed at dispatch.
func (s *oneJob) bindRunnerEvent(name string, runnerID int, requestID int64, jobID string) (bool, error) {
	if name != s.state.RunnerName {
		if s.state.RequestID > 0 && requestID == s.state.RequestID {
			return false, errors.New("event runner differs from reserved runner")
		}
		return false, nil
	}
	if s.state.RunnerID > 0 && runnerID != s.state.RunnerID {
		return false, errors.New("runner ID changed within one lease")
	}
	if s.state.JobID != "" && jobID != s.state.JobID {
		return false, errors.New("job UUID changed within one lease")
	}
	if s.state.RequestID > 0 && requestID > 0 && requestID != s.state.RequestID {
		return false, errors.New("request ID changed within one lease")
	}
	if requestID <= 0 {
		if _, err := uuid.Parse(jobID); err != nil {
			return false, errors.New("zero-request event requires valid stable job UUID")
		}
	}
	if s.state.RequestID == 0 {
		s.state.RequestID = requestID
	}
	s.state.JobID = jobID
	s.state.RunnerID = runnerID
	s.state.Assigned = true
	return true, nil
}

func normalizeResult(result string) string {
	switch result {
	case "succeeded":
		return "success"
	case "failed":
		return "failure"
	default:
		return result
	}
}

// Restrict diagnostics to identifiers, not arbitrary event bodies or credentials.
func logJobIdentity(kind, runner string, runnerID int, requestID int64, jobID, result string) {
	json.NewEncoder(os.Stderr).Encode(map[string]any{"event": kind, "runner_name": runner, "runner_id": runnerID, "request_id": requestID, "job_id": jobID, "result": result})
}

func listenOne(ctx context.Context, client *scaleset.Client, url string, id int, name, path string) error {
	initial := receipt{RepositoryURL: url, ScaleSetID: id, RunnerName: name}
	state, save, closeJournal, err := openJournal(path, initial)
	if err != nil {
		return err
	}
	defer closeJournal()
	if state.Completed {
		return printReceipt(state)
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	session, err := client.MessageSessionClient(ctx, id, "runway-"+host)
	if err != nil {
		return errors.New("cannot open GitHub scale-set message session")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := session.Close(cleanupCtx); err != nil {
			fmt.Fprintln(os.Stderr, "message session cleanup failed; verify before restart")
		}
	}()
	poller, err := listener.New(session, listener.Config{ScaleSetID: id, MaxRunners: 1})
	if err != nil {
		return err
	}
	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	scaler := &oneJob{state: state, client: session, save: save, stop: cancel}
	fmt.Fprintln(os.Stderr, "listening with capacity one; journal:", path)
	err = poller.Run(listenCtx, scaler)
	if err != nil && !(scaler.state.Completed && errors.Is(err, context.Canceled)) {
		// Raw SDK error text can include API response bodies. The durable
		// journal and GitHub UI are the recovery evidence for this spike.
		return errors.New("listener stopped before confirmed completion/acknowledgment; retain journal and inspect GitHub; VM still requires cleanup")
	}
	return printReceipt(scaler.state)
}

func printReceipt(state receipt) error {
	if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
		return err
	}
	return completionError(state)
}

func completionError(state receipt) error {
	if !state.Completed || state.Result != "success" {
		return errors.New("assignment did not complete successfully; inspect receipt and GitHub workflow; cleanup is still required")
	}
	return nil
}

func openJournal(path string, initial receipt) (receipt, func(receipt) error, func(), error) {
	directory := filepath.Dir(path)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return receipt{}, nil, nil, errors.New("journal parent must be an existing private directory (mode 0700)")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return receipt{}, nil, nil, err
	}
	closeLock := func() { lock.Close() }
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeLock()
		return receipt{}, nil, nil, errors.New("another listener owns this journal")
	}
	fail := func(err error) (receipt, func(receipt) error, func(), error) {
		closeLock()
		return receipt{}, nil, nil, err
	}
	state := initial
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fail(errors.New("journal must be a private regular file"))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(data, &state); err != nil {
			return fail(errors.New("journal is invalid; do not discard it while resources may exist"))
		}
		if state.RepositoryURL != initial.RepositoryURL || state.ScaleSetID != initial.ScaleSetID || state.RunnerName != initial.RunnerName {
			return fail(errors.New("journal identity differs from selected repository, scale set, or runner"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	save := func(state receipt) error {
		file, err := os.CreateTemp(directory, ".runway-journal-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		defer file.Close()
		if err := json.NewEncoder(file).Encode(state); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(file.Name(), path); err != nil {
			return err
		}
		dir, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	if err := save(state); err != nil {
		return fail(err)
	}
	return state, save, closeLock, nil
}
