package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

type fakeSession struct {
	calls     [][]int64
	acquire   func([]int64) ([]int64, error)
	messages  []*scaleset.RunnerScaleSetMessage
	acks      []int
	beforeAck func()
}

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.calls = append(f.calls, append([]int64{}, ids...))
	if f.acquire != nil {
		return f.acquire(ids)
	}
	return ids, nil
}
func (f *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}
func (f *fakeSession) GetMessage(_ context.Context, _ int, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	if capacity != 1 {
		return nil, errors.New("expected capacity one")
	}
	if len(f.messages) == 0 {
		return nil, errors.New("test queue exhausted")
	}
	m := f.messages[0]
	f.messages = f.messages[1:]
	return m, nil
}
func (f *fakeSession) DeleteMessage(_ context.Context, id int) error {
	if f.beforeAck != nil {
		f.beforeAck()
	}
	f.acks = append(f.acks, id)
	return nil
}

func available(ids ...int64) *scaleset.RunnerScaleSetMessage {
	m := &scaleset.RunnerScaleSetMessage{MessageID: 4}
	for _, id := range ids {
		m.JobAvailableMessages = append(m.JobAvailableMessages, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: id}})
	}
	return m
}

func TestOneJobPersistsBeforeAcquireAndIgnoresOtherDemand(t *testing.T) {
	var saved receipt
	client := &fakeSession{acquire: func(ids []int64) ([]int64, error) {
		if saved.RequestID != ids[0] || saved.Acquired {
			t.Fatal("intent must persist before acquisition")
		}
		return ids, nil
	}}
	s := &oneJob{client: client, save: func(r receipt) error { saved = r; return nil }, stop: func() {}}
	for _, msg := range []*scaleset.RunnerScaleSetMessage{available(10, 11), available(10, 12), nil} {
		if err := s.Scale(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(client.calls, [][]int64{{10}}) || !saved.Acquired {
		t.Fatalf("unexpected admission: %+v %+v", client.calls, saved)
	}
}

func TestUncertainAcquisitionResumesOnlyReservedRequest(t *testing.T) {
	var saved receipt
	failed := &fakeSession{acquire: func([]int64) ([]int64, error) { return nil, errors.New("response lost") }}
	s := &oneJob{client: failed, save: func(r receipt) error { saved = r; return nil }, stop: func() {}}
	if err := s.Scale(context.Background(), available(10)); err == nil {
		t.Fatal("expected acquire failure")
	}
	resumed := &fakeSession{}
	s = &oneJob{state: saved, client: resumed, save: func(r receipt) error { saved = r; return nil }, stop: func() {}}
	if err := s.Scale(context.Background(), available(11)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.calls, [][]int64{{10}}) {
		t.Fatalf("acquired new request after uncertain result: %+v", resumed.calls)
	}
}

func TestPersistenceFailurePreventsAcquire(t *testing.T) {
	client := &fakeSession{}
	s := &oneJob{client: client, save: func(receipt) error { return errors.New("disk full") }, stop: func() {}}
	if err := s.Scale(context.Background(), available(10)); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(client.calls) != 0 {
		t.Fatal("acquired without durable reservation")
	}
}

func TestOfficialListenerAcknowledgesAfterDurableCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved receipt
	client := &fakeSession{messages: []*scaleset.RunnerScaleSetMessage{
		{MessageID: 7, JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "runway-guest", Result: "success", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 10}}}},
	}}
	client.beforeAck = func() {
		if !saved.Completed {
			t.Fatal("ack before completion persisted")
		}
	}
	s := &oneJob{state: receipt{RunnerName: "runway-guest", RequestID: 10, Acquired: true}, client: client, save: func(r receipt) error { saved = r; return nil }, stop: cancel}
	poller, err := listener.New(client, listener.Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := poller.Run(ctx, s); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected listener result %v", err)
	}
	if !reflect.DeepEqual(client.acks, []int{7}) || saved.Result != "success" {
		t.Fatalf("completion not acknowledged %+v %+v", client.acks, saved)
	}
}

func TestWrongRunnerCompletionIsNotAcknowledged(t *testing.T) {
	s := &oneJob{state: receipt{RunnerName: "runway-guest", RequestID: 10, Acquired: true}, client: &fakeSession{}, save: func(receipt) error { t.Fatal("unexpected persistence"); return nil }, stop: func() { t.Fatal("unexpected stop") }}
	err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "runway-other", Result: "success", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 10}}}})
	if err == nil {
		t.Fatal("accepted another runner's result")
	}
}

func TestEmptyAcquireResponseDoesNotReleaseReservation(t *testing.T) {
	client := &fakeSession{acquire: func([]int64) ([]int64, error) { return nil, nil }}
	s := &oneJob{state: receipt{RequestID: 10}, client: client, save: func(receipt) error { return nil }, stop: func() {}}
	if err := s.Scale(context.Background(), available(11)); err != nil {
		t.Fatal(err)
	}
	if s.state.RequestID != 10 || s.state.Acquired {
		t.Fatal("empty response resolved uncertain outcome")
	}
}

func TestCanceledAssignmentIsNotReportedAsSuccessfulWorkflow(t *testing.T) {
	for _, result := range []string{"canceled", "failure", "", "skipped"} {
		if completionError(receipt{Completed: true, Result: result}) == nil {
			t.Fatalf("result %q accepted as success", result)
		}
	}
	if completionError(receipt{Completed: true, Result: "success"}) != nil {
		t.Fatal("successful completion rejected")
	}
	if completionError(receipt{Result: "success"}) == nil {
		t.Fatal("incomplete receipt accepted")
	}
}

func TestPreRegisteredRunnerCanCompleteWithoutAvailability(t *testing.T) {
	for _, withStart := range []bool{false, true} {
		client := &fakeSession{}
		var saved receipt
		stopped := false
		s := &oneJob{state: receipt{RunnerName: "runway-guest"}, client: client, save: func(r receipt) error { saved = r; return nil }, stop: func() { stopped = true }}
		if withStart {
			err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "runway-guest", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 10, WorkflowRunID: 20}}}})
			if err != nil {
				t.Fatal(err)
			}
			if saved.RequestID != 10 || !saved.Started {
				t.Fatal("direct assignment start was lost")
			}
		}
		err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "runway-guest", Result: "success", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 10, WorkflowRunID: 20}}}})
		if err != nil {
			t.Fatal(err)
		}
		if !saved.Completed || saved.RequestID != 10 || saved.WorkflowRunID != 20 || !stopped {
			t.Fatalf("direct completion was lost: %+v", saved)
		}
		if len(client.calls) != 0 {
			t.Fatal("tried acquiring already assigned job")
		}
	}
}

func TestOtherRunnerDoesNotClaimUnreservedSlot(t *testing.T) {
	s := &oneJob{state: receipt{RunnerName: "runway-guest"}, client: &fakeSession{}, save: func(receipt) error { t.Fatal("unexpected persistence"); return nil }, stop: func() { t.Fatal("unexpected stop") }}
	err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "runway-other", Result: "success", JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 10}}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.state.RequestID != 0 {
		t.Fatal("bound unrelated runner")
	}
}

func TestJournalLockRecoveryAndIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "job.json")
	initial := receipt{RepositoryURL: "https://github.com/test/repo", ScaleSetID: 1, RunnerName: "runway-guest"}
	_, save, closeJournal, err := openJournal(path, initial)
	if err != nil {
		t.Fatal(err)
	}
	defer closeJournal()
	if _, _, closeOther, err := openJournal(path, initial); err == nil {
		closeOther()
		t.Fatal("second listener obtained lock")
	}
	initial.RequestID = 10
	if err := save(initial); err != nil {
		t.Fatal(err)
	}
	closeJournal()
	restored, _, closeRestored, err := openJournal(path, initial)
	if err != nil {
		t.Fatal(err)
	}
	if restored.RequestID != 10 {
		t.Fatal("lost persisted reservation")
	}
	closeRestored()
	initial.RunnerName = "runway-other"
	if _, _, closeOther, err := openJournal(path, initial); err == nil {
		closeOther()
		t.Fatal("accepted unrelated identity")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("journal not private")
	}
}

func TestCapturedZeroRequestEventsPersistAndAcknowledge(t *testing.T) {
	// Shape captured from the live third workflow; runner names use Runway branding.
	var started scaleset.JobStarted
	var completed scaleset.JobCompleted
	if err := json.Unmarshal([]byte(`{"messageType":"JobStarted","runnerId":4,"runnerName":"runway-guest-003","runnerRequestId":0,"jobId":"65a0bf0e-2335-5536-afac-ce28104d3b47","workflowRunId":36974691601}`), &started); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"messageType":"JobCompleted","runnerId":4,"runnerName":"runway-guest-003","runnerRequestId":0,"jobId":"65a0bf0e-2335-5536-afac-ce28104d3b47","workflowRunId":36974691601,"result":"succeeded"}`), &completed); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved receipt
	client := &fakeSession{messages: []*scaleset.RunnerScaleSetMessage{{MessageID: 9, JobStartedMessages: []*scaleset.JobStarted{&started}, JobCompletedMessages: []*scaleset.JobCompleted{&completed}}}}
	client.beforeAck = func() {
		if !saved.Completed {
			t.Fatal("ack before durable completion")
		}
	}
	s := &oneJob{state: receipt{RunnerName: "runway-guest-003"}, client: client, save: func(r receipt) error { saved = r; return nil }, stop: cancel}
	poller, err := listener.New(client, listener.Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := poller.Run(ctx, s); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if saved.RequestID != 0 || saved.JobID != started.JobID || !saved.Assigned || !saved.Started || saved.Acquired || saved.Result != "success" || saved.RawResult != "succeeded" || saved.RunnerID != 4 {
		t.Fatalf("invalid receipt %+v", saved)
	}
	if len(client.calls) != 0 || !reflect.DeepEqual(client.acks, []int{9}) {
		t.Fatal("unexpected acquisition or lost acknowledgement")
	}
	if completionError(saved) != nil {
		t.Fatal("captured succeeded result not accepted")
	}
}

func TestZeroRequestCompletionCannotSwitchJobUUID(t *testing.T) {
	s := &oneJob{state: receipt{RunnerName: "runway-guest", RunnerID: 4, JobID: "65a0bf0e-2335-5536-afac-ce28104d3b47"}}
	if _, err := s.bindRunnerEvent("runway-guest", 4, 0, "65a0bf0e-2335-5536-afac-ce28104d3b48"); err == nil {
		t.Fatal("accepted another job's completion")
	}
	if _, err := s.bindRunnerEvent("runway-guest", 5, 0, s.state.JobID); err == nil {
		t.Fatal("accepted another runner ID")
	}
}
