package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testAtlasCredential  = "atlas-credential-sentinel"
	testRunwayCredential = "site-key:site-secret-sentinel"
	testCacheToken       = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

type cacheTestTransport func(*http.Request) (*http.Response, error)

func (f cacheTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type cacheProvisionFixture struct {
	t             *testing.T
	reconciler    *cacheReconciler
	now           time.Time
	vm            *cacheAtlasVM
	extra         *cacheAtlasVM
	enrolled      bool
	pending       bool
	terminating   bool
	loseCreate    bool
	rejectList    bool
	deleteRead404 string
	calls         []string
}

func newCacheProvisionFixture(t *testing.T) *cacheProvisionFixture {
	t.Helper()
	f := &cacheProvisionFixture{t: t, now: time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)}
	client := &http.Client{Transport: cacheTestTransport(f.roundTrip)}
	credential := func(token string) func() ([]byte, error) {
		return func() ([]byte, error) { return []byte(token), nil }
	}
	config := cacheReconcileRegion{
		Region: "blr-test", AtlasURL: "https://atlas.example.test", RunwayURL: "https://runway.example.test",
		EnrolmentSecond: 600, Firewall: cacheRunbookFirewall,
		cacheVMShape: cacheVMShape{ImageID: "image-1", CPU: 2000, Memory: 2048, Disk: 8192},
	}
	f.reconciler = &cacheReconciler{
		config: config, path: filepath.Join(t.TempDir(), "region.json"),
		atlas:  cacheProvisionAPI{origin: config.AtlasURL, atlas: true, client: client, credential: credential(testAtlasCredential)},
		runway: cacheProvisionAPI{origin: config.RunwayURL, client: client, credential: credential(testRunwayCredential)},
		now:    func() time.Time { return f.now },
	}
	if err := f.reconciler.load(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *cacheProvisionFixture) roundTrip(req *http.Request) (*http.Response, error) {
	t := f.t
	w := httptest.NewRecorder()
	path, method := req.URL.Path, req.Method
	key := method + " " + path
	if req.URL.Host == "atlas.example.test" {
		if req.Header.Get("Authorization") != "Bearer "+testAtlasCredential || req.Header.Get("X-Tenant-ID") != "0" {
			t.Error("Atlas request lacks tenant-0 operator authorization")
		}
		switch key {
		case "GET " + cacheAtlasVMs:
			tag := req.URL.Query().Get("tag")
			expected := "role:runway-cache,region:" + f.reconciler.config.Region
			// An uncertain create must be searched by its exact attempt tag.
			attempt := ""
			if tag != expected {
				a := f.reconciler.journal.Attempt
				if a == nil || tag != expected+",attempt:"+a.Intent.Tags["attempt"] {
					t.Errorf("unexpected tag filter %q", tag)
				} else {
					attempt = a.Intent.Tags["attempt"]
					key += " attempt"
				}
			}
			if f.rejectList {
				w.WriteHeader(503)
				fmt.Fprint(w, testAtlasCredential, testCacheToken)
				break
			}
			items := []cacheAtlasVM{}
			for _, vm := range []*cacheAtlasVM{f.vm, f.extra} {
				if vm != nil && (attempt == "" || vm.Tags["attempt"] == attempt) {
					items = append(items, *vm)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items, "has_more": false})
		case "POST " + cacheAtlasVMs:
			var payload struct {
				cacheVMIntent
				UserData string `json:"user_data"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(f.reconciler.path)
			if err != nil {
				t.Fatal(err)
			}
			var persisted cacheReconcileJournal
			if err := json.Unmarshal(raw, &persisted); err != nil || persisted.Attempt == nil ||
				persisted.Attempt.Phase != "submitted" || !reflect.DeepEqual(persisted.Attempt.Intent, payload.cacheVMIntent) {
				t.Fatal("create reached Atlas without the same durable intent")
			}
			info, err := os.Stat(f.reconciler.path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("journal lacks mode 0600")
			}
			if !payload.IsPrivileged || !reflect.DeepEqual(payload.Firewall, cacheRunbookFirewall) {
				t.Error("incorrect cache VM privilege or firewall")
			}
			_, encoded, _ := strings.Cut(payload.UserData, "    content: ")
			env, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
			if err != nil || !strings.Contains(payload.UserData, "permissions: '0600'") ||
				!strings.Contains(string(env), "RUNWAY_ENROLMENT_TOKEN='"+testCacheToken+"'") ||
				!strings.Contains(string(env), "RUNWAY_REGION='blr-test'") {
				t.Error("incorrect cache bootstrap user data")
			}
			if strings.Contains(string(raw), testCacheToken) || (encoded != "" && strings.Contains(string(raw), strings.TrimSpace(encoded))) {
				t.Error("journal contains bootstrap secrets")
			}
			tenant := 0
			f.vm = &cacheAtlasVM{
				ID: fmt.Sprintf("vm-%d", persisted.Attempts), Tenant: &tenant,
				Tags: payload.Tags, cacheVMShape: payload.cacheVMShape, CreatedAt: f.now.Unix(),
			}
			if f.loseCreate {
				f.loseCreate = false
				f.calls = append(f.calls, key)
				return nil, errors.New(testAtlasCredential + testCacheToken)
			}
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(f.vm)
		default:
			if !strings.HasPrefix(path, cacheAtlasVMs+"/") {
				t.Fatalf("unexpected Atlas call %s", key)
			}
			if f.vm == nil {
				w.WriteHeader(http.StatusNotFound)
				if f.deleteRead404 != "" {
					fmt.Fprint(w, f.deleteRead404)
				} else {
					fmt.Fprint(w, `{"error":{"code":"not_found"}}`)
				}
				break
			}
			if path != cacheAtlasVMs+"/"+f.vm.ID {
				t.Fatalf("unexpected Atlas VM request %s", key)
			}
			switch method {
			case "GET":
				detail := cacheAtlasVMDetail{cacheAtlasVM: *f.vm, State: "running"}
				if f.terminating {
					detail.State = "terminating"
				}
				detail.Network.Mesh = "fdaa:1::7"
				json.NewEncoder(w).Encode(detail)
			case "DELETE":
				if f.enrolled || f.pending || f.reconciler.journal.Attempt.Phase != "deleting" {
					t.Error("delete must follow revocation and journal write")
				}
				w.WriteHeader(http.StatusAccepted)
				f.terminating = true
			default:
				t.Fatalf("unexpected Atlas request %s", key)
			}
		}
	} else {
		if req.Header.Get("Authorization") != "token "+testRunwayCredential || req.Header.Get("X-Tenant-ID") != "" {
			t.Error("Runway request lacks provisioner authorization or carries the Atlas tenant")
		}
		switch key {
		case "GET /api/method/runway.api.cache_status":
			status := map[string]any{"enrolled": f.enrolled, "token_pending": f.pending}
			if f.enrolled {
				status["mesh_address"], status["ca_fingerprint"] = "fdaa:1::7", strings.Repeat("a", 64)
			}
			json.NewEncoder(w).Encode(map[string]any{"message": status})
		case "POST /api/method/runway.api.cache_issue_enrolment":
			if f.reconciler.journal.Attempt.Phase != "preparing" {
				t.Error("token issued before intent")
			}
			f.pending = true
			json.NewEncoder(w).Encode(map[string]string{"message": testCacheToken})
		case "POST /api/method/runway.api.cache_revoke":
			if f.reconciler.journal.Attempt.Phase != "revoking" {
				t.Error("revoke started before replacement was journalled")
			}
			f.enrolled, f.pending = false, false
		default:
			t.Fatalf("unexpected Runway call %s", key)
		}
	}
	f.calls = append(f.calls, key)
	return w.Result(), nil
}

func (f *cacheProvisionFixture) step(expected string) {
	f.t.Helper()
	err := f.reconciler.step(context.Background())
	if (expected == "" && err != nil) || (expected != "" && (err == nil || err.Error() != expected)) {
		f.t.Fatalf("reconcile error %v, want %q", err, expected)
	}
}

func TestCacheReconcileConfigExample(t *testing.T) {
	raw, err := os.ReadFile("deploy/regional-cache/cache-reconcile.example.json")
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeCacheReconcileConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if config.StateDir != "/var/lib/runway/cache-reconcile" || config.Regions[0].EnrolmentSecond != 600 {
		t.Fatalf("unexpected example: %+v", config)
	}
	for _, unsafe := range []string{
		strings.Replace(string(raw), `"enabled": true`, `"enabled": false`, 1),
		strings.Replace(string(raw), "https://atlas-blr-1", "http://atlas-blr-1", 1),
		strings.Replace(string(raw), `"enrolment_timeout_seconds"`, `"enrollment_timeout_seconds"`, 1),
	} {
		if _, err := decodeCacheReconcileConfig([]byte(unsafe)); err == nil {
			t.Fatal("accepted unsafe or misspelled configuration")
		}
	}
}

func TestCacheReconcileRecoversCreateByAttemptTag(t *testing.T) {
	f := newCacheProvisionFixture(t)
	f.loseCreate = true
	f.step("atlas_transport")
	raw, err := os.ReadFile(f.reconciler.path)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.journal = cacheReconcileJournal{}
	if err := f.reconciler.decodeJournal(raw); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	f.step("")
	if !reflect.DeepEqual(f.calls, []string{"GET " + cacheAtlasVMs, "GET " + cacheAtlasVMs + " attempt"}) {
		t.Fatalf("restart calls %v", f.calls)
	}
	a := f.reconciler.journal.Attempt
	if a.Phase != "current" || a.VM != f.vm.ID || f.reconciler.journal.Attempts != 1 {
		t.Fatal("lost create was not adopted")
	}
}

func TestCacheReconcileWaitsBeforeRetryingMissingAttempt(t *testing.T) {
	f := newCacheProvisionFixture(t)
	f.loseCreate = true
	f.step("atlas_transport")
	first := f.reconciler.journal.Attempt.Intent.Tags["attempt"]
	f.vm = nil
	f.now = f.now.Add(cacheCreateWindow - time.Second)
	f.step("")
	if f.reconciler.journal.Attempt.Phase != "submitted" {
		t.Fatal("attempt failed before the discovery window ended")
	}
	f.now = f.now.Add(time.Second)
	f.step("")
	if f.reconciler.journal.Attempt.Phase != "failed" {
		t.Fatal("missing attempt was not marked failed")
	}
	f.step("")
	if a := f.reconciler.journal.Attempt; a.Phase != "current" || a.Intent.Tags["attempt"] == first ||
		f.reconciler.journal.Attempts != 2 {
		t.Fatal("missing attempt was not replaced with a fresh attempt")
	}
}

func TestCacheReconcileReplacementWaitsForScopedAbsence(t *testing.T) {
	for _, cause := range []string{"enrolment timeout", "config drift"} {
		t.Run(cause, func(t *testing.T) {
			f := newCacheProvisionFixture(t)
			f.step("")
			original := f.vm.ID
			if cause == "config drift" {
				f.enrolled, f.pending = true, false
				f.step("")
				f.reconciler.config.Memory *= 2
			} else {
				f.step("")
				f.now = f.now.Add(10 * time.Minute)
			}
			f.step("")
			if f.reconciler.journal.Attempt.Phase != "confirming" || !f.terminating {
				t.Fatal("replacement did not start")
			}
			f.step("")
			if f.reconciler.journal.Attempt.VM != original {
				t.Fatal("accepted DELETE was treated as confirmed absence")
			}
			f.vm, f.terminating = nil, false
			f.deleteRead404 = "proxy 404"
			f.step("atlas_http")
			if f.reconciler.journal.Attempt.Phase != "confirming" {
				t.Fatal("unscoped 404 released replacement")
			}
			f.deleteRead404 = ""
			f.step("")
			if f.reconciler.journal.Attempt != nil || f.vm != nil {
				t.Fatal("scoped 404 did not clear the old VM")
			}
			f.step("")
			if f.vm.ID == original || f.vm.cacheVMShape != f.reconciler.config.cacheVMShape ||
				f.reconciler.journal.Attempts != 2 || f.reconciler.journal.Replacements != 1 {
				t.Fatal("replacement was not created with the configured shape")
			}
		})
	}
}

func TestCacheReconcileLeavesExtraOrRetaggedVMsAlone(t *testing.T) {
	f := newCacheProvisionFixture(t)
	f.step("")
	duplicate := *f.vm
	duplicate.ID = "extra"
	f.extra = &duplicate
	f.reconciler.config.CPU *= 2
	f.calls = nil
	f.step("duplicate_vms")
	if len(f.calls) != 1 || f.reconciler.journal.Attempt.VM != f.vm.ID {
		t.Fatal("duplicate VM caused a write")
	}
	f.extra = nil
	delete(f.vm.Tags, "role")
	f.step("atlas_scope")
	if f.terminating || !f.pending {
		t.Fatal("retagged VM caused revocation or deletion")
	}
}

func TestCacheReconcileBackoffAndSecretSafeMetrics(t *testing.T) {
	f := newCacheProvisionFixture(t)
	f.loseCreate = true
	var messages bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&messages)
	t.Cleanup(func() { log.SetOutput(previous) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var observation cacheReconcileObservation
	f.reconciler.run(ctx, func(row cacheReconcileObservation) {
		observation = row
		cancel()
	})
	if observation.lastError != "atlas_transport" || observation.attempts != 1 {
		t.Fatalf("unexpected observation: %+v", observation)
	}
	metrics := cacheReconcileMetrics{rows: []cacheReconcileObservation{observation}}
	metrics.write(&messages)
	if !strings.Contains(messages.String(), `runway_cache_reconcile_last_error{region="blr-test",class="atlas_transport"} 1`) {
		t.Fatal("missing error class metric")
	}
	for _, secret := range []string{testAtlasCredential, testRunwayCredential, testCacheToken} {
		if strings.Contains(messages.String(), secret) {
			t.Fatal("credential leaked to logs or metrics")
		}
	}
	f.rejectList = true
	f.step("atlas_http")
	delay := cacheReconcilePoll
	for _, expected := range []time.Duration{30, 60, 120, 120} {
		delay = cacheReconcileDelay(delay, errors.New("atlas_http"))
		if delay != expected*time.Second {
			t.Fatalf("retry delay %s, want %ds", delay, expected)
		}
	}
	if cacheReconcileDelay(delay, nil) != cacheReconcilePoll {
		t.Fatal("successful reconciliation did not reset the delay")
	}
}

func TestCacheProvisionStatusAgainstRunwayAppContract(t *testing.T) {
	python := centralContractPython(t)
	f := newCacheProvisionFixture(t)
	api := f.reconciler.runway
	api.client = &http.Client{Transport: cacheTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/api/method/runway.api.cache_status" || r.URL.Query().Get("region") != "blr-test" {
			t.Fatal("unexpected status request")
		}
		cmd := exec.Command(python, "integrations/central/contract_harness.py")
		cmd.Stdin = strings.NewReader(`{"cache_status":{"region":"blr-test","cache_enrolment_hash":"test-hash","cache_enrolment_expires":"2026-11-20T15:15:00+00:00"}}`)
		answer, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		w.Write(answer)
		return w.Result(), nil
	})}
	status, err := api.cacheStatus(context.Background(), f.reconciler.config.Region)
	if err != nil || *status.Enrolled || !*status.TokenPending || status.TokenExpires == nil ||
		status.Mesh != "" || status.CA != "" {
		t.Fatalf("unexpected status response %+v: %v", status, err)
	}
}
