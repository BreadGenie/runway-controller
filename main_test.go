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
