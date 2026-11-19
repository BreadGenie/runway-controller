package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/BreadGenie/runway-controller/cache/grant"
	"github.com/BreadGenie/runway-controller/cache/store"
	"github.com/BreadGenie/runway-controller/cache/store/storetest"
)

func cacheDataConfigFixture(t *testing.T) (cacheDataServiceConfig, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	budget := store.DefaultStorageBudget
	return cacheDataServiceConfig{
		Version: 1, Listen: address, PublicOrigin: "https://" + address,
		CertificateFile: "/fixture/cert.pem", PrivateKeyFile: "/fixture/key.pem",
		PublicKeys: map[string]string{grant.KeyID(public): hex.EncodeToString(public)}, RegionID: "region",
		StateDir: state, QuotaBytes: 4096, TotalBytes: 4096, MaxArchiveBytes: 1024,
		Disk:          store.DiskConfig{Directory: filepath.Join(t.TempDir(), "spools"), PhysicalBytes: 1 << 20},
		Garage:        store.GarageConfig{Endpoint: "http://127.0.0.1:1", Region: "fixture", Bucket: "disposable", AccessKey: "fixture-access", SecretKey: "fixture-secret"},
		StorageBudget: &budget,
	}, private
}

// coreFixture decodes a fixture configuration the way the command does.
func coreFixture(t *testing.T, config cacheDataServiceConfig) store.Config {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	_, core, err := decodeCacheDataServiceConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return core
}

func TestCacheDataServiceConfigurationIsFiniteAndStrict(t *testing.T) {
	config, _ := cacheDataConfigFixture(t)
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, core, err := decodeCacheDataServiceConfig(encoded); err != nil || core.Disk.Directory != config.Disk.Directory || core.MaxArchiveBytes != 1024 {
		t.Fatal("valid explicit configuration refused", err)
	}
	for name, change := range map[string]func(*cacheDataServiceConfig){
		"wildcard":             func(c *cacheDataServiceConfig) { c.Listen = "0.0.0.0:4443" },
		"ipv6 wildcard":        func(c *cacheDataServiceConfig) { c.Listen = "[::]:4443" },
		"public":               func(c *cacheDataServiceConfig) { c.Listen = "8.8.8.8:4443" },
		"dns":                  func(c *cacheDataServiceConfig) { c.Listen = "localhost:4443" },
		"dynamic port":         func(c *cacheDataServiceConfig) { c.Listen = "127.0.0.1:0" },
		"large port":           func(c *cacheDataServiceConfig) { c.Listen = "127.0.0.1:65536" },
		"version":              func(c *cacheDataServiceConfig) { c.Version = 2 },
		"plaintext":            func(c *cacheDataServiceConfig) { c.PublicOrigin = "http://cache.example" },
		"origin path":          func(c *cacheDataServiceConfig) { c.PublicOrigin += "/" },
		"origin query":         func(c *cacheDataServiceConfig) { c.PublicOrigin += "?" },
		"origin fragment":      func(c *cacheDataServiceConfig) { c.PublicOrigin += "#" },
		"origin user":          func(c *cacheDataServiceConfig) { c.PublicOrigin = "https://user:secret@cache.example" },
		"missing region":       func(c *cacheDataServiceConfig) { c.RegionID = "" },
		"relative state":       func(c *cacheDataServiceConfig) { c.StateDir = "state" },
		"disk relative":        func(c *cacheDataServiceConfig) { c.Disk.Directory = "relative" },
		"no keys":              func(c *cacheDataServiceConfig) { c.PublicKeys = nil },
		"wrong key id":         func(c *cacheDataServiceConfig) { c.PublicKeys = map[string]string{"wrong": strings.Repeat("01", 32)} },
		"private key":          func(c *cacheDataServiceConfig) { c.PublicKeys = map[string]string{"wrong": strings.Repeat("01", 64)} },
		"uppercase key":        func(c *cacheDataServiceConfig) { c.PublicKeys = map[string]string{"wrong": strings.Repeat("AB", 32)} },
		"relative certificate": func(c *cacheDataServiceConfig) { c.CertificateFile = "cert.pem" },
		"relative key":         func(c *cacheDataServiceConfig) { c.PrivateKeyFile = "key.pem" },
		"no state":             func(c *cacheDataServiceConfig) { c.StateDir = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := config
			change(&bad)
			data, _ := json.Marshal(bad)
			if _, _, err := decodeCacheDataServiceConfig(data); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	for name, data := range map[string]string{
		"duplicate":         strings.Replace(string(encoded), `"version":1`, `"version":1,"version":1`, 1),
		"escaped duplicate": strings.Replace(string(encoded), `"version":1`, `"version":1,"\u0076ersion":1`, 1),
		"case alias":        strings.Replace(string(encoded), `"version":1`, `"Version":1`, 1),
		"missing":           strings.Replace(string(encoded), `"version":1,`, "", 1),
		"null":              strings.Replace(string(encoded), `"version":1`, `"version":null`, 1),
		"token mode":        strings.Replace(string(encoded), `"version":1`, `"version":1,"tokens":{"fixture":{}}`, 1),
		"skip verify":       strings.Replace(string(encoded), `"bucket":`, `"insecure_skip_verify":true,"bucket":`, 1),
		"unknown nested":    strings.Replace(string(encoded), `"bucket":`, `"admin_token":"secret","bucket":`, 1),
		"duplicate nested":  strings.Replace(string(encoded), `"bucket":`, `"bucket":"other","bucket":`, 1),
		"lifetime":          strings.Replace(string(encoded), `"version":1`, `"version":1,"lifetime_seconds":60`, 1),
		"clock source":      strings.Replace(string(encoded), `"version":1`, `"version":1,"clock_source":"host_ntp"`, 1),
		"unknown disk":      strings.Replace(string(encoded), `"directory":`, `"grace":1,"directory":`, 1),
		"duplicate disk":    strings.Replace(string(encoded), `"physical_bytes":`, `"physical_bytes":1,"physical_bytes":`, 1),
		"unknown budget":    strings.Replace(string(encoded), `"base_seconds":`, `"grace":1,"base_seconds":`, 1),
		"null budget":       strings.Replace(string(encoded), `"storage_budget":{`, `"storage_budget":null,"x":{`, 1),
		"old garage key":    strings.Replace(string(encoded), `"garage":`, `"garage_fixture":`, 1),
		"null disk":         strings.Replace(string(encoded), `"disk":{`, `"disk":null,"x":{`, 1),
		"trailing":          string(encoded) + `{}`,
		"oversized":         string(encoded) + strings.Repeat(" ", 4097),
		"array":             `[]`,
		"malformed":         `{`,
		"invalid utf8":      string(encoded) + "\xff",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeCacheDataServiceConfig([]byte(data)); err == nil {
				t.Fatal("ambiguous configuration accepted")
			}
		})
	}
	for _, listen := range []string{"10.1.2.3:443", "[fd00::1]:443", "[::1]:443"} {
		config.Listen = listen
		data, _ := json.Marshal(config)
		if _, _, err := decodeCacheDataServiceConfig(data); err != nil {
			t.Fatal("explicit private/loopback bounds refused", err)
		}
	}
	// Storage routes get the storage ceiling; chunk uploads keep 30 s.
	for path, want := range map[string]bool{"/_apis/artifactcache/caches/12": true, "/_apis/artifactcache/caches": false, "/_apis/artifactcache/caches/x": false} {
		if got := cacheDataStorageRoute(httptest.NewRequest("POST", path, nil)); got != want {
			t.Fatalf("%s storage route %v", path, got)
		}
	}
	if !cacheDataStorageRoute(httptest.NewRequest("GET", "/download/3", nil)) || cacheDataStorageRoute(httptest.NewRequest("PATCH", "/_apis/artifactcache/caches/12", nil)) {
		t.Fatal("storage deadline route selection changed")
	}
}

func TestCacheDataServiceRefusesUnprotectedConfigurationWithoutLeaking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"secret":"must-never-appear"}`), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, link, "relative.json", path + ".absent"} {
		if err := runCacheDataService(candidate); err == nil || strings.Contains(err.Error(), "must-never-appear") || strings.Contains(err.Error(), candidate) {
			t.Fatal("unsafe file accepted or error disclosed configuration", err)
		}
	}
}

func cacheDataTLSFixture(t *testing.T) (tls.Certificate, *http.Client) {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	pair := server.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	server.Close()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	t.Cleanup(transport.CloseIdleConnections)
	return pair, &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

type runningCacheDataService struct {
	cancel context.CancelFunc
	done   chan error
	joined bool
	result error
}

func (running *runningCacheDataService) join(t *testing.T) error {
	t.Helper()
	if !running.joined {
		select {
		case running.result = <-running.done:
			running.joined = true
		case <-time.After(8 * time.Second):
			t.Fatal("data service did not join")
		}
	}
	return running.result
}

func startCacheDataFixture(t *testing.T, config cacheDataServiceConfig, pair tls.Certificate, client *http.Client, observe func(context.Context) (grant.Clock, error)) *runningCacheDataService {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	running := &runningCacheDataService{cancel: cancel, done: make(chan error, 1)}
	core := coreFixture(t, config)
	go func() {
		running.done <- startCacheDataService(ctx, config.Listen, core, pair, observe, config.MetricsListen)
	}()
	t.Cleanup(func() { cancel(); running.join(t) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 50 * time.Millisecond}, "tcp", config.Listen, client.Transport.(*http.Transport).TLSClientConfig)
		if err == nil {
			connection.Close()
			return running
		}
		select {
		case result := <-running.done:
			running.joined, running.result = true, result
			t.Fatal("data service failed to start", result)
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("data service never accepted verified TLS")
	return nil
}

func cacheDataGrant(t *testing.T, private ed25519.PrivateKey, origin, region string, clock grant.Clock) string {
	t.Helper()
	token, err := grant.Issue(private, origin, grant.Authorization{
		RegionID: region, JobID: "afc2e663-0d50-409b-bf27-2254fcf2fe97", RunID: 12, RunAttempt: 1, RepositoryID: 1,
		ReadRefs: []string{"refs/heads/main"}, WriteRef: "refs/heads/main", Deadline: clock.Latest.Add(time.Minute),
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func callCacheData(t *testing.T, client *http.Client, method, endpoint, token, body, contentRange, host string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if contentRange != "" {
		request.Header.Set("Content-Range", contentRange)
	}
	request.Host = host
	request.Header.Set("X-Forwarded-Host", "untrusted.example")
	request.Header.Set("X-Forwarded-Proto", "http")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		t.Fatal("data request did not negotiate TLS 1.3")
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func assertCacheDataListenerClosed(t *testing.T, address string) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("data listener survived exit")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("data listener not reusable", err)
	}
	listener.Close()
}

func TestCacheDataServiceTLSAndClock(t *testing.T) {
	config, private := cacheDataConfigFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config.MetricsListen = listener.Addr().String()
	listener.Close()
	garage := storetest.NewGarage(t)
	config.Garage.Endpoint = garage.URL
	pair, client := cacheDataTLSFixture(t)
	now := time.Now()
	clock := grant.Clock{Earliest: now.Add(-time.Second), Latest: now}
	var clockRefused atomic.Bool
	observe := func(context.Context) (grant.Clock, error) {
		if clockRefused.Load() {
			return grant.Clock{}, errors.New("private clock diagnostic")
		}
		return clock, nil
	}
	running := startCacheDataFixture(t, config, pair, client, observe)
	token := cacheDataGrant(t, private, config.PublicOrigin, config.RegionID, clock)
	lookup := config.PublicOrigin + "/_apis/artifactcache/cache?keys=deps&version=v1"
	if status, _ := callCacheData(t, client, "GET", lookup, "", "", "", ""); status != 401 {
		t.Fatal("unsigned request accepted", status)
	}
	if status, _ := callCacheData(t, client, "GET", lookup, token, "", "", "other.example"); status != 400 {
		t.Fatal("foreign Host accepted", status)
	}
	legacy := client.Transport.(*http.Transport).TLSClientConfig.Clone()
	legacy.MinVersion, legacy.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", config.Listen, legacy); err == nil {
		connection.Close()
		t.Fatal("TLS 1.2 accepted")
	}
	plainRequest, _ := http.NewRequestWithContext(t.Context(), "GET", "http://"+config.Listen+"/", nil)
	plain := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: time.Second}
	response, err := plain.Do(plainRequest)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("plaintext was not refused", response.StatusCode)
	}
	if status, _ := callCacheData(t, client, "GET", lookup, token, "", "", ""); status != 204 {
		t.Fatal("signed lookup failed", status)
	}
	clockRefused.Store(true)
	if status, data := callCacheData(t, client, "GET", lookup, token, "", "", ""); status != 503 || strings.Contains(string(data), "private clock") {
		t.Fatal("clock refusal failed or disclosed diagnostic", status)
	}
	response, err = plain.Get("http://" + config.MetricsListen + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil ||
		!strings.Contains(string(body), "runway_cache_misses_total 1\n") ||
		!strings.Contains(string(body), "runway_cache_hits_total 0\n") {
		t.Fatalf("invalid metrics scrape: %s (%v)", body, readErr)
	}
	for _, area := range []string{"journal", "spool"} {
		for _, name := range []string{"available", "size"} {
			var bytes uint64
			pattern := fmt.Sprintf("runway_cache_filesystem_%s_bytes{area=%q} ", name, area)
			_, value, found := strings.Cut(string(body), pattern)
			if _, err := fmt.Sscan(value, &bytes); !found || err != nil || bytes == 0 {
				t.Errorf("missing positive filesystem sample %s in %s", pattern, body)
			}
		}
	}
	running.cancel()
	if err := running.join(t); err != nil {
		t.Fatal(err)
	}
}

func TestCacheDataServiceParentCancellationCancelsActiveRequestsBeforeUnlock(t *testing.T) {
	config, private := cacheDataConfigFixture(t)
	pair, client := cacheDataTLSFixture(t)
	now := time.Now()
	clock := grant.Clock{Earliest: now.Add(-time.Second), Latest: now}
	entered, canceled := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	observe := func(ctx context.Context) (grant.Clock, error) {
		if calls.Add(1) == 1 {
			return clock, nil
		}
		close(entered)
		<-ctx.Done()
		close(canceled)
		return grant.Clock{}, ctx.Err()
	}
	running := startCacheDataFixture(t, config, pair, client, observe)
	token := cacheDataGrant(t, private, config.PublicOrigin, config.RegionID, clock)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, _ := http.NewRequestWithContext(t.Context(), "GET", config.PublicOrigin+"/_apis/artifactcache/cache?keys=deps&version=v1", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if response, err := client.Do(r); err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached clock")
	}
	// A second process cannot own this journal while the active request runs.
	assertCacheDataJournalLock(t, config.StateDir, false)
	running.cancel() // systemd stop: SIGTERM cancels the parent context
	if err := running.join(t); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("active request was not canceled before service exit")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP client did not finish")
	}
	assertCacheDataListenerClosed(t, config.Listen)
	assertCacheDataJournalLock(t, config.StateDir, true)
}

func assertCacheDataJournalLock(t *testing.T, state string, available bool) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(state, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if (err == nil) != available {
		t.Fatal("unexpected journal ownership", err)
	}
}

func TestCacheDataServiceStartupFailuresReleaseListenerAndJournal(t *testing.T) {
	for _, failure := range []string{"clock", "clock cancellation", "TLS", "journal already owned", "listener already owned", "origin rebind"} {
		t.Run(failure, func(t *testing.T) {
			config, _ := cacheDataConfigFixture(t)
			pair, _ := cacheDataTLSFixture(t)
			observe := func(context.Context) (grant.Clock, error) {
				now := time.Now()
				return grant.Clock{Earliest: now, Latest: now}, nil
			}
			if failure == "clock" {
				observe = func(context.Context) (grant.Clock, error) {
					return grant.Clock{}, errors.New("fixture-secret")
				}
			}
			if failure == "clock cancellation" {
				observe = func(ctx context.Context) (grant.Clock, error) {
					<-ctx.Done()
					return grant.Clock{}, ctx.Err()
				}
			}
			if failure == "TLS" {
				pair = tls.Certificate{}
			}
			if failure == "journal already owned" || failure == "origin rebind" {
				core := coreFixture(t, config)
				core.Clock = observe
				owner, err := store.New(core)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "origin rebind" {
					if err := owner.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					config.PublicOrigin = "https://other.example"
				} else {
					t.Cleanup(func() {
						if err := owner.Close(context.Background()); err != nil {
							t.Error(err)
						}
					})
				}
			}
			if failure == "listener already owned" {
				listener, err := net.Listen("tcp", config.Listen)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if err := startCacheDataService(ctx, config.Listen, coreFixture(t, config), pair, observe, config.MetricsListen); err == nil || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), config.Garage.Endpoint) {
				t.Fatal("startup failure was not sanitized", err)
			}
			if failure != "listener already owned" {
				assertCacheDataListenerClosed(t, config.Listen)
			}
			if failure == "origin rebind" {
				assertCacheDataJournalLock(t, config.StateDir, true)
			}
		})
	}
}

func TestCacheDataServiceForcedShutdownJoinsBlockedUpload(t *testing.T) {
	config, private := cacheDataConfigFixture(t)
	pair, client := cacheDataTLSFixture(t)
	client.Timeout = 8 * time.Second
	now := time.Now()
	clock := grant.Clock{Earliest: now.Add(-time.Second), Latest: now}
	entered := make(chan struct{})
	var observations atomic.Int32
	observe := func(context.Context) (grant.Clock, error) {
		if observations.Add(1) == 3 {
			close(entered)
		}
		return clock, nil
	}
	running := startCacheDataFixture(t, config, pair, client, observe)
	token := cacheDataGrant(t, private, config.PublicOrigin, config.RegionID, clock)
	status, data := callCacheData(t, client, "POST", config.PublicOrigin+"/_apis/artifactcache/caches", token, `{"key":"blocked","version":"v1","cacheSize":3}`, "", "")
	var reserved struct {
		CacheID int64 `json:"cacheId"`
	}
	if status != 201 || json.Unmarshal(data, &reserved) != nil || reserved.CacheID <= 0 {
		t.Fatal("reservation failed", status)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	request, err := http.NewRequestWithContext(t.Context(), "PATCH", fmt.Sprintf("%s/_apis/artifactcache/caches/%d", config.PublicOrigin, reserved.CacheID), reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 3
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Range", "bytes 0-2/*")
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	// Send only part of the declared body, leaving a real core upload blocked in
	// ReadAll. Request context cancellation alone cannot unblock this read.
	if _, err := writer.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upload never reached core")
	}
	running.cancel()
	err = running.join(t)
	if err == nil || err.Error() != "cache data shutdown incomplete" {
		t.Fatal("forced shutdown was not reported", err)
	}
	writer.Close()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("blocked upload client survived shutdown")
	}
	assertCacheDataListenerClosed(t, config.Listen)
	assertCacheDataJournalLock(t, config.StateDir, true)
}

func TestCacheDataServicePropagatesDurableCloseFailure(t *testing.T) {
	config, _ := cacheDataConfigFixture(t)
	pair, client := cacheDataTLSFixture(t)
	observe := func(context.Context) (grant.Clock, error) {
		now := time.Now()
		return grant.Clock{Earliest: now, Latest: now}, nil
	}
	running := startCacheDataFixture(t, config, pair, client, observe)
	state := filepath.Join(config.StateDir, "state.json")
	// A directory cannot be replaced by the journal's atomic file rename, even
	// when these local tests run as root. Close must report that durable failure.
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	running.cancel()
	err := running.join(t)
	if err == nil || !strings.Contains(err.Error(), "cache data state close failed") || strings.Contains(err.Error(), config.StateDir) {
		t.Fatal("close failure lost or disclosed state path", err)
	}
	assertCacheDataListenerClosed(t, config.Listen)
	assertCacheDataJournalLock(t, config.StateDir, true)
}

func TestCacheDataServiceAdmissionRefusesExcessAndStoppedRequests(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	requests := &cacheDataServiceRequests{slots: make(chan struct{}, 4), handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release; w.WriteHeader(204) })}
	for range 4 {
		go requests.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("admission did not fill")
		}
	}
	response := httptest.NewRecorder()
	requests.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 503 {
		t.Fatal("excess request queued or admitted")
	}
	requests.mu.Lock()
	requests.stopped = true
	requests.mu.Unlock()
	close(release)
	requests.active.Wait()
	response = httptest.NewRecorder()
	requests.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 503 {
		t.Fatal("stopped service admitted request")
	}
}

func TestManagedCacheDataConfigRoundTrip(t *testing.T) {
	config, _ := cacheDataConfigFixture(t)
	config.Garage = store.GarageConfig{}
	config.Managed = &cacheManagedConfig{CentralURL: "https://central.example.test", CentralTokenFile: "/etc/runway/central-token", StorageCAFile: "/etc/runway/storage-ca.pem"}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeCacheDataServiceConfig(encoded)
	if err != nil || decoded.Managed == nil {
		t.Fatalf("generated managed configuration refused: %v", err)
	}
}
