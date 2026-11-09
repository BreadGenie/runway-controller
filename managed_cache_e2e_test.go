package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/store"
	"github.com/BreadGenie/runway/cache/store/storetest"
	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// Peer injection is deliberately outside the product handler. Each TLS listener
// represents one mesh connection identity; no guest header can select its peer.
func managedPeer(handler http.Handler, peer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.RemoteAddr = peer; handler.ServeHTTP(w, r) })
}

type managedE2E struct {
	t                        *testing.T
	service                  *cacheService
	data                     *storetest.Service
	central, operator, guest *httptest.Server
	configure                *cacheConfigure
	key                      ed25519.PrivateKey
	setup                    grant.SetupRequest
	mu                       sync.Mutex
	credential               string
	cachePool                pilotPool
	lease                    cacheServiceLease
	buckets                  map[int64]store.GarageConfig
	provider                 *cacheBucketProvider
	storage                  map[int64]*managedS3
}

func newManagedE2E(t *testing.T, garageBuckets map[int64]store.GarageConfig) *managedE2E {
	t.Helper()
	f := &managedE2E{t: t, credential: "central-e2e-first", buckets: map[int64]store.GarageConfig{}}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	f.key = key
	authority := newAuthorityCase(t)
	gh, github := authority.authority(t)
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Scheme = "https"
		r.URL.Host = "api.github.com"
		response, err := github.RoundTrip(r)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
	t.Cleanup(rest.Close)
	gh.client = &http.Client{Transport: controllerRESTTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		u := *r.URL
		base, _ := url.Parse(rest.URL)
		u.Scheme = base.Scheme
		u.Host = base.Host
		clone.URL = &u
		return rest.Client().Do(clone)
	})}
	f.setup = authority.setup
	f.lease = cacheServiceLease{Lease: "e2e-lease", Team: "team-a", TenantID: 2, Region: "blr-1", Repository: fixtureRepository, RepositoryID: 580439734, DefaultBranch: "develop", VMID: "vm-7", MeshAddress: "fdaa:1:0:2::7", RunnerID: 219, RunnerName: authority.binding.RunnerName, Deadline: time.Now().Add(time.Hour), Job: &cacheServiceJob{Verified: true, Ref: fixtureRef, SHA: fixtureSHA, JobID: authority.release.JobID, RunID: 41867135947, RunAttempt: 1, DisplayName: "Integration (mariadb, 1)", WorkflowRef: fixtureWorkflow, EventName: "workflow_dispatch"}}
	// Offline S3 fake, explicitly not a Garage qualification. Separate listeners
	// and buckets make accidental cross-team routing observable.
	f.storage = map[int64]*managedS3{}
	for id := int64(2); id <= 3; id++ {
		f.storage[id], f.buckets[id] = newManagedS3(t, fmt.Sprintf("team-%d", id))
	}
	if garageBuckets != nil {
		f.buckets = garageBuckets
	}
	f.central = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		if r.URL.Path == "/api/method/central.api.jwks.get_jwks" {
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "key", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
			return
		}
		if r.Header.Get("Authorization") != "token "+f.credential {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/method/central.runway.cache_lease":
			address := r.URL.Query().Get("address")
			l := f.lease
			switch address {
			case "fdaa:1:0:2::7":
			case "fdaa:1:0:2::8":
				l.Lease = "e2e-rotated"
				l.MeshAddress = address
			case "fdaa:1:0:3::7":
				l.Lease = "e2e-lease-b"
				l.Team = "team-b"
				l.TenantID = 3
				l.MeshAddress = address
			default:
				w.WriteHeader(404)
				io.WriteString(w, `{"error":"not_found"}`)
				return
			}
			json.NewEncoder(w).Encode(l)
		case "/api/method/central.runway.cache_bucket":
			query := r.URL.Query()
			if len(query) != 1 || (query.Get("team") != "team-a" && query.Get("team") != "team-b" && query.Get("tenant_id") != "2" && query.Get("tenant_id") != "3") {
				w.WriteHeader(404)
				io.WriteString(w, `{"error":"not_found"}`)
				return
			}
			id := int64(2)
			// cache-data currently queries tenant_id, whereas setup queries team.
			if r.URL.Query().Get("team") == "team-b" || r.URL.Query().Get("tenant_id") == "3" {
				id = 3
			}
			c := f.buckets[id]
			team := "team-a"
			if id == 3 {
				team = "team-b"
			}
			json.NewEncoder(w).Encode(map[string]any{"team": team, "tenant_id": id, "bucket": c.Bucket, "endpoint": c.Endpoint, "region": c.Region, "access_key": c.AccessKey, "secret_key": c.SecretKey})
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"not_found"}`)
		}
	}))
	t.Cleanup(f.central.Close)
	rotating := &cacheCentralRotating{}
	f.configure = &cacheConfigure{config: cacheServiceConfig{CentralURL: f.central.URL, CentralJWKSURL: f.central.URL + "/api/method/central.api.jwks.get_jwks", CentralIssuer: "central", CentralRegionID: 1, MeshRegionID: 1, RegionID: "blr-1", CentralTokenFile: filepath.Join(t.TempDir(), "central-token")}, central: rotating, client: f.central.Client(), store: writeCacheCredential}
	f.operator = httptest.NewTLSServer(managedPeer(f.configure, "[fdaa:1::7]:9000"))
	t.Cleanup(f.operator.Close)
	f.provider = &cacheBucketProvider{credential: func() (string, error) {
		raw, err := os.ReadFile(f.configure.config.CentralTokenFile)
		return string(raw), err
	}, central: cacheCentralHTTPS{origin: f.central.URL, token: f.credential, client: f.central.Client()}, ca: f.buckets[2].CAPEM}
	f.data = storetest.Start(t, func(c *store.Config) {
		c.Buckets = f.provider
		c.QuotaBytes = 32 << 20
		c.TotalBytes = 64 << 20
		c.MaxArchiveBytes = 4 << 20
		c.Disk.PhysicalBytes = 64 << 20
	})
	// Use the same signing key/verifier as the real data handler, in blr-1.
	verifier, err := grant.NewVerifier(map[string]ed25519.PublicKey{grant.KeyID(f.data.Signer.Key.Public().(ed25519.PublicKey)): f.data.Signer.Key.Public().(ed25519.PublicKey)}, f.data.Config.PublicOrigin, "blr-1")
	if err != nil {
		t.Fatal(err)
	}
	f.data.Config.Verifier = verifier
	f.data.Restart(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	f.service = &cacheService{ctx: ctx, config: cacheServiceConfig{MeshRegionID: 1, RegionID: "blr-1"}, central: rotating, sessions: map[string]*cacheHost{}, slots: make(chan struct{}, 64), template: cacheHost{key: f.data.Signer.Key, roots: f.data.Client.Transport.(*http.Transport).TLSClientConfig.RootCAs, audience: f.data.Config.PublicOrigin, archiveBytes: 4 << 20, log: func(map[string]any) {}, clock: f.data.Clock.Observe}}
	f.guest = httptest.NewUnstartedServer(managedPeer(f.service, servicePeer))
	f.guest.Config.IdleTimeout = 5 * time.Second
	identity := t.TempDir()
	c := cacheServiceConfig{CertificateFile: "/cert.pem", PrivateKeyFile: "/key.pem", CacheDataCAFile: "/ca.pem"}
	local := func(path string) string { return filepath.Join(identity, path) }
	if err := prepareCacheServiceTLS(c, local, "cache.example.test"); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(local(c.CertificateFile), local(c.PrivateKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	f.configure.config.CacheDataCAFile = local(c.CacheDataCAFile)
	f.guest.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	f.guest.StartTLS()
	t.Cleanup(f.guest.Close)
	t.Cleanup(func() {
		cancel()
		f.service.mu.Lock()
		defer f.service.mu.Unlock()
		for _, h := range f.service.sessions {
			h.mu.Lock()
			if h.current != nil {
				h.current.Close()
			}
			h.mu.Unlock()
		}
	})
	// Resolve JobAssigned through the controller's REST worker, before any
	// JobStarted event, then publish only the persisted evidence to Central.
	pilot := newPilotStore()
	pilot.snapshot.Pool.RepositoryURL = "https://github.com/" + fixtureRepository
	lease := prepareVM(t, pilot)
	lease.Phase = "running"
	lease.RunnerID = 219
	lease.RunnerName = authority.binding.RunnerName
	engine := newPilotEngine(pilot, &fakePilotGitHub{store: pilot}, &fakeGuestTransport{})
	engine.assignmentREST = gh.client
	job := scaleset.JobMessageBase{JobID: authority.release.JobID, WorkflowRunID: 41867135947, OwnerName: "BreadGenie", RepositoryName: "frappe", JobDisplayName: "Integration (mariadb, 1)", JobWorkflowRef: fixtureWorkflow, EventName: "workflow_dispatch"}
	if err := engine.Handle(t.Context(), &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: job}}}); err != nil {
		t.Fatal(err)
	}
	drainAssignment(t, engine)
	deadline := time.Now().Add(5 * time.Second)
	for !pilot.snapshot.Leases[0].Verified && time.Now().Before(deadline) {
		if err := engine.verifyJobs(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	persisted := pilot.snapshot.Leases[0]
	if !persisted.Verified {
		t.Fatal("controller verification failed", persisted.VerifyReason)
	}
	if persisted.JobID != job.JobID || persisted.Started {
		t.Fatal("controller failed to persist early assignment")
	}
	attempt, _ := strconv.ParseInt(persisted.RunAttempt, 10, 64)
	f.lease.Job = &cacheServiceJob{Verified: persisted.Verified, Ref: persisted.Ref, SHA: persisted.SHA, JobID: persisted.JobID, RunID: job.WorkflowRunID, RunAttempt: attempt, DisplayName: persisted.JobDisplayName, WorkflowRef: persisted.JobWorkflowRef, EventName: persisted.EventName, Started: persisted.Started}
	return f
}

// Share the two disposable team buckets across client cases. Each case gets
// a new journal-owned namespace, so initial misses are still meaningful.
func managedGarageBuckets(t *testing.T) map[int64]store.GarageConfig {
	t.Helper()
	command := exec.Command("python3", "integrations/actions-cache/managed_garage.py")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("Garage helper failed: %v", err)
		}
	})
	var configs map[int64]store.GarageConfig
	if err := json.NewDecoder(output).Decode(&configs); err != nil {
		t.Fatalf("Garage fixture startup: %v", err)
	}
	for id, c := range configs {
		upstream, _ := url.Parse(c.Endpoint)
		proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(upstream))
		t.Cleanup(proxy.Close)
		c.Endpoint = proxy.URL
		c.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}))
		configs[id] = c
	}
	return configs
}

func (f *managedE2E) enroll(credential string) {
	f.t.Helper()
	body := configureBody(f.configure, credential)
	r, _ := http.NewRequest("POST", f.operator.URL+"/configure", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+configureToken(f.t, f.key, "key", nil))
	response, err := f.operator.Client().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var configured struct {
		Configured bool   `json:"configured"`
		CA         string `json:"ca"`
	}
	if response.StatusCode != 200 || json.Unmarshal(raw, &configured) != nil || !configured.Configured || configured.CA == "" {
		f.t.Fatalf("enrolment: %d %s", response.StatusCode, raw)
	}
	request, _ := json.Marshal(map[string]any{"pool": map[string]any{"name": "pool", "regional_cache": true, "fence": 1, "region": "blr-1"}, "region": map[string]string{"cache_service_name": "internal-cache.example.test:8443", "cache_service_ca": configured.CA}})
	command := exec.Command("python3", "integrations/central/contract_harness.py")
	command.Stdin = bytes.NewReader(request)
	output, err := command.CombinedOutput()
	var snapshot struct {
		Message pilotSnapshot `json:"message"`
	}
	if err != nil || json.Unmarshal(output, &snapshot) != nil {
		f.t.Fatalf("Central pool snapshot: %v %s", err, output)
	}
	f.cachePool = snapshot.Message.Pool
	f.mu.Lock()
	f.credential = credential
	f.mu.Unlock()
}

func (f *managedE2E) request(method, path string, body []byte, headers map[string]string) (int, []byte) {
	f.t.Helper()
	r, _ := http.NewRequestWithContext(f.t.Context(), method, f.guest.URL+path, bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	response, err := f.guest.Client().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, secret := range []string{"test-access", "test-secret", "central-e2e-first", "central-e2e-second", base64.StdEncoding.EncodeToString(f.data.Signer.Key)} {
		if bytes.Contains(raw, []byte(secret)) {
			f.t.Fatal("secret in guest response")
		}
	}
	return response.StatusCode, raw
}
func (f *managedE2E) enable(want string) {
	f.t.Helper()
	f.setup.Nonce = uuid.NewString()
	raw, _ := json.Marshal(f.setup)
	status, raw := f.request("POST", "/setup", append(raw, '\n'), nil)
	var result struct{ Outcome string }
	json.Unmarshal(raw, &result)
	if status != 200 || result.Outcome != want {
		f.t.Fatalf("setup: %d %s; want %s", status, raw, want)
	}
}
func (f *managedE2E) lookup(key string) (int, string) {
	f.t.Helper()
	status, raw := f.request("GET", "/_apis/artifactcache/cache?keys="+key+"&version=v1", nil, nil)
	var result struct {
		Location string `json:"archiveLocation"`
	}
	json.Unmarshal(raw, &result)
	return status, result.Location
}
func (f *managedE2E) save() {
	f.t.Helper()
	status, raw := f.request("POST", "/_apis/artifactcache/caches", []byte(`{"key":"deps-1","version":"v1","cacheSize":7}`), nil)
	var reply struct {
		ID int64 `json:"cacheId"`
	}
	json.Unmarshal(raw, &reply)
	if status != 201 || reply.ID == 0 {
		f.t.Fatalf("reserve: %d %s", status, raw)
	}
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", reply.ID)
	if status, _ = f.request("PATCH", path, []byte("archive"), map[string]string{"Content-Range": "bytes 0-6/*"}); status != 204 {
		f.t.Fatal("upload", status)
	}
	if status, raw = f.request("POST", path, []byte(`{"size":7}`), nil); status != 204 {
		f.t.Fatalf("finalize %d %s", status, raw)
	}
}
func TestManagedCacheEndToEnd(t *testing.T) {
	if os.Getenv("RUNWAY_MANAGED_E2E") != "1" {
		t.Skip("set RUNWAY_MANAGED_E2E=1 (offline S3 fake)")
	}
	t.Run("managed-lifecycle", func(t *testing.T) {
		f := newManagedE2E(t, nil)
		f.enable("disabled")
		f.enroll("central-e2e-first")
		f.enable("enabled")
		if status, _ := f.lookup("deps-1"); status != 204 {
			t.Fatal("initial miss", status)
		}
		f.save()
		for _, key := range []string{"deps-1", "deps-"} {
			status, location := f.lookup(key)
			if status != 200 {
				t.Fatal("hit", key, status)
			}
			path := strings.TrimPrefix(location, "http://127.0.0.1:18080")
			status, raw := f.request("GET", path, nil, nil)
			if status != 200 || string(raw) != "archive" {
				t.Fatalf("byte equality: %d %q", status, raw)
			}
		}
		journalNamespace := func() string {
			raw, err := os.ReadFile(filepath.Join(f.data.Config.StateDir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var value struct{ Namespace string }
			if json.Unmarshal(raw, &value) != nil {
				t.Fatal("journal JSON")
			}
			return value.Namespace
		}
		oldNamespace := journalNamespace()
		f.data.Restart(t)
		if journalNamespace() != oldNamespace {
			t.Fatal("restart changed service id")
		}
		if status, _ := f.lookup("deps-1"); status != 200 {
			t.Fatal("restart lost hit", status)
		}
		other := httptest.NewTLSServer(managedPeer(f.service, "[fdaa:1:0:3::7]:1234"))
		defer other.Close()
		original := f.guest
		status, locationA := f.lookup("deps-1")
		if status != 200 {
			t.Fatal("team A archive missing")
		}
		f.guest = other
		f.enable("enabled")
		if status, _ := f.lookup("deps-1"); status != 204 {
			t.Fatal("cross-team read", status)
		}
		if status, _ := f.request("GET", strings.TrimPrefix(locationA, "http://127.0.0.1:18080"), nil, nil); status == 200 {
			t.Fatal("team B read team A ticket")
		}
		// Tenant binding also holds at cache-data without the translator's
		// session check: team B's own bearer and no bearer fail on A's URL.
		dataLocation := strings.Replace(locationA, "http://127.0.0.1:18080", f.data.Config.PublicOrigin, 1)
		now := time.Now()
		teamBBearer, err := grant.Issue(f.data.Signer.Key, f.data.Config.PublicOrigin, grant.Authorization{
			TenantID: 3, RegionID: "blr-1", JobID: uuid.NewString(), RepositoryID: 580439734,
			RunID: 41867135947, RunAttempt: 1, ReadRefs: []string{"refs/heads/develop"}, Deadline: now.Add(10 * time.Minute),
		}, grant.Clock{Earliest: now, Latest: now})
		if err != nil {
			t.Fatal("team B bearer", err)
		}
		for name, token := range map[string]string{"team-b session": teamBBearer, "no session": ""} {
			if status, _ := f.data.Do(t, "GET", dataLocation, token, "", nil); status != 404 {
				t.Fatalf("cache-data admitted %s on team A ticket: %d", name, status)
			}
		}
		f.guest = original
		f.mu.Lock()
		f.lease.TenantID = 3
		f.mu.Unlock()
		if got := serviceSetup(t, f.service, f.setup, servicePeer); got != "disabled" {
			t.Fatal("Central/source tenant mismatch enabled", got)
		}
		f.mu.Lock()
		f.lease.TenantID = 2
		f.mu.Unlock()
		for _, peer := range []string{"127.0.0.1:3", "[fdaa:2:0:2::7]:3", "[fdaa:1:0:2::99]:3"} {
			if got := serviceSetup(t, f.service, f.setup, peer); got != "Forbidden" && got != "disabled" {
				t.Fatal("unknown/spoofed peer", got)
			}
		}
		f.enroll("central-e2e-second")
		rotated := httptest.NewTLSServer(managedPeer(f.service, "[fdaa:1:0:2::8]:1234"))
		defer rotated.Close()
		f.guest = rotated
		f.enable("enabled")
		f.guest = original
		f.provider.Invalidate(2)
		if _, err := f.provider.Bucket(t.Context(), 2); err != nil {
			t.Fatal("managed data retained old Central credential after configure", err)
		}
		if err := f.data.Server.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.data.Server = nil
		if err := os.Remove(filepath.Join(f.data.Config.StateDir, "state.json")); err != nil {
			t.Fatal(err)
		}
		f.data.Restart(t)
		if journalNamespace() == oldNamespace {
			t.Fatal("recreate reused service id")
		}
		if status, _ := f.lookup("deps-1"); status != 204 {
			t.Fatal("recreate adopted old objects", status)
		}
		// Bucket use triggers maintenance. Join it via Close before inspecting
		// results. Production refuses thresholds <=2h, so age fake metadata
		// across a 3h threshold instead of weakening that safety bound.
		f.save()
		if err := f.data.Server.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.data.Server = nil
		if f.storage[2].count(oldNamespace) == 0 {
			t.Fatal("fresh stale-prefix objects reclaimed too early")
		}
		f.storage[2].age(oldNamespace, 4*time.Hour)
		f.data.Config.CleanupStaleAfter = 3 * time.Hour
		f.data.Restart(t)
		// A download touches storage, starting the next maintenance pass.
		_, location := f.lookup("deps-1")
		f.request("GET", strings.TrimPrefix(location, "http://127.0.0.1:18080"), nil, nil)
		if err := f.data.Server.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.data.Server = nil
		if f.storage[2].count(oldNamespace) != 0 {
			t.Fatal("aged stale prefix survived lazy cleanup")
		}
		t.Log("unenrolled fallback; early assigned setup; miss/save/exact/prefix bytes; team isolation; unknown/spoofed refusal; restart; configure rotation; recreate passed")
	})
	if os.Getenv("RUNWAY_MANAGED_E2E_OFFLINE") == "1" {
		return
	}
	var garageBuckets map[int64]store.GarageConfig
	if os.Getenv("RUNWAY_GARAGE_TEST") == "1" {
		garageBuckets = managedGarageBuckets(t)
	}
	nodeDir := t.TempDir()
	nodeTest := filepath.Join(nodeDir, "cache-node.test")
	build := exec.Command("go", "test", "-race", "-c", "-o", nodeTest, "./cmd/cache-node")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cache-node helper: %v %s", err, output)
	}
	shim := filepath.Join(nodeDir, "node")
	// Use Python exec, since a shell drops action input environment names
	// containing hyphens (notably INPUT_RESTORE-KEYS).
	shimSource := fmt.Sprintf("#!/usr/bin/python3\nimport os,sys\nos.execv(%q,[%q,'-test.run','^TestManagedNodeProcess$','--']+sys.argv[1:])\n", nodeTest, nodeTest)
	if err := os.WriteFile(shim, []byte(shimSource), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("python3", "integrations/actions-cache/test_relay_clients.py", "--list-clients").CombinedOutput()
	if err != nil {
		t.Fatalf("clients: %v %s", err, output)
	}
	for _, name := range strings.Fields(string(output)) {
		t.Run(name, func(t *testing.T) {
			f := newManagedE2E(t, garageBuckets)
			f.enroll("central-e2e-first")
			ca := filepath.Join(t.TempDir(), "ca.pem")
			os.WriteFile(ca, []byte(f.cachePool.CacheCA), 0600)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			contextPath := filepath.Join(t.TempDir(), "context.json")
			contextJSON, _ := json.Marshal(f.setup.Context)
			var fields map[string]string
			json.Unmarshal(contextJSON, &fields)
			environment := map[string]string{}
			for key, value := range fields {
				environment["GITHUB_"+strings.ToUpper(key)] = value
			}
			raw, _ := json.Marshal(environment)
			os.WriteFile(contextPath, raw, 0600)
			routes := t.TempDir()
			artifacts := t.TempDir()
			command := exec.CommandContext(ctx, "python3", "integrations/actions-cache/test_relay_clients.py", "--transport-port", strings.Split(strings.TrimPrefix(f.guest.URL, "https://"), ":")[1], "--tls-ca", ca, "--client", name, "--managed-context", contextPath, "--node-wrapper", shim, "--artifacts", artifacts)
			command.Env = append(os.Environ(), "RUNWAY_MANAGED_NODE_PROCESS=1", "RUNWAY_REAL_NODE=/home/t3code/.local/opt/node/bin/node", "RUNWAY_TEST_CA="+ca, "RUNWAY_TEST_SERVICE="+strings.TrimPrefix(f.guest.URL, "https://"), "RUNWAY_TEST_ROUTES="+routes, "RUNWAY_TEST_ENV_AUDIT="+filepath.Join(artifacts, "action-environment.json"))
			output, err := command.CombinedOutput()
			secrets := []string{f.credential, hex.EncodeToString(f.data.Signer.Key), base64.StdEncoding.EncodeToString(f.data.Signer.Key)}
			der, _ := x509.MarshalPKCS8PrivateKey(f.data.Signer.Key)
			for _, raw := range [][]byte{f.data.Signer.Key, f.data.Signer.Key.Seed(), der} {
				secrets = append(secrets, string(raw), hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw))
			}
			for _, bucket := range f.buckets {
				secrets = append(secrets, bucket.AccessKey, bucket.SecretKey)
			}
			audit := func(raw []byte) {
				for _, secret := range secrets {
					if bytes.Contains(raw, []byte(secret)) {
						t.Fatal("service secret reached guest artifact/environment/log")
					}
				}
			}
			audit(output)
			if err != nil {
				t.Fatalf("pinned client: %v\n%s", err, output)
			}
			t.Log(string(output))
			audit([]byte(strings.Join(command.Env, "\n")))
			for _, directory := range []string{artifacts, routes} {
				if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !entry.IsDir() {
						raw, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						audit(raw)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(name, "cache/v4.3.0/main-post") {
				var calls atomic.Int32
				fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
				defer fallback.Close()
				f.guest.Close()
				stopped := exec.CommandContext(ctx, command.Path, command.Args[1:]...)
				stopped.Args = append(stopped.Args, "--fallback-url", fallback.URL+"/")
				stopped.Env = append(command.Env, "RUNWAY_TEST_ROUTES="+t.TempDir())
				// Use a fresh guest workspace for the stopped-service job.
				for i, arg := range stopped.Args {
					if arg == "--artifacts" {
						stopped.Args[i+1] = t.TempDir()
					}
				}
				raw, err := stopped.CombinedOutput()
				audit(raw)
				if err != nil || calls.Load() == 0 {
					t.Fatalf("stopped-service fallback: %v %s", err, raw)
				}
				t.Log(string(raw))
			}
		})
	}
}
