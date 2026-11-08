package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// guestWorkFolder is the absolute work folder requested for self-hosted scale
// set runners. actions/setup-node stores a yarn cache as paths relative to the
// checkout workspace, and the relative prefix is baked into the archive. The
// relative "_work" default resolves under the runner install root, putting the
// checkout one level deeper than GitHub-hosted
// ($HOME/actions-runner/_work/<repo>/<repo> vs $HOME/work/<repo>/<repo>), so a
// cache entry written by one layout fails to extract under the other with
// "Cannot mkdir" on every member. Requesting the absolute GitHub-hosted layout
// makes both sides emit the same relative prefix and share cache entries.
// The runner accepts an absolute value: RunnerSettings.WorkFolder is an
// unvalidated string and HostContext resolves it with Path.Combine, which
// returns a rooted second operand verbatim.
const guestWorkFolder = "/home/runner/work"

type pilotGitHub interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

type pilotEngine struct {
	verificationResults chan controllerVerificationResult
	verificationRetries map[string]*controllerVerificationRetry
	verificationBusy    bool
	assignmentREST      *http.Client
	pendingAssignments  map[int64]*controllerAssignmentRun
	assignmentResults   chan controllerAssignmentResult
	assignmentBusy      bool
	assignmentCancel    context.CancelFunc
	assignmentLeaseID   string
	assignmentSaturated time.Time
	store               pilotStore
	github              pilotGitHub
	guest               guestTransport
	acquirer            jobAcquirer
	snapshot            pilotSnapshot
	demand              int
	haveDemand          bool
	now                 func() time.Time
}

func (e *pilotEngine) remember(lease pilotLease) {
	for i := range e.snapshot.Leases {
		if e.snapshot.Leases[i].ID == lease.ID {
			e.snapshot.Leases[i] = lease
			return
		}
	}
	e.snapshot.Leases = append(e.snapshot.Leases, lease)
}
func (e *pilotEngine) update(ctx context.Context, lease pilotLease, event string, fields leaseEvent) (pilotLease, error) {
	updated, err := e.store.Update(ctx, lease.ID, event, fields)
	if err == nil {
		e.remember(updated)
		if e.assignmentCancel != nil && updated.ID == e.assignmentLeaseID && !e.assignmentLeaseEligible(updated) {
			e.assignmentCancel()
		}
	}
	return updated, err
}
func (e *pilotEngine) occupied() bool {
	for _, lease := range e.snapshot.Leases {
		if lease.Phase != "deleted" || lease.JITState != "removed" {
			return true
		}
	}
	return false
}
func (e *pilotEngine) reserve(ctx context.Context) (pilotLease, error) {
	lease, err := e.store.Reserve(ctx, uuid.NewString())
	if err == nil {
		e.remember(lease)
	}
	return lease, err
}

// Handle persists admission and job evidence before the SDK acknowledges.
// Scaling uses the current statistics, never lifecycle-message counts.
func (e *pilotEngine) Handle(ctx context.Context, message *scaleset.RunnerScaleSetMessage) error {
	if message == nil {
		return nil
	}
	if message.Statistics != nil {
		e.demand = min(1, max(0, message.Statistics.TotalAssignedJobs))
		e.haveDemand = true
	}
	if err := e.recordJobEvidence(ctx, message); err != nil {
		return err
	}
	if !e.snapshot.Pool.Enabled {
		return nil
	}
	if !e.occupied() && (e.demand > 0 || len(message.JobAvailableMessages) > 0) {
		if _, err := e.reserve(ctx); err != nil {
			return err
		}
	}
	if err := e.verifyJobs(ctx); err != nil {
		return err
	}
	if err := e.resolveAssignments(ctx); err != nil {
		return err
	}
	return e.acquireAvailableJob(ctx, message)
}

// Reconcile executes at most one provisioning step per lease; Central returns
// every durable phase after restart. JIT material exists only during one call.
func (e *pilotEngine) Reconcile(ctx context.Context) error {
	if err := e.verifyJobs(ctx); err != nil {
		return err
	}
	if err := e.resolveAssignments(ctx); err != nil {
		return err
	}
	for _, lease := range append([]pilotLease(nil), e.snapshot.Leases...) {
		if lease.Phase == "deleted" && lease.JITState == "removed" {
			continue
		}
		if lease.Phase != "deleted" && lease.Phase != "deleting" && (!e.snapshot.Pool.Enabled || !e.now().Before(lease.Deadline)) {
			reason := "lease deadline reached"
			if !e.snapshot.Pool.Enabled {
				reason = "runner pool disabled"
			}
			updated, err := e.update(ctx, lease, "failed", leaseEvent{Error: reason})
			if err != nil {
				return err
			}
			lease = updated
		}
		switch lease.Phase {
		case "deleting", "deleted":
			if err := e.cleanup(ctx, lease); err != nil {
				return err
			}
		case "reserved", "provisioning", "uncertain":
			if !e.haveDemand {
				continue
			}
			if lease.AcquireState == "pending" {
				if err := e.acquire(ctx, lease); err != nil {
					return err
				}
				continue
			}
			if e.demand == 0 {
				if _, err := e.update(ctx, lease, "failed", leaseEvent{Error: "queued demand withdrawn"}); err != nil {
					return err
				}
				continue
			}
			updated, err := e.store.EnsureVM(ctx, lease.ID)
			if errors.Is(err, errCentralContended) {
				// Central refused because another transaction held a row it needed. Frappe
				// documents 508 as "a concurrent transaction is blocking this one, retry
				// later", and the lease is untouched, so waiting one tick is the whole
				// recovery. Returning the error instead tore down the message session, which
				// unregistered the scale set from GitHub and stalled every queued job.
				continue
			}
			if err != nil {
				return err
			}
			e.remember(updated)
		case "vm_ready":
			if e.haveDemand && e.demand == 0 {
				if _, err := e.update(ctx, lease, "failed", leaseEvent{Error: "queued demand withdrawn before registration"}); err != nil {
					return err
				}
				continue
			}
			if err := e.startFreshRunner(ctx, lease); err != nil {
				return err
			}
		case "jit_pending":
			// A prior process may have registered the runner and lost the
			// response. Retire by its stable name instead of generating twice.
			if _, err := e.update(ctx, lease, "failed", leaseEvent{Error: "interrupted JIT creation; retiring uncertain registration"}); err != nil {
				return err
			}
		case "jit_ready":
			state, err := e.guest.Call(ctx, "status", lease, "")
			if err != nil {
				return err
			}
			if state == "running" || state == "completed" {
				if _, err := e.update(ctx, lease, "bootstrapped", leaseEvent{}); err != nil {
					return err
				}
			} else {
				if _, err := e.update(ctx, lease, "failed", leaseEvent{Error: "JIT secret lost before confirmed bootstrap; retiring lease"}); err != nil {
					return err
				}
			}
		case "running":
			state, err := e.guest.Call(ctx, "status", lease, "")
			if err != nil {
				return err
			}
			if state == "unknown" || state == "installed" {
				return errors.New("running lease has no confirmed guest runner; deadline cleanup remains active")
			}
			// A stopped service is not evidence of workflow success. Wait for
			// the durable job event, bounded by the server-owned deadline.
		default:
			return fmt.Errorf("unsupported Central lease phase %q", lease.Phase)
		}
	}
	if e.snapshot.Pool.Enabled && e.haveDemand && e.demand > 0 && !e.occupied() {
		_, err := e.reserve(ctx)
		return err
	}
	return nil
}

func (e *pilotEngine) startFreshRunner(ctx context.Context, lease pilotLease) error {
	ctx, cancel := context.WithDeadline(ctx, lease.Deadline)
	defer cancel()
	lease.cachePool = e.snapshot.Pool
	state, err := e.guest.Call(ctx, "prepare", lease, "")
	if err != nil {
		return err
	}
	if state != "installed" {
		return errors.New("fresh guest preparation did not confirm an unregistered runner")
	}
	lease, err = e.update(ctx, lease, "jit_requested", leaseEvent{})
	if err != nil {
		return err
	}
	jit, err := e.github.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: lease.RunnerName, WorkFolder: guestWorkFolder}, e.snapshot.Pool.ScaleSetID)
	if err != nil {
		_, recordErr := e.update(ctx, lease, "failed", leaseEvent{Error: "JIT request outcome uncertain; registration requires cleanup"})
		if recordErr != nil {
			return recordErr
		}
		return errors.New("JIT request failed; no retry on this lease")
	}
	if jit == nil || jit.Runner == nil || jit.Runner.ID <= 0 || jit.Runner.Name != lease.RunnerName || jit.EncodedJITConfig == "" {
		return errors.New("JIT response identity invalid; lease remains pending for cleanup")
	}
	lease, err = e.update(ctx, lease, "jit_ready", leaseEvent{RunnerID: jit.Runner.ID})
	if err != nil {
		return err
	}
	state, err = e.guest.Call(ctx, "bootstrap", lease, jit.EncodedJITConfig)
	jit.EncodedJITConfig = ""
	if err != nil {
		return errors.New("guest bootstrap outcome uncertain; observe service before any recovery")
	}
	if state != "running" && state != "completed" {
		return errors.New("guest bootstrap not confirmed; lease retained for observation")
	}
	_, err = e.update(ctx, lease, "bootstrapped", leaseEvent{})
	return err
}

func (e *pilotEngine) cleanup(ctx context.Context, lease pilotLease) error {
	if lease.VMID != "" && lease.Phase != "deleted" {
		_ = e.collect(ctx, lease)
		_, _ = e.guest.Call(ctx, "stop", lease, "")
	}
	var githubErr error
	if lease.JITState != "removed" {
		var runner *scaleset.RunnerReference
		runner, githubErr = e.github.GetRunnerByName(ctx, lease.RunnerName)
		if githubErr == nil && runner != nil {
			if runner.Name != lease.RunnerName || (runner.RunnerScaleSetID != 0 && runner.RunnerScaleSetID != e.snapshot.Pool.ScaleSetID) || (lease.RunnerID != 0 && runner.ID != lease.RunnerID) {
				githubErr = errors.New("runner cleanup identity mismatch")
			} else {
				githubErr = e.github.RemoveRunner(ctx, int64(runner.ID))
				// Resolve an uncertain delete by observation; never mark clean
				// merely because DELETE was accepted or a request timed out.
				observed, err := e.github.GetRunnerByName(ctx, lease.RunnerName)
				if err == nil && observed == nil {
					githubErr = nil
				} else if githubErr == nil {
					githubErr = errors.New("runner absence not confirmed")
				}
			}
		}
		if githubErr == nil {
			updated, err := e.update(ctx, lease, "runner_removed", leaseEvent{})
			if err != nil {
				return err
			}
			lease = updated
		}
	}
	// VM cleanup must proceed even if GitHub is unavailable; Central retains
	// the admission slot until both resources are confirmed absent.
	if lease.Phase != "deleted" {
		updated, err := e.store.DeleteVM(ctx, lease.ID)
		if err != nil {
			return err
		}
		e.remember(updated)
	}
	if githubErr != nil {
		return errors.New("GitHub runner cleanup remains unconfirmed")
	}
	return nil
}

func (e *pilotEngine) collect(ctx context.Context, lease pilotLease) error {
	// Diagnostics are best-effort. They must not extend a job or hold its
	// completion/cleanup hostage to an unavailable administrative channel.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := e.guest.Call(ctx, "collect", lease, "")
	return err
}
