package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// Job evidence belongs to the lease, regardless of whether GitHub offers a job
// before dispatch or reports its identity only after the runner starts it.
// Central remains authoritative; these checks prevent conflicting evidence from
// triggering diagnostics or bootstrap recovery before Central can reject it.
func jobEvidence(lease pilotLease, job scaleset.JobMessageBase) (leaseEvent, error) {
	fields := leaseEvent{JobDisplayName: job.JobDisplayName, JobWorkflowRef: job.JobWorkflowRef, EventName: job.EventName, JobID: job.JobID, RequestID: job.RunnerRequestID,
		RequestLabels: strings.Join(job.RequestLabels, "\n")}
	if job.OwnerName != "" || job.RepositoryName != "" {
		if job.OwnerName == "" || job.RepositoryName == "" {
			return leaseEvent{}, errors.New("job evidence requires both repository owner and name")
		}
		fields.Repository = job.OwnerName + "/" + job.RepositoryName
	}
	if job.WorkflowRunID != 0 {
		fields.WorkflowRunID = strconv.FormatInt(job.WorkflowRunID, 10)
	}
	if fields.RequestID < 0 || (fields.RequestID > 0 && lease.RequestID > 0 && fields.RequestID != lease.RequestID) {
		return leaseEvent{}, errors.New("job request ID conflicts with durable lease")
	}
	for _, identity := range [][2]string{{lease.JobDisplayName, fields.JobDisplayName}, {lease.JobWorkflowRef, fields.JobWorkflowRef}, {lease.EventName, fields.EventName}, {lease.JobID, fields.JobID}, {lease.WorkflowRunID, fields.WorkflowRunID}, {lease.Repository, fields.Repository}, {lease.RequestLabels, fields.RequestLabels}} {
		if identity[0] != "" && identity[1] != "" && identity[0] != identity[1] {
			return leaseEvent{}, errors.New("job evidence conflicts with durable lease")
		}
	}
	return fields, nil
}

func (e *pilotEngine) recordJobEvidence(ctx context.Context, message *scaleset.RunnerScaleSetMessage) error {
	for _, job := range message.JobAssignedMessages {
		var matched *pilotLease
		for _, lease := range e.snapshot.Leases {
			if lease.Phase == "deleted" || lease.Phase == "deleting" {
				continue
			}
			if (job.RunnerRequestID > 0 && lease.RequestID == job.RunnerRequestID) || (lease.JobID != "" && lease.JobID == job.JobID) {
				if matched != nil {
					return errors.New("ambiguous job assignment")
				}
				copy := lease
				matched = &copy
			}
		}
		if matched == nil {
			continue
		}
		if _, err := uuid.Parse(job.JobID); err != nil {
			return errors.New("assignment requires stable job UUID")
		}
		fields, err := jobEvidence(*matched, job.JobMessageBase)
		if err != nil {
			return err
		}
		if err := e.persistJobEvidence(ctx, *matched, "assigned", fields); err != nil {
			return err
		}
	}
	for _, job := range message.JobStartedMessages {
		for _, lease := range e.snapshot.Leases {
			if lease.RunnerName != job.RunnerName {
				continue
			}
			if err := matchPilotJob(lease, job.RunnerID, job.JobID); err != nil {
				return err
			}
			if lease.Phase == "deleted" || lease.Phase == "deleting" {
				continue
			}
			fields, err := jobEvidence(lease, job.JobMessageBase)
			if err != nil {
				return err
			}
			fields.RunnerID = job.RunnerID
			if lease.Phase == "jit_ready" {
				// The authoritative job-start event resolves an interrupted
				// bootstrap acknowledgment without resending credentials.
				updated, err := e.update(ctx, lease, "bootstrapped", leaseEvent{})
				if err != nil {
					return err
				}
				lease = updated
			}
			if err := e.persistJobEvidence(ctx, lease, "started", fields); err != nil {
				return err
			}
		}
	}
	for _, job := range message.JobCompletedMessages {
		for _, lease := range e.snapshot.Leases {
			matchedName := lease.RunnerName == job.RunnerName
			canceledBeforeAssignment := job.RunnerName == "" && job.Result == "canceled" && lease.JobID != "" && lease.JobID == job.JobID
			if !matchedName && !canceledBeforeAssignment {
				continue
			}
			if matchedName {
				if err := matchPilotJob(lease, job.RunnerID, job.JobID); err != nil {
					return err
				}
			}
			fields, err := jobEvidence(lease, job.JobMessageBase)
			if err != nil {
				return err
			}
			fields.RunnerID = job.RunnerID
			fields.Result = normalizeResult(job.Result)
			diagnosticsError := ""
			if lease.Phase != "deleted" && lease.Phase != "deleting" && lease.VMID != "" {
				if err := e.collect(ctx, lease); err != nil {
					diagnosticsError = "guest diagnostics unavailable; completion and cleanup continue"
				}
			}
			fields.Error = diagnosticsError
			if _, err := e.update(ctx, lease, "completed", fields); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *pilotEngine) acquireAvailableJob(ctx context.Context, message *scaleset.RunnerScaleSetMessage) error {
	for _, lease := range e.snapshot.Leases {
		if lease.Phase == "deleting" || lease.Phase == "deleted" {
			continue
		}
		if !e.now().Before(lease.Deadline) {
			continue
		}
		if lease.AcquireState == "pending" {
			return e.acquire(ctx, lease)
		}
		// An offer can arrive after provisioning. Direct dispatch may omit
		// offers entirely, so recording evidence never gates VM creation.
		if lease.RequestID == 0 && lease.JobID == "" && len(message.JobAvailableMessages) > 0 {
			job := message.JobAvailableMessages[0]
			if job.RunnerRequestID <= 0 {
				return errors.New("available job omitted a positive request ID; not acknowledged")
			}
			fields, err := jobEvidence(lease, job.JobMessageBase)
			if err != nil {
				return err
			}
			reserved, err := e.update(ctx, lease, "acquire_requested", fields)
			if err != nil {
				return err
			}
			return e.acquire(ctx, reserved)
		}
	}
	return nil
}

func matchPilotJob(lease pilotLease, runnerID int, jobID string) error {
	if _, err := uuid.Parse(jobID); err != nil {
		return errors.New("runner event requires stable job UUID")
	}
	if lease.RunnerID > 0 && runnerID != lease.RunnerID {
		return errors.New("runner event ID conflicts with durable lease")
	}
	if lease.JobID != "" && jobID != lease.JobID {
		return errors.New("runner event job UUID conflicts with durable lease")
	}
	return nil
}

func (e *pilotEngine) acquire(ctx context.Context, lease pilotLease) error {
	ctx, cancel := context.WithDeadline(ctx, lease.Deadline)
	defer cancel()
	if e.acquirer == nil {
		return errors.New("GitHub message session unavailable for acquisition")
	}
	ids, err := e.acquirer.AcquireJobs(ctx, []int64{lease.RequestID})
	if err != nil {
		return errors.New("GitHub acquisition outcome uncertain; reserved request retained")
	}
	for _, id := range ids {
		if id == lease.RequestID {
			if _, err := e.update(ctx, lease, "acquired", leaseEvent{}); err != nil {
				return err
			}
			e.demand = 1
			return nil
		}
	}
	// Empty responses do not resolve an earlier uncertain request.
	return nil
}

// Skip durable replays, including after a restart; omitted evidence never erases a value.
func (e *pilotEngine) persistJobEvidence(ctx context.Context, lease pilotLease, event string, fields leaseEvent) error {
	changed := event == "started" && !lease.Started
	for _, pair := range [][2]string{{lease.JobID, fields.JobID}, {lease.WorkflowRunID, fields.WorkflowRunID}, {lease.Repository, fields.Repository}, {lease.RequestLabels, fields.RequestLabels}, {lease.JobDisplayName, fields.JobDisplayName}, {lease.JobWorkflowRef, fields.JobWorkflowRef}, {lease.EventName, fields.EventName}, {lease.RunAttempt, fields.RunAttempt}} {
		changed = changed || (pair[1] != "" && pair[0] != pair[1])
	}
	changed = changed || (fields.RequestID > 0 && fields.RequestID != lease.RequestID) || (fields.RunnerID > 0 && fields.RunnerID != lease.RunnerID)
	if !changed {
		return nil
	}
	_, err := e.update(ctx, lease, event, fields)
	return err
}
