package main

import (
	"strings"
	"testing"

	"github.com/BreadGenie/runway/cache/grant"
)

func TestCacheWorkflowRunRefMustComeFromAuthenticatedRun(t *testing.T) {
	value := func(s string) *string { return &s }
	context := grant.SetupContext{RunID: value("12"), RunAttempt: value("1"), EventName: value("workflow_dispatch"), Ref: value("refs/heads/feature"), SHA: value(strings.Repeat("a", 40))}
	valid := `{"id":12,"run_attempt":1,"event":"workflow_dispatch","head_branch":"feature","head_sha":"` + strings.Repeat("a", 40) + `","repository":{"id":123},"head_repository":{"id":123}}`
	for _, test := range []struct {
		name, body string
		pass       bool
	}{
		{"exact", valid, true},
		{"different run", strings.Replace(valid, `"id":12,`, `"id":13,`, 1), false},
		{"rerun", strings.Replace(valid, `"run_attempt":1`, `"run_attempt":2`, 1), false},
		{"event", strings.Replace(valid, `workflow_dispatch`, `pull_request`, 1), false},
		{"branch", strings.Replace(valid, `"feature"`, `"default"`, 1), false},
		{"sha", strings.Replace(valid, strings.Repeat("a", 40), strings.Repeat("b", 40), 1), false},
		{"repository", strings.Replace(valid, `"repository":{"id":123}`, `"repository":{"id":124}`, 1), false},
		{"fork", strings.Replace(valid, `"head_repository":{"id":123}`, `"head_repository":{"id":124}`, 1), false},
		{"missing branch", strings.Replace(valid, `"head_branch":"feature"`, `"head_branch":null`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validateCacheWorkflowRun([]byte(test.body), 123, 12, context); (got == nil) != test.pass {
				t.Fatalf("wrong run authorization outcome: %v", got)
			}
		})
	}
}
