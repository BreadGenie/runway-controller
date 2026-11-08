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
func (e *pilotEngine) rememberAssignment(job scaleset.JobMessageBase) {
	now := e.now()
	if now.Before(e.assignmentSaturated) || !canonicalCacheUUID(job.JobID) || job.WorkflowRunID <= 0 || job.JobDisplayName == "" || len(job.JobDisplayName)+len(job.JobWorkflowRef)+len(job.EventName)+len(strings.Join(job.RequestLabels, ""))+16*len(job.RequestLabels) > 8192 {
		return
	}
	repository := strings.TrimPrefix(e.snapshot.Pool.RepositoryURL, "https://github.com/")
	if job.OwnerName+"/"+job.RepositoryName != repository {
		return
	}
	if e.pendingAssignments == nil {
		e.pendingAssignments = make(map[int64]*controllerAssignmentRun)
	}
	count := 0
	for id, run := range e.pendingAssignments {
		if !now.Before(run.expires) {
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
			}
			return
		}
	}
	if count >= controllerAssignmentCap {
		clear(e.pendingAssignments)
		e.assignmentSaturated = now.Add(controllerAssignmentTTL)
		return
	}
	if run == nil {
		run = &controllerAssignmentRun{jobs: make(map[string]scaleset.JobMessageBase), expires: now.Add(controllerAssignmentTTL)}
		e.pendingAssignments[job.WorkflowRunID] = run
	}
	job.RequestLabels = append([]string(nil), job.RequestLabels...)
	run.jobs[job.JobID] = job
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
		run := e.pendingAssignments[result.run]
		if run != nil && run == result.hints && errors.Is(result.err, errCacheJobMismatch) {
			run.refused = true
		}
		if run != nil && run == result.hints && e.now().Before(run.expires) && !run.refused && result.err == nil {
			var matched *scaleset.JobMessageBase
			for _, job := range run.jobs {
				if job.JobDisplayName == result.job.Name {
					if matched != nil {
						run.refused = true
						matched = nil
						break
					}
					copy := job
					matched = &copy
				}
			}
			if matched != nil {
				for _, lease := range e.snapshot.Leases {
					if lease.ID != result.leaseID || !e.assignmentLeaseEligible(lease) || lease.RunnerName != result.runnerName || lease.RunnerID != result.runnerID {
						continue
					}
					fields, err := jobEvidence(lease, *matched)
					if err != nil {
						run.refused = true
						break
					}
					fields.RunAttempt = strconv.FormatInt(result.attempt, 10)
					if err := e.persistJobEvidence(ctx, lease, "assigned", fields); err != nil {
						return err
					}
				}
			}
		}
	default:
	}
	for id, run := range e.pendingAssignments {
		if !e.now().Before(run.expires) {
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
			client, results := e.assignmentREST, e.assignmentResults
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
