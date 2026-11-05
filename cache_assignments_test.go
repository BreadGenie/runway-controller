package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type fakeAcquirer struct{ requested [][]int64 }

func (fake *fakeAcquirer) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	fake.requested = append(fake.requested, ids)
	return ids, nil
}

func scaleSetJob(jobID string, run int64, repo string) scaleset.JobMessageBase {
	owner, name, _ := strings.Cut(repo, "/")
	return scaleset.JobMessageBase{JobID: jobID, WorkflowRunID: run, OwnerName: owner, RepositoryName: name, EventName: "workflow_dispatch", RunnerRequestID: 0, JobWorkflowRef: repo + "/.github/workflows/server-tests.yml@refs/heads/main", JobDisplayName: "Integration (mariadb, 1)"}
}

func TestCacheAssignmentScalerRecordsHostObservedAssignments(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	acquirer := &fakeAcquirer{}
	scaler := &cacheAssignmentScaler{dir: dir, repository: fixtureRepository, client: acquirer, log: func(map[string]any) {}, now: time.Now}
	files := cacheAssignmentFiles{dir: dir}
	job := "b28439b9-7035-5ae5-9219-9c19b1e97956"
	if _, err := files.job(job); !errors.Is(err, errCacheJobNotFound) {
		t.Fatal("unseen job reported", err)
	}
	available := scaleSetJob(job, 41890164615, fixtureRepository)
	available.RunnerRequestID = 7
	err := scaler.Scale(t.Context(), &scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: available}},
		JobAssignedMessages:  []*scaleset.JobAssigned{{JobMessageBase: scaleSetJob(job, 41890164615, fixtureRepository)}, {JobMessageBase: scaleSetJob("0a60a1aa-a79e-5fb1-a32f-8c1a8c35ff75", 1, "someone/else")}},
	})
	if err != nil || len(acquirer.requested) != 1 || acquirer.requested[0][0] != 7 {
		t.Fatal("available job not acquired", err, acquirer.requested)
	}
	if record, err := files.job(job); err != nil || record.WorkflowRunID != 41890164615 || record.JobDisplayName != "Integration (mariadb, 1)" {
		t.Fatal("assigned job not recorded", record, err)
	}
	if records, err := files.jobs(41890164615); err != nil || len(records) != 1 || records[0].JobID != job {
		t.Fatal("run's job records not listed", records, err)
	}
	if _, err := files.job("0a60a1aa-a79e-5fb1-a32f-8c1a8c35ff75"); !errors.Is(err, errCacheJobNotFound) {
		t.Fatal("foreign repository recorded", err)
	}
	if _, err := files.assignment(224); !errors.Is(err, errCacheJobNotFound) {
		t.Fatal("runner assignment before JobStarted", err)
	}
	started := &scaleset.JobStarted{RunnerID: 224, RunnerName: "runway-cache-test-1104-rc-e", JobMessageBase: scaleSetJob(job, 41890164615, fixtureRepository)}
	if err := scaler.Scale(t.Context(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{started}}); err != nil {
		t.Fatal(err)
	}
	record, err := files.assignment(224)
	if err != nil || record.JobID != job || record.RunnerName != "runway-cache-test-1104-rc-e" || record.Completed {
		t.Fatal("JobStarted not recorded", record, err)
	}
	// A runner never changes job; a UUID never moves to another run.
	moved := &scaleset.JobStarted{RunnerID: 224, RunnerName: "runway-cache-test-1104-rc-e", JobMessageBase: scaleSetJob("0a60a1aa-a79e-5fb1-a32f-8c1a8c35ff75", 41890164615, fixtureRepository)}
	scaler.Scale(t.Context(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{moved}, JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: scaleSetJob(job, 99, fixtureRepository)}}})
	if again, _ := files.assignment(224); again.JobID != job {
		t.Fatal("runner assignment replaced")
	}
	if again, _ := files.job(job); again.WorkflowRunID != 41890164615 {
		t.Fatal("job moved to another run")
	}
	completed := &scaleset.JobCompleted{RunnerID: 224, RunnerName: "runway-cache-test-1104-rc-e", Result: "succeeded", JobMessageBase: scaleSetJob(job, 41890164615, fixtureRepository)}
	scaler.Scale(t.Context(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{completed}})
	if done, _ := files.assignment(224); !done.Completed {
		t.Fatal("completion not recorded")
	}
	if os.Chmod(dir, 0755) != nil {
		t.Fatal("chmod")
	}
	if _, err := files.assignment(224); err == nil {
		t.Fatal("record read from a shared directory")
	}
}

func TestCacheAssignmentsConfigurationIsStrict(t *testing.T) {
	valid := `{"version":1,"repository_url":"https://github.com/BreadGenie/frappe","scale_set_id":39,"github_token_file":"/etc/runway-cache/github-token","assignment_dir":"/run/runway-cache/assignments","max_runners":4}`
	if _, err := decodeCacheAssignmentsConfig([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"duplicate":    strings.Replace(valid, `"scale_set_id":39`, `"scale_set_id":39,"scale_set_id":40`, 1),
		"unknown":      strings.Replace(valid, `"max_runners":4`, `"max_runners":4,"x":1`, 1),
		"foreign host": strings.Replace(valid, `https://github.com/`, `https://example.com/`, 1),
		"no scale set": strings.Replace(valid, `"scale_set_id":39`, `"scale_set_id":0`, 1),
		"relative dir": strings.Replace(valid, `"/run/runway-cache/assignments"`, `"assignments"`, 1),
		"too many":     strings.Replace(valid, `"max_runners":4`, `"max_runners":65`, 1),
		"version":      strings.Replace(valid, `"version":1`, `"version":2`, 1),
	} {
		if _, err := decodeCacheAssignmentsConfig([]byte(bad)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestCacheAssignmentRecordsArePrunedByAge(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "224.json"), filepath.Join(dir, "job-b28439b9-7035-5ae5-9219-9c19b1e97956.json")
	for _, path := range []string{old, fresh, old + ".tmp"} {
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	stale := now.Add(-cacheAssignmentRetention - time.Minute)
	if os.Chtimes(old, stale, stale) != nil || os.Chtimes(old+".tmp", stale, stale) != nil {
		t.Fatal("chtimes")
	}
	pruneCacheAssignments(dir, now, cacheAssignmentRetention)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale record kept")
	}
	if _, err := os.Stat(old + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("stale temporary kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh record pruned")
	}
}
