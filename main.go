// runway-controller prepares an explicitly selected GitHub runner scale set.
// It is operator tooling for a Frappe Cloud runner proof of concept.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
)

var repositoryURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// scaleSetName matches what Central accepts as a Runner Pool scale set name, so a name the
// CLI will create is a name the pool will accept.
var scaleSetName = regexp.MustCompile(`^runway-[a-z0-9-]{1,56}$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	action := flag.String("action", "", "create, lookup, jit, listen, controller, cache-data, cache-service, cache-host, cache-assignments, cache-reconcile, or delete (all except lookup and cache-* modify GitHub)")
	config := flag.String("config", "", "private configuration JSON for controller, cache-data, cache-service, cache-host, cache-assignments, or cache-reconcile")
	vmID := flag.String("vm-id", "", "standalone VM whose vsock sockets cache-host serves")
	url := flag.String("url", "", "https://github.com/OWNER/REPO; repository scope only")
	name := flag.String("name", "", "runway-prefixed scale-set name for create/lookup; runner name for jit/listen")
	id := flag.Int("id", 0, "existing scale-set ID for jit/listen/delete")
	out := flag.String("out", "", "new private file for jit; never printed")
	journal := flag.String("journal", "", "durable single-job journal for listen; parent directory must be private")
	timeout := flag.Duration("timeout", 20*time.Minute, "maximum listener duration")
	flag.Parse()
	if *action == "cache-reconcile" {
		if flag.NArg() != 0 {
			return errors.New("cache-reconcile accepts no positional arguments")
		}
		if *config == "" {
			*config = "/etc/runway/cache-reconcile.json"
		}
		return runCacheReconcile(*config)
	}
	if *action == "cache-service-enrol" || *action == "cache-service-rotate" {
		if *config == "" || flag.NArg() != 0 {
			return errors.New("cache credential management requires --config")
		}
		return manageCacheCredential(*config, *action == "cache-service-rotate")
	}
	if *action == "cache-service-check" || *action == "cache-service-init" {
		if *config == "" || flag.NArg() != 0 {
			return errors.New("cache-service initialization requires --config")
		}
		return prepareCacheService(*config, *action == "cache-service-check")
	}
	if *action == "cache-service" {
		if *config == "" || flag.NArg() != 0 {
			return errors.New("cache-service requires --config and no positional arguments")
		}
		return runCacheService(*config)
	}
	if *action == "cache-data" {
		if *config == "" || flag.NArg() != 0 {
			return errors.New("cache-data requires --config and no positional arguments")
		}
		return runCacheDataService(*config)
	}
	if *action == "cache-assignments" {
		if *config == "" || flag.NArg() != 0 {
			return errors.New("cache-assignments requires --config and no positional arguments")
		}
		return runCacheAssignments(*config)
	}
	if *action == "cache-host" {
		if *config == "" || *vmID == "" || flag.NArg() != 0 {
			return errors.New("cache-host requires --config, --vm-id and no positional arguments")
		}
		return runCacheHost(*config, *vmID)
	}
	if *action == "controller" {
		if *config == "" {
			return errors.New("controller requires --config")
		}
		return runController(*config)
	}
	if !repositoryURL.MatchString(*url) {
		return errors.New("--url must identify one github.com repository")
	}
	switch *action {
	case "create", "lookup":
		if !scaleSetName.MatchString(*name) {
			return errors.New("--name must begin runway- and contain lowercase letters, numbers or hyphens (max 63 characters)")
		}
	case "jit":
		if *id <= 0 || *out == "" || !scaleSetName.MatchString(*name) {
			return errors.New("jit requires positive --id, new --out file, and runway-prefixed --name")
		}
	case "delete":
		if *id <= 0 {
			return errors.New("delete requires positive --id")
		}
	case "listen":
		if *id <= 0 || *journal == "" || *timeout <= 0 || !scaleSetName.MatchString(*name) {
			return errors.New("listen requires --id, --journal, positive --timeout, and the exact runway-prefixed JIT runner --name")
		}
	default:
		return errors.New("select --action create, lookup, jit, listen, or delete")
	}
	// Reserve the secret destination before any API call, so an existing file
	// or symlink cannot be overwritten after a runner has been registered.
	var secret *os.File
	if *action == "jit" {
		var err error
		secret, err = os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("reserve JIT output: %w", err)
		}
		defer secret.Close()
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	duration := 90 * time.Second
	if *action == "listen" {
		duration = *timeout
	}
	ctx, cancel := context.WithTimeout(signalCtx, duration)
	defer cancel()
	client, err := authenticatedClient(ctx, *url)
	if err != nil {
		return err
	}
	switch *action {
	case "create":
		set, err := client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: *name, RunnerGroupID: 1,
			Labels:        []scaleset.Label{{Type: "System", Name: *name}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return errors.New("scale-set create failed; outcome may be uncertain: lookup by name before retrying")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": set.ID, "name": set.Name})
	case "lookup":
		set, err := client.GetRunnerScaleSet(ctx, 1, *name)
		if err != nil {
			return errors.New("scale-set lookup failed")
		}
		if set == nil {
			return errors.New("scale set not found")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": set.ID, "name": set.Name, "statistics": set.Statistics})
	case "jit":
		jit, err := client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: *name, WorkFolder: guestWorkFolder}, *id)
		if err != nil {
			return errors.New("JIT generation failed; keep this runner name reserved and reconcile its registration before retrying")
		}
		if jit.EncodedJITConfig == "" || jit.Runner == nil {
			return errors.New("GitHub returned an incomplete JIT response")
		}
		if err := writeSecret(secret, jit.EncodedJITConfig); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"runner_id": jit.Runner.ID, "runner_name": jit.Runner.Name, "scale_set_id": *id, "jit_file": *out})
	case "delete":
		if err := client.DeleteRunnerScaleSet(ctx, *id); err != nil {
			return errors.New("scale-set deletion failed; verify GitHub state before retrying")
		}
		fmt.Println("scale set deleted; guest teardown is a separate operation")
	case "listen":
		return listenOne(ctx, client, *url, *id, *name, *journal)
	}
	return nil
}

func writeSecret(file *os.File, secret string) error {
	if _, err := io.WriteString(file, secret+"\n"); err != nil {
		return fmt.Errorf("write JIT secret: %w", err)
	}
	return file.Sync()
}

func authenticatedClient(ctx context.Context, url string, options ...scaleset.HTTPOption) (*scaleset.Client, error) {
	return authenticatedControllerClient(ctx, url, nil, options...)
}

func authenticatedControllerClient(ctx context.Context, url string, rest **http.Client, options ...scaleset.HTTPOption) (*scaleset.Client, error) {
	appID := os.Getenv("RUNWAY_GITHUB_APP_CLIENT_ID")
	keyFile := os.Getenv("RUNWAY_GITHUB_APP_PRIVATE_KEY_FILE")
	installation := os.Getenv("RUNWAY_GITHUB_APP_INSTALLATION_ID")
	hasApp := appID != "" || keyFile != "" || installation != ""
	// The standalone launcher passes the root-only host credential by path.
	if tokenFile := os.Getenv("RUNWAY_GITHUB_TOKEN_FILE"); tokenFile != "" {
		if hasApp {
			return nil, errors.New("select one GitHub credential source")
		}
		if rest != nil {
			client, err := newCacheRepositoryClient(tokenFile)
			if err != nil {
				return nil, err
			}
			*rest = client
		}
		return cacheGitHubClient(url, tokenFile, options...)
	}
	if hasApp {
		id, err := strconv.ParseInt(installation, 10, 64)
		if err != nil || id <= 0 || appID == "" || keyFile == "" {
			return nil, errors.New("provide all three RUNWAY_GITHUB_APP_* variables")
		}
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, errors.New("cannot read GitHub App private key file")
		}
		if rest != nil {
			client, err := newControllerAppREST(appID, id, key)
			if err != nil {
				return nil, err
			}
			*rest = client
		}
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: url,
			GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: appID, InstallationID: id, PrivateKey: string(key)},
		}, options...)
	}
	// gh resolves its credential store; no token appears in argv or stdout.
	token, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return nil, errors.New("gh authentication unavailable; configure gh or RUNWAY_GITHUB_APP_* variables")
	}
	if rest != nil {
		*rest = &http.Client{Transport: cacheRepositoryCredential{token: strings.TrimSpace(string(token)), transport: http.DefaultTransport}}
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: url, PersonalAccessToken: strings.TrimSpace(string(token)),
	}, options...)
}
