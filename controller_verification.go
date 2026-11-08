package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type controllerVerificationRetry struct {
	tries int
	next  time.Time
}
type controllerVerificationResult struct {
	lease  pilotLease
	fields leaseEvent
}

func verifyControllerJob(ctx context.Context, client *http.Client, l pilotLease) leaseEvent {
	f := leaseEvent{VerifyReason: "rest_unavailable"}
	runID, err := strconv.ParseInt(l.WorkflowRunID, 10, 64)
	if err != nil {
		f.VerifyReason = "invalid_run"
		return f
	}
	policy, err := readCacheRepositoryPolicy(ctx, client, l.Repository)
	if err != nil {
		return f
	}
	data, err := readCacheGitHubMetadata(ctx, client, "/repos/"+l.Repository+"/actions/runs/"+l.WorkflowRunID)
	if err != nil {
		return f
	}
	c, err := cacheWorkflowContext(data, policy.RepositoryID, runID, l.JobWorkflowRef)
	if err != nil {
		f.VerifyReason = "run_mismatch"
		return f
	}
	if l.EventName != "" && l.EventName != *c.EventName || l.RunAttempt != "" && l.RunAttempt != "0" && l.RunAttempt != *c.RunAttempt {
		f.VerifyReason = "evidence_mismatch"
		return f
	}
	attempt, _ := strconv.ParseInt(*c.RunAttempt, 10, 64)
	authority := githubCacheJobAuthority{client: client, now: time.Now}
	_, job, err := authority.confirmREST(ctx, cacheGuestBinding{Repository: l.Repository, RunnerID: int64(l.RunnerID), RunnerName: l.RunnerName}, runID, &attempt)
	if err != nil {
		f.VerifyReason = "job_not_running"
		return f
	}
	if job.Name != l.JobDisplayName {
		f.VerifyReason = "job_mismatch"
		return f
	}
	return leaseEvent{Repository: policy.Repository, Verified: true, Ref: *c.Ref, SHA: *c.SHA, EventName: *c.EventName, RunAttempt: *c.RunAttempt, JobWorkflowRef: l.JobWorkflowRef, RepositoryID: policy.RepositoryID, DefaultBranch: policy.DefaultBranch}
}

// All Central mutations stay on the serialized engine. REST workers carry
// immutable snapshots and bounded timeouts; retries survive ordinary reconciles.
func (e *pilotEngine) verifyJobs(ctx context.Context) error {
	if e.verificationResults == nil {
		e.verificationResults = make(chan controllerVerificationResult, 1)
		e.verificationRetries = map[string]*controllerVerificationRetry{}
	}
	select {
	case r := <-e.verificationResults:
		e.verificationBusy = false
		for _, l := range e.snapshot.Leases {
			if l.ID != r.lease.ID || l.Verified || l.JobID != r.lease.JobID || l.WorkflowRunID != r.lease.WorkflowRunID || l.RunnerID != r.lease.RunnerID || l.RunnerName != r.lease.RunnerName || l.JobWorkflowRef != r.lease.JobWorkflowRef || l.EventName != r.lease.EventName || l.RunAttempt != r.lease.RunAttempt || l.Repository != r.lease.Repository || l.Phase == "deleted" || l.Phase == "deleting" {
				continue
			}
			if _, err := e.update(ctx, l, "verified", r.fields); err != nil {
				return err
			}
		}
	default:
	}
	if e.assignmentREST == nil || e.verificationBusy || !e.snapshot.Pool.Enabled {
		return nil
	}
	active := map[string]bool{}
	for _, l := range e.snapshot.Leases {
		if l.Phase != "deleted" && l.Phase != "deleting" {
			active[l.ID] = true
		}
	}
	for id := range e.verificationRetries {
		if !active[id] {
			delete(e.verificationRetries, id)
		}
	}
	for _, l := range e.snapshot.Leases {
		if l.Verified || l.JobID == "" || l.WorkflowRunID == "" || l.RunnerID <= 0 || l.Phase == "deleted" || l.Phase == "deleting" || !e.now().Before(l.Deadline) {
			continue
		}
		retry := e.verificationRetries[l.ID]
		if retry == nil {
			retry = &controllerVerificationRetry{}
			e.verificationRetries[l.ID] = retry
		}
		if retry.tries >= controllerAssignmentTries || e.now().Before(retry.next) {
			continue
		}
		retry.tries++
		retry.next = e.now().Add(3 * time.Second)
		e.verificationBusy = true
		repository := strings.TrimPrefix(e.snapshot.Pool.RepositoryURL, "https://github.com/")
		client, results := e.assignmentREST, e.verificationResults
		go func(l pilotLease) {
			worker, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			input := l
			if input.Repository == "" {
				input.Repository = repository
			}
			f := verifyControllerJob(worker, client, input)
			select {
			case results <- controllerVerificationResult{l, f}:
			case <-ctx.Done():
			}
		}(l)
		break
	}
	return nil
}
