package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
)

type fakeBindings struct {
	mu      sync.Mutex
	binding *cacheGuestBinding
}

func (bindings *fakeBindings) lookup(vmID string) (cacheGuestBinding, error) {
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	if bindings.binding == nil {
		return cacheGuestBinding{}, errCacheUnbound
	}
	if bindings.binding.VMID != vmID {
		return cacheGuestBinding{}, grant.ErrInvalid
	}
	return *bindings.binding, nil
}

func (bindings *fakeBindings) set(binding *cacheGuestBinding) {
	bindings.mu.Lock()
	bindings.binding = binding
	bindings.mu.Unlock()
}

type runningCacheHost struct {
	github   *githubFixture
	uds      string
	bindings *fakeBindings
	upstream chan string
	logs     *strings.Builder
	logMu    *sync.Mutex
}

// startCacheHostFixture serves real Unix sockets. The test process is the
// socket peer, so a binding naming os.Getpid() plays the Firecracker process.
func startCacheHostFixture(t *testing.T, test *authorityCase, configure ...func(*cacheHost)) *runningCacheHost {
	t.Helper()
	upstream := make(chan string, 8)
	data := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream <- r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(data.Close)
	dir, err := os.MkdirTemp("/tmp", "rch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	uds := filepath.Join(dir, "v.sock")
	test.binding.VsockUDS, test.binding.FirecrackerPID = uds, os.Getpid()
	authority, github := test.authority(t)
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	logs, logMu := &strings.Builder{}, &sync.Mutex{}
	bindings := &fakeBindings{}
	host := &cacheHost{
		vmID: test.binding.VMID, uds: uds, bindings: bindings, authority: &authority,
		clock: func(context.Context) (grant.Clock, error) {
			now := time.Now()
			return grant.Clock{Earliest: now.Add(-300 * time.Millisecond), Latest: now.Add(300 * time.Millisecond)}, nil
		},
		key: private, audience: data.URL, roots: data.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, archiveBytes: 1 << 20,
		peer: cacheUnixPeerPID,
		log: func(fields map[string]any) {
			encoded, _ := json.Marshal(fields)
			logMu.Lock()
			logs.Write(append(encoded, '\n'))
			logMu.Unlock()
		},
	}
	for _, change := range configure {
		change(host)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- host.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, err := os.Stat(uds + "_1027"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache host did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &runningCacheHost{github: github, uds: uds, bindings: bindings, upstream: upstream, logs: logs, logMu: logMu}
}

func exchangeLine(t *testing.T, path string, request any) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	data, _ := json.Marshal(request)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err == io.EOF && len(line) == 0 {
		return nil // refused: closed without a response
	}
	var response map[string]any
	if err != nil || json.Unmarshal(line, &response) != nil {
		t.Fatalf("bad response %q: %v", line, err)
	}
	return response
}

func (host *runningCacheHost) gate(t *testing.T, jobID string) map[string]any {
	return exchangeLine(t, host.uds+"_1025", map[string]any{"version": 1, "job_id": jobID, "request_id": 0, "nonce": "3d1f7c2a-8b4e-4f6a-9c1d-2e3f4a5b6c7d"})
}

func (host *runningCacheHost) setup(t *testing.T, request grant.SetupRequest) string {
	response := exchangeLine(t, host.uds+"_1026", request)
	if response == nil {
		t.Fatal("setup closed without a response")
	}
	return response["outcome"].(string)
}

func (host *runningCacheHost) lookup(t *testing.T) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", host.uds+"_1027")
	}}}
	response, err := client.Get("http://127.0.0.1:18080/_apis/artifactcache/cache?keys=yarn&version=" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

// logged waits briefly: the host logs a setup after writing its response.
func (host *runningCacheHost) logged(reason string) bool {
	return host.eventually(`"reason":"` + reason + `"`)
}

func (host *runningCacheHost) eventually(text string) bool {
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if strings.Contains(host.logsText(), text) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func TestCacheHostRefusesUnboundGuest(t *testing.T) {
	test := newAuthorityCase(t)
	host := startCacheHostFixture(t, test)
	if response := host.gate(t, test.release.JobID); response != nil {
		t.Fatal("unbound guest released", response)
	}
	if outcome := host.setup(t, test.setup); outcome != "disabled" || !host.logged("unbound") {
		t.Fatal("unbound guest enabled", outcome)
	}
	if status := host.lookup(t); status != http.StatusForbidden {
		t.Fatal("unbound guest reached the translator", status)
	}
}

func TestCacheHostGrantsOnlyTheReleasedVerifiedJob(t *testing.T) {
	test := newAuthorityCase(t)
	host := startCacheHostFixture(t, test)
	binding := test.binding
	host.bindings.set(&binding)
	if outcome := host.setup(t, test.setup); outcome != "disabled" || !host.logged("not_released") {
		t.Fatal("setup before release enabled", outcome)
	}
	response := host.gate(t, test.release.JobID)
	if response == nil || response["released"] != true || int64(response["stop_unix_seconds"].(float64)) != binding.ExpiresAt.Unix() {
		t.Fatal("bound runner not released", response)
	}
	if again := host.gate(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); again != nil {
		t.Fatal("ephemeral runner released a second job")
	}
	otherRun := test.setup
	value := "41867135948"
	otherRun.Context.RunID, otherRun.Nonce = &value, "6f7a8b9c-0d1e-4f2a-8b3c-4d5e6f7a8b9c"
	if outcome := host.setup(t, otherRun); outcome != "disabled" || !host.logged("github_job_mismatch") {
		t.Fatal("setup claiming another run enabled", outcome)
	}
	if outcome := host.setup(t, otherRun); outcome != "disabled" || !host.logged("nonce_replayed") {
		t.Fatal("refused nonce reused", outcome)
	}
	if status := host.lookup(t); status != http.StatusServiceUnavailable {
		t.Fatal("translator installed before a verified setup", status)
	}
	if outcome := host.setup(t, test.setup); outcome != "enabled" {
		t.Fatal("verified job not enabled", outcome)
	}
	if outcome := host.setup(t, test.setup); outcome != "disabled" || !host.logged("already_enabled") {
		t.Fatal("replayed setup accepted", outcome)
	}
	second := test.setup
	second.Nonce = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	if outcome := host.setup(t, second); outcome != "disabled" {
		t.Fatal("a second setup for the release enabled", outcome)
	}
	if status := host.lookup(t); status != http.StatusNoContent {
		t.Fatal("verified lookup not forwarded", status)
	}
	if forwarded := <-host.upstream; !strings.HasPrefix(forwarded, "GET /_apis/artifactcache/cache Bearer ") {
		t.Fatal("translator did not use the signed grant", forwarded)
	}
	// A different peer (another process using the socket) is refused even though bound.
	other := binding
	other.FirecrackerPID = os.Getpid() + 1
	host.bindings.set(&other)
	if status := host.lookup(t); status != http.StatusForbidden || !host.logged("peer_mismatch") {
		t.Fatal("foreign peer reached the translator", status)
	}
	// Teardown removes the binding: the translator stops at once.
	host.bindings.set(nil)
	if status := host.lookup(t); status != http.StatusForbidden {
		t.Fatal("translator survived unbinding", status)
	}
}

func TestCacheHostConfigurationIsStrict(t *testing.T) {
	valid := `{"version":1,"region_id":"blr-1","audience":"https://127.0.0.1:18445","cache_data_ca_file":"/etc/runway-cache/tls/ca.pem","signing_key_file":"/etc/runway-cache/signing-key.pem","github_token_file":"/etc/runway-cache/github-token","binding_dir":"/run/runway-cache/bindings","vm_dir":"/run/runway-cache/vms","assignment_dir":"/run/runway-cache/assignments","archive_bytes":1073741824}`
	if _, err := decodeCacheHostConfig([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"duplicate":     strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"unknown":       strings.Replace(valid, `"version":1`, `"version":1,"central_url":"x"`, 1),
		"plain origin":  strings.Replace(valid, `https://127.0.0.1:18445`, `http://127.0.0.1:18445`, 1),
		"relative":      strings.Replace(valid, `"/run/runway-cache/bindings"`, `"bindings"`, 1),
		"unclean":       strings.Replace(valid, `"/run/runway-cache/vms"`, `"/run/runway-cache/../vms"`, 1),
		"huge archive":  strings.Replace(valid, `1073741824`, `1073741825`, 1),
		"version":       strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"team":          strings.Replace(valid, `"region_id":"blr-1"`, `"region_id":"blr-1","team_id":"standalone"`, 1),
		"empty region":  strings.Replace(valid, `"region_id":"blr-1"`, `"region_id":""`, 1),
		"trailing data": valid + `{}`,
	} {
		if _, err := decodeCacheHostConfig([]byte(bad)); err == nil {
			t.Fatalf("%s configuration accepted", name)
		}
	}
}

func TestCacheHostGateReleasesUnconfirmedJobButSetupFallsBack(t *testing.T) {
	budget := cacheHostConfirmBudget
	cacheHostConfirmBudget = 300 * time.Millisecond
	t.Cleanup(func() { cacheHostConfirmBudget = budget })
	test := newAuthorityCase(t)
	test.binding.RunnerID = 999 // GitHub never shows this runner in the run
	host := startCacheHostFixture(t, test)
	binding := test.binding
	host.bindings.set(&binding)
	started := time.Now()
	if response := host.gate(t, test.release.JobID); response == nil || response["released"] != true {
		t.Fatal("unconfirmed job not released; cache must never block the job")
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond || elapsed > 2*time.Second || !host.eventually(`"event":"confirm_refused"`) {
		t.Fatal("gate confirmation not bounded", elapsed)
	}
	if outcome := host.setup(t, test.setup); outcome != "disabled" || !host.logged("unconfirmed") {
		t.Fatal("unconfirmed job enabled", outcome)
	}
	if status := host.lookup(t); status != http.StatusServiceUnavailable {
		t.Fatal("unconfirmed job reached the translator", status)
	}
	select {
	case forwarded := <-host.upstream:
		t.Fatal("request forwarded without confirmation", forwarded)
	default:
	}
}

func TestCacheHostGateConfirmsBeforeSetup(t *testing.T) {
	test := newAuthorityCase(t)
	host := startCacheHostFixture(t, test)
	binding := test.binding
	host.bindings.set(&binding)
	if host.gate(t, test.release.JobID) == nil || !host.eventually(`"event":"confirmed"`) || !host.eventually(`"source":"rest_jobs"`) {
		t.Fatal("gate did not confirm the assignment")
	}
	if outcome := host.setup(t, test.setup); outcome != "enabled" {
		t.Fatal("confirmed job not enabled", outcome)
	}
}

func (host *runningCacheHost) logsText() string {
	host.logMu.Lock()
	defer host.logMu.Unlock()
	return host.logs.String()
}

// Bug: cache-host exited at startup when the key, CA or credential was
// unreadable, so the gate never answered and the job failed.
func TestCacheHostServesTheGateWhenSetupIsUnavailable(t *testing.T) {
	test := newAuthorityCase(t)
	dir, err := os.MkdirTemp("/tmp", "rch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	config := cacheHostConfig{Version: 1, RegionID: "blr-1", Audience: "https://127.0.0.1:18445", CacheDataCAFile: "/nonexistent/ca.pem", SigningKeyFile: "/nonexistent/key.pem", GitHubTokenFile: "/nonexistent/token", BindingDir: "/nonexistent", VMDir: dir, AssignmentDir: "/nonexistent", ArchiveBytes: 1 << 20}
	if err := os.Mkdir(filepath.Join(dir, test.binding.VMID), 0700); err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	var logMu sync.Mutex
	host := newCacheHost(config, test.binding.VMID, func(fields map[string]any) {
		encoded, _ := json.Marshal(fields)
		logMu.Lock()
		logs.Write(append(encoded, '\n'))
		logMu.Unlock()
	})
	bindings := &fakeBindings{}
	host.bindings = bindings
	binding := test.binding
	binding.VsockUDS, binding.FirecrackerPID = host.uds, os.Getpid()
	bindings.set(&binding)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- host.serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	running := &runningCacheHost{uds: host.uds, bindings: bindings, logs: logs, logMu: &logMu}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(host.uds + "_1027"); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("cache host did not listen")
		}
	}
	if response := running.gate(t, test.release.JobID); response == nil || response["released"] != true {
		t.Fatal("gate not served without setup custody", response)
	}
	if outcome := running.setup(t, test.setup); outcome != "disabled" || !running.logged("signing_key_unavailable") {
		t.Fatal("setup not disabled", outcome, running.logsText())
	}
}

// Bug: renewal ran under the host lock, so a slow GitHub stalled every request.
func TestCacheHostRenewsInTheBackgroundWithoutBlockingRequests(t *testing.T) {
	test := newAuthorityCase(t)
	host := startCacheHostFixture(t, test, func(host *cacheHost) {
		host.renewWindow = 30*time.Minute - 5*time.Second - 3*time.Second // renew ~3 s after setup
	})
	binding := test.binding
	host.bindings.set(&binding)
	if host.gate(t, test.release.JobID) == nil || host.setup(t, test.setup) != "enabled" {
		t.Fatal("setup")
	}
	host.github.mu.Lock()
	host.github.stall = true // renewal now waits for its whole 5 s budget
	host.github.mu.Unlock()
	repository := "/repos/" + fixtureRepository
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		host.github.mu.Lock()
		renewing := host.github.calls[repository] >= 2
		host.github.mu.Unlock()
		if renewing {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("renewal did not start")
		}
	}
	started := time.Now()
	if status := host.lookup(t); status != http.StatusNoContent || time.Since(started) > time.Second {
		t.Fatal("request blocked by renewal", status, time.Since(started))
	}
	for deadline := time.Now().Add(8 * time.Second); !strings.Contains(host.logsText(), `"reason":"github_timeout"`); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("stalled renewal not bounded")
		}
	}
	if status := host.lookup(t); status != http.StatusNoContent {
		t.Fatal("current grant dropped after a failed renewal", status)
	}
}

// Concurrent setups share one confirmation, and excess connections are closed.
func TestCacheHostSharesConfirmationAndBoundsConnections(t *testing.T) {
	budget := cacheHostConfirmBudget
	cacheHostConfirmBudget = 400 * time.Millisecond
	t.Cleanup(func() { cacheHostConfirmBudget = budget })
	test := newAuthorityCase(t)
	delete(test.assignments.jobs, test.release.JobID) // nothing confirms: every caller polls
	host := startCacheHostFixture(t, test)
	binding := test.binding
	host.bindings.set(&binding)
	gated := make(chan map[string]any)
	go func() { gated <- host.gate(t, test.release.JobID) }()
	time.Sleep(50 * time.Millisecond)
	var wg sync.WaitGroup
	for _, nonce := range []string{"8b9c0d1e-2f3a-4b4c-8d5e-6f7a8b9c0d1e", "9c0d1e2f-3a4b-4c5d-8e6f-7a8b9c0d1e2f"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := test.setup
			request.Nonce = nonce
			if outcome := host.setup(t, request); outcome != "disabled" {
				t.Error("unconfirmed setup enabled", outcome)
			}
		}()
	}
	wg.Wait()
	if <-gated == nil {
		t.Fatal("gate refused")
	}
	if refused := strings.Count(host.logsText(), `"event":"confirm_refused"`); refused != 1 {
		t.Fatal("confirmations run:", refused)
	}
	var held []net.Conn
	for range cacheHostSlots[1026] {
		conn, err := net.Dial("unix", host.uds+"_1026")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	time.Sleep(50 * time.Millisecond)
	extra, err := net.Dial("unix", host.uds+"_1026")
	if err != nil {
		t.Fatal(err)
	}
	extra.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("connection beyond the setup slots was not closed", err)
	}
	for _, conn := range append(held, extra) {
		conn.Close()
	}
}
