package main

import (
	"context"
	"strings"
	"testing"
)

func TestPartialRunwayAppCredentialsDoNotFallbackToGH(t *testing.T) {
	variables := []string{"RUNWAY_GITHUB_APP_CLIENT_ID", "RUNWAY_GITHUB_APP_INSTALLATION_ID", "RUNWAY_GITHUB_APP_PRIVATE_KEY_FILE"}
	for _, supplied := range variables {
		t.Run(supplied, func(t *testing.T) {
			for _, variable := range variables {
				t.Setenv(variable, "")
			}
			t.Setenv(supplied, "123")
			// No gh executable is available: partial App config must fail as
			// such, rather than unexpectedly using an operator's login.
			t.Setenv("PATH", "")
			client, err := authenticatedClient(context.Background(), "https://github.com/test/repo")
			if client != nil || err == nil || !strings.Contains(err.Error(), "provide all three RUNWAY_GITHUB_APP_*") {
				t.Fatalf("unexpected partial App configuration result: %v", err)
			}
		})
	}
}

// The CLI must not create a scale set name that Central would then refuse on a pool, since
// that would only be discovered at pool save. These are the same values the pool's own
// validation refuses.
func TestScaleSetNameMatchesWhatAPoolAccepts(t *testing.T) {
	accepted := []string{"runway-dogfood", "runway-frappe", "runway-4vcpu-8gb", "runway-a"}
	for _, name := range accepted {
		if !scaleSetName.MatchString(name) {
			t.Errorf("pool would accept %q, but the CLI refuses it", name)
		}
	}
	refused := []string{"dogfood", "Runway-4vcpu-8gb", "runway_4vcpu_8gb", "runway-4VCPU-8GB", "runway", "", "runway-"}
	for _, name := range refused {
		if scaleSetName.MatchString(name) {
			t.Errorf("the CLI would create %q, but a pool refuses it", name)
		}
	}
}
