package main

import (
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/actions/scaleset"
)

// Only these fields cross the trace boundary. Never encode SDK messages,
// leases, REST responses or errors directly.
type controllerTrace struct {
	Event     string               `json:"event"`
	MessageID int                  `json:"message_id,omitempty"`
	Reason    string               `json:"reason,omitempty"`
	Attempt   int                  `json:"attempt,omitempty"`
	Jobs      []controllerTraceJob `json:"jobs,omitempty"`
	controllerTraceJob
}

type controllerTraceJob struct {
	Evidence   string `json:"evidence,omitempty"`
	LeaseID    string `json:"lease_id,omitempty"`
	RunnerName string `json:"runner_name,omitempty"`
	RunnerID   int    `json:"runner_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	JobUUID    string `json:"job_uuid,omitempty"`
}

func traceLease(l pilotLease) controllerTraceJob {
	j := controllerTraceJob{LeaseID: l.ID, RunnerName: l.RunnerName, RunnerID: l.RunnerID}
	if id, err := strconv.ParseInt(l.WorkflowRunID, 10, 64); err == nil && id > 0 {
		j.RunID = l.WorkflowRunID
	}
	if canonicalCacheUUID(l.JobID) {
		j.JobUUID = l.JobID
	}
	return j
}

func traceScaleSetJob(job scaleset.JobMessageBase, runnerName string, runnerID int) controllerTraceJob {
	return traceLease(pilotLease{JobID: job.JobID, WorkflowRunID: strconv.FormatInt(job.WorkflowRunID, 10), RunnerName: runnerName, RunnerID: runnerID})
}

func traceMessage(event string, message *scaleset.RunnerScaleSetMessage) controllerTrace {
	t := controllerTrace{Event: event}
	if message == nil {
		return t
	}
	t.MessageID = message.MessageID
	if event != "controller_scale_set_received" {
		return t
	}
	appendJob := func(kind string, job scaleset.JobMessageBase, runnerName string, runnerID int) {
		j := traceScaleSetJob(job, runnerName, runnerID)
		j.Evidence = kind
		t.Jobs = append(t.Jobs, j)
	}
	for _, job := range message.JobAvailableMessages {
		if job != nil {
			appendJob("available", job.JobMessageBase, "", 0)
		}
	}
	for _, job := range message.JobAssignedMessages {
		if job != nil {
			appendJob("assigned", job.JobMessageBase, "", 0)
		}
	}
	for _, job := range message.JobStartedMessages {
		if job != nil {
			appendJob("started", job.JobMessageBase, job.RunnerName, job.RunnerID)
		}
	}
	for _, job := range message.JobCompletedMessages {
		if job != nil {
			appendJob("completed", job.JobMessageBase, job.RunnerName, job.RunnerID)
		}
	}
	return t
}

func (e *pilotEngine) trace(t controllerTrace) {
	if e.logTrace != nil {
		e.logTrace(t)
	}
}

func writeControllerTrace(w io.Writer, pool string, event controllerTrace) {
	json.NewEncoder(w).Encode(struct {
		controllerTrace
		Timestamp time.Time `json:"timestamp"`
		Pool      string    `json:"pool"`
	}{event, time.Now().UTC(), pool})
}

func (e *pilotEngine) traceJobEvidence(lease pilotLease, evidence string, messageID int) {
	job := traceLease(lease)
	job.Evidence = evidence
	e.trace(controllerTrace{Event: "controller_job_evidence_written", MessageID: messageID, controllerTraceJob: job})
}
