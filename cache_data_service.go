package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/store"
)

// cacheDataServiceConfig is the strict cache-data configuration. It names
// Garage storage explicitly; there are no ambient credentials or discovery.
type cacheDataServiceConfig struct {
	Version         int                  `json:"version"`
	Listen          string               `json:"listen"`
	PublicOrigin    string               `json:"public_origin"`
	CertificateFile string               `json:"certificate_file"`
	PrivateKeyFile  string               `json:"private_key_file"`
	PublicKeys      map[string]string    `json:"public_keys"`
	RegionID        string               `json:"region_id"`
	StateDir        string               `json:"state_dir"`
	QuotaBytes      int64                `json:"quota_bytes"`
	TotalBytes      int64                `json:"total_bytes"`
	MaxArchiveBytes int64                `json:"max_archive_bytes"`
	Disk            store.DiskConfig     `json:"disk"`
	StorageBudget   *store.StorageBudget `json:"storage_budget,omitempty"`
	Managed         *cacheManagedConfig  `json:"managed,omitempty"`
	Garage          store.GarageConfig   `json:"garage,omitzero"`
}

var cacheDataNestedFields = map[string][]string{
	"disk":           {"directory", "physical_bytes"},
	"garage":         {"endpoint", "region", "bucket", "access_key", "secret_key"},
	"storage_budget": {"base_seconds", "min_bytes_per_second", "max_seconds", "recovery_max_seconds"},
}

// decodeCacheDataServiceConfig requires exactly the known fields at every
// level (no duplicates, nulls or case aliases) and validates what the store
// does not: the listener, the file paths and the grant keys. store.New
// validates the storage bounds.
func decodeCacheDataServiceConfig(data []byte) (cacheDataServiceConfig, store.Config, error) {
	var config cacheDataServiceConfig
	var fields map[string]json.RawMessage
	if len(data) > 4096 || json.Unmarshal(data, &fields) != nil {
		return config, store.Config{}, grant.ErrInvalid
	}
	required := []string{"version", "listen", "public_origin", "certificate_file", "private_key_file", "public_keys", "region_id", "state_dir", "quota_bytes", "total_bytes", "max_archive_bytes", "disk"}
	if _, ok := fields["managed"]; ok {
		required = append(required, "managed")
	} else {
		required = append(required, "garage")
	}
	if _, ok := fields["storage_budget"]; ok {
		required = append(required, "storage_budget")
	}
	if raw, ok := fields["managed"]; ok {
		if readCacheObject(raw, managedConfigFields(raw)) != nil {
			return config, store.Config{}, grant.ErrInvalid
		}
	}
	if readCacheObject(data, required) != nil {
		return config, store.Config{}, grant.ErrInvalid
	}
	for name, names := range cacheDataNestedFields {
		if name == "garage" {
			var garageFields map[string]json.RawMessage
			if json.Unmarshal(fields[name], &garageFields) == nil {
				if _, ok := garageFields["ca_pem"]; ok {
					names = append(append([]string(nil), names...), "ca_pem")
				}
			}
		}
		if raw, ok := fields[name]; ok && readCacheObject(raw, names) != nil {
			return config, store.Config{}, grant.ErrInvalid
		}
	}
	if json.Unmarshal(data, &config) != nil {
		return config, store.Config{}, grant.ErrInvalid
	}
	if m := config.Managed; m != nil {
		if !cacheHTTPSOrigin(m.CentralURL) || !filepath.IsAbs(m.CentralTokenFile) || !filepath.IsAbs(m.StorageCAFile) || m.CleanupStaleHours < 0 || m.CleanupStaleHours > 8760 || m.CleanupStaleHours > 0 && m.CleanupStaleHours <= 2 {
			return config, store.Config{}, grant.ErrInvalid
		}

	}
	host, port, err := net.SplitHostPort(config.Listen)
	address := net.ParseIP(host)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number <= 0 || number > 65535 || address == nil || !(address.IsLoopback() || address.IsPrivate()) || address.IsUnspecified() || config.Version != 1 || !filepath.IsAbs(config.StateDir) || !filepath.IsAbs(config.CertificateFile) || !filepath.IsAbs(config.PrivateKeyFile) || !filepath.IsAbs(config.Disk.Directory) {
		return config, store.Config{}, grant.ErrInvalid
	}
	keys := make(map[string]ed25519.PublicKey, len(config.PublicKeys))
	for id, encoded := range config.PublicKeys {
		key, err := hex.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize || hex.EncodeToString(key) != encoded {
			return config, store.Config{}, grant.ErrInvalid
		}
		keys[id] = ed25519.PublicKey(key)
	}
	verifier, err := grant.NewVerifier(keys, config.PublicOrigin, config.RegionID)
	if err != nil {
		return config, store.Config{}, grant.ErrInvalid
	}
	return config, store.Config{
		PublicOrigin: config.PublicOrigin, Verifier: verifier, StateDir: config.StateDir, Disk: config.Disk, Garage: config.Garage,
		StorageBudget: config.StorageBudget, QuotaBytes: config.QuotaBytes, TotalBytes: config.TotalBytes, MaxArchiveBytes: config.MaxArchiveBytes,
	}, nil
}

func runCacheDataService(configPath string) error {
	data, err := readProtectedCacheFile(configPath, true)
	if err != nil {
		return grant.ErrInvalid
	}
	config, core, err := decodeCacheDataServiceConfig(data)
	if err != nil {
		return grant.ErrInvalid
	}
	if m := config.Managed; m != nil {
		token, err := readProtectedCacheFile(m.CentralTokenFile, true)
		if _, statErr := os.Lstat(m.CentralTokenFile); err != nil && !errors.Is(statErr, os.ErrNotExist) {
			return grant.ErrInvalid
		}
		ca, err := readProtectedCacheFile(m.StorageCAFile, false)
		if err != nil {
			return grant.ErrInvalid
		}
		if len(ca) == 0 {
			ca, err = os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
			if err != nil {
				return grant.ErrInvalid
			}
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return grant.ErrInvalid
		}
		if m.CentralCAFile != "" {
			centralCA, err := readProtectedCacheFile(m.CentralCAFile, false)
			if err != nil || !roots.AppendCertsFromPEM(centralCA) {
				return grant.ErrInvalid
			}
		}
		if len(ca) > 0 && !x509.NewCertPool().AppendCertsFromPEM(ca) {
			return grant.ErrInvalid
		}
		core.CleanupStaleAfter = time.Duration(m.CleanupStaleHours) * time.Hour
		core.Buckets = &cacheBucketProvider{
			central: cacheCentralHTTPS{origin: m.CentralURL, token: strings.TrimSpace(string(token)), client: &http.Client{Timeout: cacheHostSetupBudget, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}},
			ca:      string(ca),
			credential: func() (string, error) {
				token, err := readProtectedCacheFile(m.CentralTokenFile, true)
				return strings.TrimSpace(string(token)), err
			},
		}
	}
	certificate, certErr := readProtectedCacheFile(config.CertificateFile, false)
	privateKey, keyErr := readProtectedCacheFile(config.PrivateKeyFile, true)
	if certErr != nil || keyErr != nil || len(certificate) > 4096 || len(privateKey) > 4096 {
		return grant.ErrInvalid
	}
	pair, err := tls.X509KeyPair(certificate, privateKey)
	if err != nil {
		return grant.ErrInvalid
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return startCacheDataService(ctx, config.Listen, core, pair, newCacheHostClock().observe)
}

// startCacheDataService serves until ctx ends. The injected clock keeps local
// tests independent of the host kernel clock; the command always uses adjtimex.
func startCacheDataService(ctx context.Context, listen string, core store.Config, pair tls.Certificate, observe func(context.Context) (grant.Clock, error)) (result error) {
	if observe == nil || len(pair.Certificate) == 0 || pair.PrivateKey == nil {
		return grant.ErrInvalid
	}
	core.Clock = observe
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return errors.New("cache data listener unavailable")
	}
	defer listener.Close()
	service, err := store.New(core)
	if err != nil {
		return errors.New("cache data initialization refused")
	}
	defer func() {
		closeContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if service.Close(closeContext) != nil {
			result = errors.Join(result, errors.New("cache data state close failed"))
		}
	}()
	budget := store.DefaultStorageBudget
	if core.StorageBudget != nil {
		budget = *core.StorageBudget
	}
	// Finalize and download wait on scaled storage work; give exactly those
	// routes the storage ceiling plus the ordinary 30-second transfer bound.
	requests := &cacheDataServiceRequests{handler: service, slots: make(chan struct{}, 8), storage: time.Duration(budget.MaxSeconds)*time.Second + 30*time.Second}
	server := &http.Server{
		Handler: requests, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}},
		ReadHeaderTimeout: time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0),
	}
	return serveCacheDataService(ctx, server, listener, requests)
}

// Shutdown/Close do not join forced-closed HTTP handlers. Track admission under
// a mutex so Wait cannot race with Add, and join before releasing journal custody.
type cacheDataServiceRequests struct {
	handler http.Handler
	storage time.Duration
	slots   chan struct{}
	mu      sync.Mutex
	stopped bool
	active  sync.WaitGroup
}

func (requests *cacheDataServiceRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requests.mu.Lock()
	if requests.stopped || r.Context().Err() != nil {
		requests.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	select {
	case requests.slots <- struct{}{}:
		requests.active.Add(1)
	default:
		requests.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	requests.mu.Unlock()
	defer requests.active.Done()
	defer func() { <-requests.slots }()
	if requests.storage > 0 && cacheDataStorageRoute(r) {
		// Server Read/WriteTimeout stay 30 s for everything else. A lapsed read
		// deadline would cancel the request context in the middle of finalize.
		deadline := time.Now().Add(requests.storage)
		controller := http.NewResponseController(w)
		if controller.SetReadDeadline(deadline) != nil || controller.SetWriteDeadline(deadline) != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
	}
	requests.handler.ServeHTTP(w, r)
}

// cacheDataStorageRoute matches finalize (POST caches/{id}) and archive download.
// Their bodies stay bounded by the core's small control limits.
func cacheDataStorageRoute(r *http.Request) bool {
	path := r.URL.Path
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return strings.HasPrefix(path, "/download/")
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(path, "/_apis/artifactcache/caches/") {
		return false
	}
	id := strings.TrimPrefix(path, "/_apis/artifactcache/caches/")
	_, err := strconv.ParseInt(id, 10, 64)
	return err == nil
}

func serveCacheDataService(parent context.Context, server *http.Server, listener net.Listener, requests *cacheDataServiceRequests) error {
	defer listener.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		requests.mu.Lock()
		requests.stopped = true
		requests.mu.Unlock()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		err := server.Shutdown(shutdown)
		err = errors.Join(err, server.Close()) // unblock body reads even when graceful shutdown failed
		requests.active.Wait()
		done <- err
	}()
	err := server.ServeTLS(listener, "", "")
	cancel()
	if shutdownError := <-done; shutdownError != nil {
		return errors.New("cache data shutdown incomplete")
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("cache data service stopped")
}

func managedConfigFields(raw json.RawMessage) []string {
	fields := []string{"central_url", "central_token_file", "storage_ca_file"}
	var obj map[string]json.RawMessage
	json.Unmarshal(raw, &obj)
	if _, ok := obj["central_ca_file"]; ok {
		fields = append(fields, "central_ca_file")
	}
	if _, ok := obj["cleanup_stale_hours"]; ok {
		fields = append(fields, "cleanup_stale_hours")
	}
	return fields
}
