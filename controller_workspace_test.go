package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// recordingPilotGitHub captures the settings the engine hands the scaleset SDK.
// It embeds the shared fake rather than replacing it so the JIT invariants under
// test (durable intent before registration, identity echo) stay authoritative.
type recordingPilotGitHub struct {
	*fakePilotGitHub
	requested *scaleset.RunnerScaleSetJitRunnerSetting
}

func (g *recordingPilotGitHub) GenerateJitRunnerConfig(ctx context.Context, settings *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	g.requested = settings
	return g.fakePilotGitHub.GenerateJitRunnerConfig(ctx, settings, id)
}

// workspaceDepthToHome counts the "../" hops from the checkout workspace back to
// the runner's home directory. actions/setup-node caches the yarn store as
// ../../<...>/.cache/yarn/v6 relative to the workspace, so this depth is baked
// into every archive member and must be identical on both runner types or the
// restore lands outside the tree tar can create.
func workspaceDepthToHome(workFolder, home, repo string) int {
	workspace := filepath.Join(workFolder, repo, repo)
	depth := 0
	for current := workspace; current != home; current = filepath.Dir(current) {
		if current == "/" || current == "." || current == "" {
			return -1
		}
		depth++
	}
	return depth
}

// TestJitRequestsGitHubHostedCompatibleWorkFolder pins the SDK contract: the
// work folder must be the absolute GitHub-hosted path so cache archives written
// here restore on GitHub-hosted runners and vice versa.
func TestJitRequestsGitHubHostedCompatibleWorkFolder(t *testing.T) {
	s := newPilotStore()
	prepareVM(t, s)
	github := &recordingPilotGitHub{fakePilotGitHub: &fakePilotGitHub{store: s}}
	snap, _ := s.Renew(context.Background(), "")
	e := &pilotEngine{store: s, github: github, guest: &fakeGuestTransport{}, snapshot: snap, now: time.Now, haveDemand: true, demand: 1}

	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.requested == nil {
		t.Fatal("no JIT config was requested")
	}
	got := github.requested.WorkFolder

	if got != "/home/runner/work" {
		t.Fatalf("work folder = %q, want the absolute GitHub-hosted layout /home/runner/work", got)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("work folder %q must be absolute so the cache prefix does not depend on the runner install root", got)
	}
	// Guards the exact regression: the relative "_work" default resolved under
	// the runner install root and produced a different cache prefix depth.
	if strings.HasPrefix(got, "_") || !filepath.IsAbs(got) {
		t.Fatalf("work folder %q must not be runner-relative; that was the cache-incompatible layout", got)
	}
}

// TestJitWorkFolderMatchesGitHubHostedCacheLayout asserts the reason, not just the
// literal: the workspace depth below the runner home must equal GitHub-hosted's,
// because that depth is what setup-node encodes into the yarn cache archive.
// A mismatch makes every member fail with "Cannot mkdir", which the restore
// reports as a warning while the step still passes.
func TestJitWorkFolderMatchesGitHubHostedCacheLayout(t *testing.T) {
	const (
		home = "/home/runner"
		repo = "frappe"
	)
	// Layouts observed in the comparison logs for the same job and revision.
	const (
		githubHostedWorkFolder = "/home/runner/work"
		legacyRelativeWork     = "/home/runner/actions-runner/_work" // "_work" under the install root
	)

	want := workspaceDepthToHome(githubHostedWorkFolder, home, repo)
	legacy := workspaceDepthToHome(legacyRelativeWork, home, repo)

	if want != 3 {
		t.Fatalf("GitHub-hosted workspace depth = %d, want 3 for %s", want, githubHostedWorkFolder)
	}
	if got := workspaceDepthToHome(guestWorkFolder, home, repo); got != want {
		t.Fatalf("engine work folder depth = %d, want %d to match GitHub-hosted", got, want)
	}
	if legacy == want {
		t.Fatalf("legacy relative layout depth %d must differ from %d for this test to be meaningful", legacy, want)
	}

	// Both layouts resolve the same ".." prefix, but only the pinned one lands
	// on the home directory that holds .cache/yarn.
	for _, folder := range []string{githubHostedWorkFolder, guestWorkFolder} {
		prefix := strings.Repeat("../", workspaceDepthToHome(folder, home, repo))
		if got := filepath.Clean(filepath.Join(folder, repo, repo, prefix, ".cache/yarn")); got != filepath.Join(home, ".cache/yarn") {
			t.Fatalf("%s resolves cache prefix %q to %q, want %q", folder, prefix, got, filepath.Join(home, ".cache/yarn"))
		}
	}
}
