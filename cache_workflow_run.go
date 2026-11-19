package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/BreadGenie/runway-controller/cache/grant"
)

// verifyCacheWorkflowRun binds the untrusted frozen context to GitHub's exact run.
func verifyCacheWorkflowRun(ctx context.Context, client *http.Client, policy grant.RepositoryPolicy, runID int64, frozen grant.SetupContext) error {
	if client == nil || runID <= 0 {
		return errCacheRepository
	}
	data, err := readCacheGitHubMetadata(ctx, client, "/repos/"+policy.Repository+"/actions/runs/"+strconv.FormatInt(runID, 10))
	if err != nil {
		return err
	}
	return validateCacheWorkflowRun(data, policy.RepositoryID, runID, frozen)
}

func validateCacheWorkflowRun(data []byte, repositoryID, runID int64, frozen grant.SetupContext) error {
	type repository struct {
		ID int64 `json:"id"`
	}
	var run struct {
		ID             int64       `json:"id"`
		RunAttempt     int64       `json:"run_attempt"`
		Event          string      `json:"event"`
		HeadBranch     string      `json:"head_branch"`
		HeadSHA        string      `json:"head_sha"`
		Repository     *repository `json:"repository"`
		HeadRepository *repository `json:"head_repository"`
	}
	if json.Unmarshal(data, &run) != nil || run.ID != runID || run.RunAttempt <= 0 ||
		(run.Event != "push" && run.Event != "workflow_dispatch") || run.HeadBranch == "" || run.HeadSHA == "" ||
		run.Repository == nil || run.Repository.ID != repositoryID || run.HeadRepository == nil || run.HeadRepository.ID != repositoryID ||
		frozen.RunID == nil || *frozen.RunID != strconv.FormatInt(runID, 10) ||
		frozen.RunAttempt == nil || *frozen.RunAttempt != strconv.FormatInt(run.RunAttempt, 10) ||
		frozen.EventName == nil || *frozen.EventName != run.Event ||
		frozen.Ref == nil || *frozen.Ref != "refs/heads/"+run.HeadBranch ||
		frozen.SHA == nil || *frozen.SHA != run.HeadSHA {
		return errCacheRepository
	}
	return nil
}

// cacheWorkflowContext derives context from REST, then applies the same run
// validation used by standalone guests. The workflow ref disambiguates tags.
func cacheWorkflowContext(data []byte, repositoryID, runID int64, workflowRef string) (grant.SetupContext, error) {
	var run struct {
		RunAttempt int64  `json:"run_attempt"`
		Event      string `json:"event"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
	}
	if json.Unmarshal(data, &run) != nil {
		return grant.SetupContext{}, errCacheRepository
	}
	id, attempt, ref := strconv.FormatInt(runID, 10), strconv.FormatInt(run.RunAttempt, 10), "refs/heads/"+run.HeadBranch
	c := grant.SetupContext{RunID: &id, RunAttempt: &attempt, EventName: &run.Event, Ref: &ref, SHA: &run.HeadSHA}
	if !strings.HasSuffix(workflowRef, "@"+ref) {
		return c, errCacheJobMismatch
	}
	return c, validateCacheWorkflowRun(data, repositoryID, runID, c)
}
