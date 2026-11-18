package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnershipHeartbeatKeepsSanitizedCentralCauseAndStaysFailClosed(t *testing.T) {
	// Central reports a blocked renewal as HTTP 508 (QueryDeadlockError).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusLoopDetected)
		w.Write([]byte(`{"exception":"test-secret should never be logged"}`))
	}))
	defer server.Close()
	client := &centralRPC{baseURL: server.URL, pool: "pool", owner: "owner", fence: 1, claimedPool: pilotPool{ID: "pool"}, http: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	err := holdOwnership(ctx, func(ctx context.Context) error {
		calls++
		_, err := client.Renew(ctx, "")
		return err
	}, time.Millisecond)

	if err == nil {
		t.Fatal("a blocked renewal must stop ownership")
	}
	if !strings.Contains(err.Error(), "all side effects stopped") {
		t.Fatalf("renewal failure must stay fail-closed: %v", err)
	}
	if !strings.Contains(err.Error(), "508") {
		t.Fatalf("renewal failure dropped the Central cause an operator needs: %v", err)
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("renewal failure leaked a secret: %v", err)
	}
	if calls != 1 {
		t.Fatalf("renewal failure must stop the heartbeat immediately, got %d renewals", calls)
	}
	// Fail-closed must not depend on ctx: the caller is the only thing that cancels.
	if ctx.Err() != nil {
		t.Fatal("renewal failure cancelled the caller context by itself")
	}
}

func TestCentralRenewReportsPoolIdentityChangeCause(t *testing.T) {
	// A genuine fencing loss must stay distinguishable from a blocked transaction.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := &centralRPC{baseURL: server.URL, pool: "pool", owner: "owner", fence: 1, claimedPool: pilotPool{ID: "pool"}, http: server.Client()}
	err := holdOwnership(context.Background(), func(ctx context.Context) error {
		_, err := client.Renew(ctx, "")
		return err
	}, time.Millisecond)
	if err == nil {
		t.Fatal("a rejected renewal must stop ownership")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "all side effects stopped") {
		t.Fatalf("renewal cause must stay distinguishable from a blocked transaction: %v", err)
	}
}

func TestOwnershipHeartbeatCancellationDuringRenewalIsAShutdown(t *testing.T) {
	// Cancellation can land after the heartbeat starts a renewal. The in-flight
	// failure is a shutdown, not a lost fence, so it must not fail closed. The
	// callback blocks until the test confirms cancellation, so the outcome cannot
	// depend on which goroutine happens to run first.
	for attempt := range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		cancelled := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- holdOwnership(ctx, func(context.Context) error {
				select {
				case <-started:
				default:
					close(started)
				}
				<-cancelled
				return errors.New("Central renew_pool rejected (HTTP 508)")
			}, time.Microsecond)
		}()
		<-started
		cancel()
		close(cancelled)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("attempt %d: cancellation during renewal must be a shutdown, got: %v", attempt, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d: ownership did not stop after cancellation", attempt)
		}
	}
}

func TestOwnershipHeartbeatReturnsCleanlyWhenContextEnds(t *testing.T) {
	// A renewal that fails while the controller is still alive is a real lost
	// fence and must stay fail-closed. Cancellation only converts the outcome
	// once it has happened, which the handshake test above pins down. Cancelling
	// here without synchronising would race the callback and assert nothing.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := holdOwnership(ctx, func(context.Context) error {
		return errors.New("Central renew_pool rejected (HTTP 508)")
	}, time.Microsecond)
	if err == nil {
		t.Fatal("a live controller must fail closed on renewal failure")
	}
	if !strings.Contains(err.Error(), "all side effects stopped") {
		t.Fatalf("fail-closed renewal must keep its notice: %v", err)
	}
}

func TestOwnershipHeartbeatFailsClosedWhileContextIsAlive(t *testing.T) {
	// The cancellation guard must not weaken fail-closed behaviour: a real Central
	// failure on a live controller still stops ownership and keeps the cause.
	for attempt := range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := holdOwnership(ctx, func(context.Context) error {
			return errors.New("Central renew_pool rejected (HTTP 508)")
		}, time.Microsecond)
		if err == nil {
			t.Fatalf("attempt %d: a live controller must fail closed on renewal failure", attempt)
		}
		if !strings.Contains(err.Error(), "all side effects stopped") || !strings.Contains(err.Error(), "508") {
			t.Fatalf("attempt %d: fail-closed renewal must keep its sanitized cause: %v", attempt, err)
		}
	}
}

func TestCentralRPCSendsFencingAndKeepsSecretsOutOfErrors(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "token test-key:test-secret" || r.Method != "POST" {
			t.Error("missing API authentication")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		if body["pool"] != "pool" || body["owner_id"] != "owner" {
			t.Error("missing ownership identity")
		}
		if strings.HasSuffix(r.URL.Path, "claim_pool") {
			json.NewEncoder(w).Encode(map[string]any{"message": pilotSnapshot{Fence: 17, Pool: pilotPool{ID: "pool"}}})
			return
		}
		if body["fence"] != float64(17) {
			t.Error("missing current fencing token")
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"exception":"test-secret should never be logged"}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-key:test-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newCentralRPC(controllerConfig{CentralURL: server.URL, CredentialFile: path, Pool: "pool", OwnerID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = client.Update(context.Background(), "lease", "jit_requested", leaseEvent{})
	if !errors.Is(err, errCentralFenceRejected) || strings.Contains(err.Error(), "test-secret") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("bad sanitized error: %v", err)
	}
	if requests != 2 {
		t.Fatal("RPC mutations must not automatically retry")
	}
}

// The labels reached Central only because Update builds a map by hand. Encoding the event
// itself dropped them silently, which is what this asserts: a job's repository and labels
// must survive any encoding of the event, not just this one call path.
func TestLeaseEventEncodingCarriesRepositoryAndLabels(t *testing.T) {
	event := leaseEvent{
		Repository:    "BreadGenie/frappe",
		RequestLabels: "self-hosted\nrunway-2vcpu-6gb",
		RequestID:     42,
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["repository"] != "BreadGenie/frappe" {
		t.Fatalf("repository lost in encoding: %s", encoded)
	}
	if got["request_labels"] != "self-hosted\nrunway-2vcpu-6gb" {
		t.Fatalf("labels lost in encoding: %s", encoded)
	}
	if got["request_id"] != float64(42) {
		t.Fatalf("request id lost in encoding: %s", encoded)
	}
}

func TestControllerConfigRequiresHTTPSExceptLiteralLoopback(t *testing.T) {
	cases := []struct {
		name    string
		central string
		allow   bool
	}{
		{"https host", "https://central.example.com", true},
		{"https port and path", "https://central.example.com:8443/runway", true},
		{"https literal ip", "https://10.0.0.7:8443", true},
		{"https userinfo rejected", "https://key:secret@central.example.com", false},
		{"localhost", "http://localhost:8080", true},
		{"localhost upper case", "HTTP://LOCALHOST:8080", true},
		{"loopback ipv4", "http://127.0.0.1:8080", true},
		{"loopback ipv6", "http://[::1]:8080", true},
		{"http external host", "http://central.example.com", false},
		{"http external host with port", "http://central.example.com:8080", false},
		{"http private lan address", "http://10.0.0.7:8080", false},
		{"http deceptive suffix", "http://localhost.example.com", false},
		{"http deceptive ip suffix", "http://127.0.0.1.example.com", false},
		{"http deceptive prefix", "http://notlocalhost", false},
		{"http trailing dot", "http://localhost.:8080", false},
		{"http other loopback spelling", "http://127.0.0.2:8080", false},
		{"http zone scoped loopback", "http://[::1%25lo]:8080", false},
		{"http userinfo before loopback", "http://central.example.com@localhost:8080", false},
		{"http userinfo remote", "http://key:secret@central.example.com", false},
		{"http unspecified ipv4", "http://0.0.0.0:8080", false},
		{"https unspecified ipv4", "https://0.0.0.0:8443", false},
		{"https unspecified ipv6", "https://[::]:8443", false},
		{"http unspecified ipv4 mapped", "http://[::ffff:0.0.0.0]:8080", false},
		{"query rejected", "https://central.example.com?token=1", false},
		{"empty query rejected", "https://central.example.com?", false},
		{"fragment rejected", "https://central.example.com#frag", false},
		{"malformed rejected", "://central.example.com", false},
		{"scheme missing rejected", "central.example.com", false},
		{"host missing rejected", "https:///runway", false},
		{"unsupported scheme rejected", "ftp://central.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.json")
			config, err := json.Marshal(map[string]any{
				"central_url":             tc.central,
				"central_credential_file": "/srv/runway/private/central.token",
				"pool":                    "pool",
				"transport_command":       []string{"python3", "/srv/runway/scripts/metal-guest-transport.py"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, config, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = loadControllerConfig(path)
			if tc.allow && err != nil {
				t.Fatalf("refused %s: %v", tc.central, err)
			}
			if !tc.allow && err == nil {
				t.Fatalf("accepted %s", tc.central)
			}
		})
	}
}

func TestCentralRPCRefusesPlaintextRemoteEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-key:test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []string{"https://central.example.com", "http://127.0.0.1:8080"} {
		if _, err := newCentralRPC(controllerConfig{CentralURL: allowed, CredentialFile: path, Pool: "pool"}); err != nil {
			t.Fatalf("refused %s: %v", allowed, err)
		}
	}
	for _, refused := range []string{"http://central.example.com", "http://localhost.example.com", "https://0.0.0.0:8443"} {
		if _, err := newCentralRPC(controllerConfig{CentralURL: refused, CredentialFile: path, Pool: "pool"}); err == nil {
			t.Fatalf("accepted %s; the pool token would leave the process unencrypted", refused)
		}
	}
}

func TestCentralRPCDoesNotForwardTokenAcrossRedirects(t *testing.T) {
	leaked := make(chan string, 1)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case leaked <- r.Header.Get("Authorization"):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound)
	}))
	defer redirector.Close()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-key:test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newCentralRPC(controllerConfig{CentralURL: redirector.URL, CredentialFile: path, Pool: "pool"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background()); err == nil {
		t.Fatal("followed a redirect from Central")
	}
	select {
	case header := <-leaked:
		t.Fatalf("Central token reached a redirect target: %q", header)
	default:
	}
}

func TestCentralCredentialRejectsReadableFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("key:secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newCentralRPC(controllerConfig{CentralURL: "https://central.example.com", CredentialFile: path}); err == nil {
		t.Fatal("accepted world-readable credential")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newCentralRPC(controllerConfig{CentralURL: "https://central.example.com", CredentialFile: link}); err == nil {
		t.Fatal("accepted credential symlink")
	}
}

func TestCentralRenewRejectsChangedPoolIdentity(t *testing.T) {
	original := pilotSnapshot{Fence: 1, Pool: pilotPool{ID: "pool", RepositoryURL: "https://github.com/test/repo", ScaleSetID: 7, MaxConcurrency: 1, Region: "region", Team: "team"}}
	mutations := map[string]func(*pilotSnapshot){
		"repository": func(s *pilotSnapshot) { s.Pool.RepositoryURL = "https://github.com/other/repo" },
		"scale set":  func(s *pilotSnapshot) { s.Pool.ScaleSetID = 8 },
		"capacity":   func(s *pilotSnapshot) { s.Pool.MaxConcurrency = 2 },
		"region":     func(s *pilotSnapshot) { s.Pool.Region = "other" },
		"team":       func(s *pilotSnapshot) { s.Pool.Team = "other" },
		"fence":      func(s *pilotSnapshot) { s.Fence = 2 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := original
			mutate(&changed)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"message": changed})
			}))
			defer server.Close()
			client := &centralRPC{baseURL: server.URL, pool: "pool", owner: "owner", fence: 1, claimedPool: original.Pool, http: server.Client()}
			if _, err := client.Renew(context.Background(), ""); err == nil {
				t.Fatal("renew accepted identity incompatible with existing GitHub session/transport")
			}
		})
	}
}

// The health line is the only thing an operator sees when the controller cannot
// open an Actions message session, and that session has failed for a deleted
// scale set, an unusable token and a network blip alike. One generic message for
// all three is what turned a five-minute configuration mistake into an afternoon,
// so the fields an operator needs have to be pinned here.
func TestSessionUnavailableErrorNamesTheScaleSetAndKeepsTheCause(t *testing.T) {
	cause := errors.New("unexpected status code: 404 Not Found")
	got := sessionUnavailableError(7, "pool-abc", cause).Error()

	for _, want := range []string{"7", "pool-abc", "404 Not Found"} {
		if !strings.Contains(got, want) {
			t.Errorf("health line %q does not mention %q", got, want)
		}
	}
	if !strings.Contains(got, "lease deadlines remain enforced") {
		t.Errorf("health line %q drops the reassurance that Central still holds the deadline", got)
	}
	// Wrapped, not flattened: the caller's own inspection has to keep working.
	if !errors.Is(sessionUnavailableError(7, "pool-abc", cause), cause) {
		t.Error("cause is not reachable with errors.Is")
	}
}

func TestSessionUnavailableErrorDistinguishesTwoScaleSets(t *testing.T) {
	cause := errors.New("boom")
	stale := sessionUnavailableError(6, "pool-abc", cause).Error()
	fresh := sessionUnavailableError(8, "pool-abc", cause).Error()
	if stale == fresh {
		t.Error("two scale sets produce identical health lines, so the log cannot say which one failed")
	}
}

func TestSessionUnavailableErrorIsSafeForAnArbitraryCause(t *testing.T) {
	// The SDK's error can carry a response body. Whatever it is, the line has to
	// render it rather than dropping it, because that is the whole point of the
	// change; and a nil cause must not panic a controller that is already failing.
	got := sessionUnavailableError(1, "p", errors.New("body: {\"message\":\"Not Found\"}")).Error()
	if !strings.Contains(got, "Not Found") {
		t.Errorf("health line %q drops the response body", got)
	}
	if sessionUnavailableError(1, "p", nil) == nil {
		t.Error("a nil cause produced a nil error, which the health report would drop silently")
	}
}
