package grant

import (
	"strings"
	"testing"
	"time"
)

func text(value string) *string { return &value }

const testNonce = "8db8f716-c5da-40e2-9f0a-fb0e4e2bc40d"

func TestSetupWireStrict(t *testing.T) {
	request := SetupRequest{Version: 2, Nonce: testNonce}
	encoded := string(mustJSON(t, request))
	if _, err := DecodeSetup([]byte(encoded)); err != nil {
		t.Fatal("valid setup refused", err)
	}
	for _, bad := range []string{
		strings.Replace(encoded, `"version":2`, `"version":2,"job_id":"18259e53-743c-49b8-a55b-59564db482a1"`, 1),
		strings.Replace(encoded, `"version":2`, `"version":2,"request_id":0`, 1),
		strings.Replace(encoded, `"version":2`, `"version":null`, 1),
		strings.Replace(encoded, `"version":2`, `"version":1`, 1),
		strings.Replace(encoded, `"version":2`, `"version":2,"version":2`, 1),
		strings.Replace(encoded, `"repository":null`, `"repository":null,"repository":null`, 1),
		strings.Replace(encoded, `"repository":null`, `"Repository":null`, 1),
		strings.Replace(encoded, `"repository":null`, `"repository":{"token":"secret"}`, 1),
		strings.Replace(encoded, `"run_id":null,`, ``, 1),
		strings.Replace(encoded, `"repository":null`, `"repository":"`+strings.Repeat("a", 2049)+`"`, 1),
		encoded + encoded,
	} {
		if _, err := DecodeSetup([]byte(bad)); err == nil {
			t.Fatal("malformed setup accepted")
		}
	}
}

func TestSetupDerivesScopeOnlyForSupportedVerifiedContext(t *testing.T) {
	key, claims, clock, verifier := fixture(t)
	a := Authorization{
		RegionID: claims.RegionID, JobID: claims.JobID, RunID: 123, RunAttempt: 1, Deadline: clock.Earliest.Add(5 * time.Minute),
		ReadRefs: []string{"refs/heads/unrelated"}, WriteRef: "refs/heads/unrelated",
	}
	policy := RepositoryPolicy{RepositoryID: 456, Repository: "owner/repo", DefaultBranch: "main"}
	request := SetupRequest{Version: 2, Nonce: testNonce, Context: SetupContext{
		Repository: text("owner/repo"), RepositoryID: text("456"), RunID: text("123"), RunAttempt: text("1"), Ref: text("refs/heads/feature"), RefType: text("branch"), SHA: text(strings.Repeat("a", 40)), EventName: text("workflow_dispatch"),
	}}
	token, err := IssueSetup(key, claims.Audience, a, policy, request, clock)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifier.Verify(token, clock)
	if err != nil || strings.Join(got.ReadRefs, ",") != "refs/heads/feature,refs/heads/main" || got.WriteRef != "refs/heads/feature" {
		t.Fatal("unexpected scope", err)
	}
	for _, change := range []func(*SetupRequest){
		func(r *SetupRequest) { r.Context.Repository = text("other/repo") },
		func(r *SetupRequest) { r.Context.RepositoryID = text("457") },
		func(r *SetupRequest) { r.Context.RepositoryID = text("0456") },
		func(r *SetupRequest) { r.Context.RunID = text("124") },
		func(r *SetupRequest) { r.Context.RunAttempt = text("2") },
		func(r *SetupRequest) { r.Context.EventName = text("pull_request_target") },
		func(r *SetupRequest) { r.Context.EventName = text("pull_request") },
		func(r *SetupRequest) { r.Context.RefType = text("tag") },
		func(r *SetupRequest) { r.Context.BaseRef = text("main") },
		func(r *SetupRequest) { r.Context.SHA = nil },
	} {
		bad := request
		change(&bad)
		if _, err := IssueSetup(key, claims.Audience, a, policy, bad, clock); err == nil {
			t.Fatal("unqualified or mismatched context issued a grant")
		}
	}
	policy.RepositoryID++
	if _, err := IssueSetup(key, claims.Audience, a, policy, request, clock); err == nil {
		t.Fatal("policy from another repository accepted")
	}
}
