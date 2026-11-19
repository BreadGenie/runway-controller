package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
)

const controllerAssignmentTTL = 3 * time.Minute
const controllerAssignmentCap = 256
const controllerAssignmentTries = 10

type controllerAssignmentRun struct {
	jobs          map[string]scaleset.JobMessageBase
	messageIDs    map[string]int
	expires, next time.Time
	tries         int
	refused       bool
}
type controllerAssignmentResult struct {
	hints      *controllerAssignmentRun
	run        int64
	leaseID    string
	runnerName string
	runnerID   int
	job        cacheGitHubJob
	attempt    int64
	err        error
}

// Re-derivable assignment hints never belong in Central. Keep UUIDs (including
// resolved siblings) for three minutes to detect duplicate rerun display names.
// Saturation disables fallback for a full TTL: dropping a sibling must never
// turn an ambiguous display name into an apparently unique one.
func (e *pilotEngine) rememberAssignment(job scaleset.JobMessageBase, messageID int) {
	now := e.now()
	trace := controllerTrace{Event: "controller_assignment_hint_rejected", MessageID: messageID, controllerTraceJob: traceScaleSetJob(job, "", 0)}
	if now.Before(e.assignmentSaturated) {
		trace.Reason = "capacity"
		e.trace(trace)
		return
	}
	if !canonicalCacheUUID(job.JobID) || job.WorkflowRunID <= 0 || job.JobDisplayName == "" || len(job.JobDisplayName)+len(job.JobWorkflowRef)+len(job.EventName)+len(strings.Join(job.RequestLabels, ""))+16*len(job.RequestLabels) > 8192 {
		trace.Reason = "invalid_hint"
		e.trace(trace)
		return
	}
	repository := strings.TrimPrefix(e.snapshot.Pool.RepositoryURL, "https://github.com/")
	if job.OwnerName+"/"+job.RepositoryName != repository {
		trace.Reason = "repository_mismatch"
		e.trace(trace)
		return
	}
	if e.pendingAssignments == nil {
		e.pendingAssignments = make(map[int64]*controllerAssignmentRun)
	}
	count := 0
	for id, run := range e.pendingAssignments {
		if !now.Before(run.expires) {
			e.traceAssignmentRun(run, "controller_assignment_hint_expired", "ttl_elapsed")
			delete(e.pendingAssignments, id)
			continue
		}
		count += len(run.jobs)
	}
	run := e.pendingAssignments[job.WorkflowRunID]
	if run != nil {
		if old, ok := run.jobs[job.JobID]; ok {
			// Replays do not extend TTL or restart the retry budget. Conflicting
			// redeliveries poison the run rather than replacing earlier evidence.
			if old.JobDisplayName != job.JobDisplayName || old.JobWorkflowRef != job.JobWorkflowRef || old.EventName != job.EventName || old.RunnerRequestID != job.RunnerRequestID {
				run.refused = true
				trace.Reason = "conflicting_replay"
				e.trace(trace)
			}
			return
		}
	}
	if count >= controllerAssignmentCap {
		for _, pending := range e.pendingAssignments {
			e.traceAssignmentRun(pending, "controller_assignment_hint_rejected", "capacity")
		}
		clear(e.pendingAssignments)
		trace.Reason = "capacity"
		e.trace(trace)
		e.assignmentSaturated = now.Add(controllerAssignmentTTL)
		return
	}
	if run == nil {
		run = &controllerAssignmentRun{jobs: make(map[string]scaleset.JobMessageBase), messageIDs: make(map[string]int), expires: now.Add(controllerAssignmentTTL)}
		e.pendingAssignments[job.WorkflowRunID] = run
	}
	job.RequestLabels = append([]string(nil), job.RequestLabels...)
	run.jobs[job.JobID] = job
	run.messageIDs[job.JobID] = messageID
	trace.Event = "controller_assignment_hint_created"
	e.trace(trace)
}

func (e *pilotEngine) traceAssignmentRun(run *controllerAssignmentRun, event, reason string) {
	if e.logTrace == nil {
		return
	}
	for id, job := range run.jobs {
		e.trace(controllerTrace{Event: event, Reason: reason, MessageID: run.messageIDs[id], Attempt: run.tries, controllerTraceJob: traceScaleSetJob(job, "", 0)})
	}
}

// Only this serialized engine method touches hints and leases. The worker owns
// immutable inputs and returns REST observations; it never writes Central.
func (e *pilotEngine) resolveAssignments(ctx context.Context) error {
	if e.assignmentResults == nil {
		e.assignmentResults = make(chan controllerAssignmentResult, 1)
	}
	select {
	case result := <-e.assignmentResults:
		e.assignmentBusy = false
		if e.assignmentCancel != nil {
			e.assignmentCancel()
			e.assignmentCancel = nil
		}
		if err := e.applyAssignmentResult(ctx, result); err != nil {
			return err
		}
	default:
	}
	for id, run := range e.pendingAssignments {
		if !e.now().Before(run.expires) {
			e.traceAssignmentRun(run, "controller_assignment_hint_expired", "ttl_elapsed")
			delete(e.pendingAssignments, id)
		}
	}
	if e.assignmentBusy && e.assignmentCancel != nil {
		eligible := false
		for _, lease := range e.snapshot.Leases {
			if lease.ID == e.assignmentLeaseID && e.assignmentLeaseEligible(lease) {
				eligible = true
			}
		}
		if !eligible {
			e.assignmentCancel()
		}
	}
	if e.assignmentREST == nil || e.assignmentBusy || e.now().Before(e.assignmentSaturated) {
		return nil
	}
	for id, run := range e.pendingAssignments {
		if run.refused || run.tries >= controllerAssignmentTries || e.now().Before(run.next) {
			continue
		}
		for _, lease := range e.snapshot.Leases {
			if !e.assignmentLeaseEligible(lease) {
				continue
			}
			run.tries++
			run.next = e.now().Add(3 * time.Second)
			e.assignmentBusy = true
			client, results, ready := e.assignmentREST, e.assignmentResults, e.jobWorkerReady
			workerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			e.assignmentCancel = cancel
			e.assignmentLeaseID = lease.ID
			repository := strings.TrimPrefix(e.snapshot.Pool.RepositoryURL, "https://github.com/")
			go func(runID int64, lease pilotLease, hints *controllerAssignmentRun) {
				defer cancel()
				authority := githubCacheJobAuthority{client: client, now: time.Now}
				attempt := int64(0)
				_, job, err := authority.confirmREST(workerCtx, cacheGuestBinding{Repository: repository, RunnerName: lease.RunnerName, RunnerID: int64(lease.RunnerID)}, runID, &attempt, true)
				results <- controllerAssignmentResult{hints: hints, run: runID, leaseID: lease.ID, runnerName: lease.RunnerName, runnerID: lease.RunnerID, job: job, attempt: attempt, err: err}
				notifyJobWorkerReady(ready)
			}(id, lease, run)
			return nil
		}
	}
	return nil
}

func (e *pilotEngine) assignmentLeaseEligible(lease pilotLease) bool {
	repository := strings.TrimPrefix(e.snapshot.Pool.RepositoryURL, "https://github.com/")
	return e.snapshot.Pool.Enabled && lease.RunnerID > 0 && lease.RunnerName != "" && lease.JobID == "" && !lease.Started && lease.Phase != "deleted" && lease.Phase != "deleting" && e.now().Before(lease.Deadline) && (lease.Repository == "" || lease.Repository == repository)
}

func (e *pilotEngine) applyAssignmentResult(ctx context.Context, result controllerAssignmentResult) error {
	run := e.pendingAssignments[result.run]
	trace := controllerTrace{Event: "controller_assignment_hint_rejected", controllerTraceJob: traceLease(pilotLease{
		ID: result.leaseID, RunnerName: result.runnerName, RunnerID: result.runnerID, WorkflowRunID: strconv.FormatInt(result.run, 10),
	})}
	if run == nil || run != result.hints {
		trace.Reason = "stale_hint"
		e.trace(trace)
		return nil
	}
	if errors.Is(result.err, errCacheJobMismatch) {
		run.refused = true
	}
	trace.Attempt = run.tries
	switch {
	case !e.now().Before(run.expires):
		trace.Reason = "ttl_elapsed"
	case result.err != nil:
		trace.Reason = controllerRESTJobReason(result.err)
	case run.refused:
		trace.Reason = "refused_hint"
	default:
		trace.Reason = "name_not_found"
		var matched *scaleset.JobMessageBase
		for _, job := range run.jobs {
			if job.JobDisplayName != result.job.Name {
				continue
			}
			if matched != nil {
				run.refused = true
				matched = nil
				trace.Reason = "ambiguous_name"
				break
			}
			copy := job
			matched = &copy
		}
		if matched != nil {
			trace.JobUUID = matched.JobID
			trace.MessageID = run.messageIDs[matched.JobID]
			trace.Reason = "stale_lease"
			for _, lease := range e.snapshot.Leases {
				if lease.ID != result.leaseID || !e.assignmentLeaseEligible(lease) || lease.RunnerName != result.runnerName || lease.RunnerID != result.runnerID {
					continue
				}
				fields, err := jobEvidence(lease, *matched)
				if err != nil {
					run.refused = true
					trace.Reason = "evidence_mismatch"
					break
				}
				fields.RunAttempt = strconv.FormatInt(result.attempt, 10)
				if err := e.persistJobEvidence(ctx, lease, "assigned", fields, trace.MessageID); err != nil {
					trace.Reason = "write_failed"
					e.trace(trace)
					return err
				}
				trace.Event = "controller_assignment_hint_resolved"
				trace.Reason = ""
				e.trace(trace)
			}
		}
	}
	if trace.Reason != "" {
		e.trace(trace)
		if run.tries == controllerAssignmentTries && !run.refused && trace.Reason != "ttl_elapsed" {
			e.traceAssignmentRun(run, "controller_assignment_hint_exhausted", "retry_limit")
		}
	}
	return nil
}

func controllerRESTJobReason(err error) string {
	switch {
	case errors.Is(err, errCacheJobMismatch):
		return "runner_or_job_mismatch"
	case errors.Is(err, errCacheJobCompleted):
		return "job_completed"
	case err == errCacheJobNotFound:
		return "job_not_running"
	default:
		return "rest_unavailable"
	}
}
