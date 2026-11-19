package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestControllerTraceReceiptPrecedesHandlingAndDurableEvidence(t *testing.T) {
	s := newPilotStore()
	lease := pilotLease{ID: "lease", Phase: "running", RunnerName: "vm-9000-0002", RunnerID: 318, LastError: "secret-token-from-error", Deadline: time.Now().Add(time.Hour)}
	s.snapshot.Leases = []pilotLease{lease}
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	traces := make(chan controllerTrace, 8)
	e.logTrace = func(trace controllerTrace) { traces <- trace }
	deliveries := make(chan pilotDelivery)
	scaler := pilotScaler{deliveries: deliveries, trace: e.trace}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	job := scaleset.JobMessageBase{
		JobID: "75938dc1-f3f1-53b4-8254-ed5ffa6cf9ee", WorkflowRunID: 123,
		JobDisplayName: "secret-display", JobWorkflowRef: "secret-workflow",
		RequestLabels: []string{"secret-label"}, EventName: "secret-event",
	}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID: 17,
		JobStartedMessages: []*scaleset.JobStarted{
			{JobMessageBase: job, RunnerName: lease.RunnerName, RunnerID: lease.RunnerID},
		},
		JobCompletedMessages: []*scaleset.JobCompleted{
			{JobMessageBase: job, RunnerName: lease.RunnerName, RunnerID: lease.RunnerID, Result: "success"},
		},
	}
	done := make(chan error, 1)
	go func() { done <- scaler.Scale(ctx, msg) }()
	select {
	case receipt := <-traces:
		if receipt.Event != "controller_scale_set_received" || receipt.MessageID != 17 || len(receipt.Jobs) != 2 ||
			receipt.Jobs[0].Evidence != "started" || receipt.Jobs[0].JobUUID != job.JobID || s.find(lease.ID).Started {
			t.Fatalf("unexpected receipt before handling: %+v", receipt)
		}
		assertTraceJSON(t, receipt, []string{"event", "jobs", "message_id", "pool", "timestamp"})
	case <-ctx.Done():
		t.Fatal("receipt was not logged before the engine accepted the delivery")
	}
	var delivery pilotDelivery
	select {
	case delivery = <-deliveries:
	case <-ctx.Done():
		t.Fatal("delivery stalled")
	}
	err := e.Handle(ctx, delivery.message)
	delivery.result <- err
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []string{"started", "completed"} {
		select {
		case trace := <-traces:
			if trace.Event != "controller_job_evidence_written" || trace.Evidence != evidence || trace.LeaseID != lease.ID || trace.JobUUID != job.JobID || trace.RunID != "123" || trace.MessageID != 17 {
				t.Fatalf("unexpected evidence trace: %+v", trace)
			}
			assertTraceJSON(t, trace, []string{"event", "evidence", "job_uuid", "lease_id", "message_id", "pool", "run_id", "runner_id", "runner_name", "timestamp"})
		default:
			t.Fatalf("missing %s evidence trace", evidence)
		}
	}
	select {
	case trace := <-traces:
		if trace.Event != "controller_verification_skipped" || trace.Reason != "started_and_completed" || trace.JobUUID != job.JobID {
			t.Fatalf("unexpected skip trace: %+v", trace)
		}
	default:
		t.Fatal("missing same-delivery verification skip")
	}
}

func assertTraceJSON(t *testing.T, trace controllerTrace, keys []string) {
	t.Helper()
	var buf bytes.Buffer
	writeControllerTrace(&buf, "pool", trace)
	if bytes.Contains(buf.Bytes(), []byte("secret-")) {
		t.Fatalf("trace leaked input: %s", buf.Bytes())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != len(keys) {
		t.Fatalf("unexpected trace fields: %s", buf.Bytes())
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %s: %s", key, buf.Bytes())
		}
	}
	var timestamp time.Time
	if err := json.Unmarshal(fields["timestamp"], &timestamp); err != nil || timestamp.IsZero() {
		t.Fatalf("invalid timestamp: %s", fields["timestamp"])
	}
	if jobs, ok := fields["jobs"]; ok {
		var decoded []map[string]json.RawMessage
		if err := json.Unmarshal(jobs, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, job := range decoded {
			delete(job, "evidence")
			delete(job, "job_uuid")
			delete(job, "runner_id")
			delete(job, "runner_name")
			delete(job, "run_id")
			if len(job) != 0 {
				t.Fatalf("unexpected job fields: %v", job)
			}
		}
	}
}

func TestControllerTraceVerificationDiscardedAfterRetirement(t *testing.T) {
	l, client := verifiedLeaseFixture(t)
	s := newPilotStore()
	s.snapshot.Leases = []pilotLease{l}
	e := newPilotEngine(s, &fakePilotGitHub{store: s}, &fakeGuestTransport{})
	e.assignmentREST = client
	traces := make(chan controllerTrace, 8)
	e.logTrace = func(trace controllerTrace) { traces <- trace }
	if err := e.verifyJobs(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.update(t.Context(), l, "completed", leaseEvent{Result: "success"}); err != nil {
		t.Fatal(err)
	}
	drainVerification(t, e)
	var events []string
	for len(traces) > 0 {
		trace := <-traces
		events = append(events, trace.Event)
		switch trace.Event {
		case "controller_verification_rest_result":
			if trace.Reason != "verified" {
				t.Fatalf("fixture did not produce a successful REST result: %+v", trace)
			}
		case "controller_verification_discarded":
			if trace.Reason != "lease_retiring" || trace.LeaseID != l.ID || trace.JobUUID != l.JobID {
				t.Fatalf("incorrect discard: %+v", trace)
			}
		}
	}
	want := []string{"controller_verification_scheduled", "controller_verification_rest_result", "controller_verification_discarded"}
	if !reflect.DeepEqual(events, want) || s.find(l.ID).Verified {
		t.Fatalf("events = %v, want %v and no durable verification", events, want)
	}
	if reason := controllerRESTJobReason(errors.New("secret-rest-error")); reason != "rest_unavailable" {
		t.Fatalf("unexpected REST reason %q", reason)
	}
}
