package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
)

type cacheServiceConfig struct {
	MetricsListen     string `json:"metrics_listen,omitempty"`
	DataMetricsListen string `json:"data_metrics_listen,omitempty"`
	CentralCAFile     string `json:"runway_ca_file"`
	Version           int    `json:"version"`
	RegionID          string `json:"region_id"`
	MeshRegionID      uint16 `json:"mesh_region_id"`
	Listen            string `json:"listen"`
	Audience          string `json:"audience"`
	CertificateFile   string `json:"certificate_file"`
	PrivateKeyFile    string `json:"private_key_file"`
	SigningKeyFile    string `json:"signing_key_file"`
	CacheDataCAFile   string `json:"cache_data_ca_file"`
	CentralURL        string `json:"runway_url"`
	CentralTokenFile  string `json:"runway_credential_file"`
	ArchiveBytes      int64  `json:"archive_bytes"`
}

func decodeCacheServiceConfig(data []byte) (cacheServiceConfig, error) {
	var c cacheServiceConfig
	fields := []string{"runway_ca_file", "version", "region_id", "mesh_region_id", "listen", "audience", "certificate_file", "private_key_file", "signing_key_file", "cache_data_ca_file", "runway_url", "runway_credential_file", "archive_bytes"}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return c, grant.ErrInvalid
	}
	for _, name := range []string{"metrics_listen", "data_metrics_listen"} {
		if _, ok := object[name]; ok {
			fields = append(fields, name)
		}
	}
	if readCacheObject(data, fields) != nil || json.Unmarshal(data, &c) != nil {
		return c, grant.ErrInvalid
	}
	for _, p := range []string{c.CertificateFile, c.PrivateKeyFile, c.SigningKeyFile, c.CacheDataCAFile, c.CentralTokenFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return c, grant.ErrInvalid
		}
	}
	if c.CentralCAFile != "" && (!filepath.IsAbs(c.CentralCAFile) || filepath.Clean(c.CentralCAFile) != c.CentralCAFile) {
		return c, grant.ErrInvalid
	}
	addr, err := netip.ParseAddrPort(c.Listen)
	if err != nil || addr.Port() == 0 || !cacheHTTPSOrigin(c.CentralURL) || c.Version != 1 || c.ArchiveBytes <= 0 || c.ArchiveBytes > 1<<30 {
		return c, grant.ErrInvalid
	}
	if !cacheServiceMeshAddress(addr.Addr(), c.MeshRegionID) || addr.Port() < 1024 {
		return c, grant.ErrInvalid
	}
	if _, err = grant.NewVerifier(map[string]ed25519.PublicKey{grant.KeyID(make([]byte, 32)): make([]byte, 32)}, c.Audience, c.RegionID); err != nil {
		return c, err
	}
	return c, nil
}
func cacheMeshPeer(peer string, region uint16) (netip.Addr, uint32, error) {
	ap, err := netip.ParseAddrPort(peer)
	if err != nil {
		return netip.Addr{}, 0, grant.ErrInvalid
	}
	ip := ap.Addr()
	if !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
		return ip, 0, grant.ErrInvalid
	}
	b := ip.As16()
	tenant := binary.BigEndian.Uint32(b[4:8])
	if b[0] != 0xfd || b[1] != 0xaa || binary.BigEndian.Uint16(b[2:4]) != region || tenant == 0 {
		return ip, 0, grant.ErrInvalid
	}
	return ip, tenant, nil
}
func ensureCacheServiceKey(path string) (ed25519.PrivateKey, error) {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return readCacheSigningKey(path)
	}
	if checkCachePrivateDirectory(filepath.Dir(path)) != nil {
		return nil, grant.ErrInvalid
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// Publish a fully synced key atomically without replacing an existing key.
	f, err := os.CreateTemp(filepath.Dir(path), ".cache-key-")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err = os.Link(name, path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	os.Remove(name)
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return nil, err
	}
	return readCacheSigningKey(path)
}

type cacheService struct {
	translatorErrors atomic.Uint64
	config           cacheServiceConfig
	central          cacheCentral
	template         cacheSessionConfig
	ctx              context.Context
	mu               sync.Mutex
	sessions         map[string]*cacheServiceSession
	slots            chan struct{}
}

// cacheServiceSession retains the mesh address's lease identity until expiry.
type cacheServiceSession struct {
	leaseID      string
	session      *cacheSession
	authenticate func() (cacheGuestBinding, string)
}

func (s *cacheService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/setup" && !strings.HasPrefix(r.URL.Path, "/_apis/artifactcache/") &&
		!strings.HasPrefix(r.URL.Path, "/download/") {
		http.NotFound(w, r)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		http.Error(w, "cache unavailable", 503)
		return
	}
	address, tenant, err := cacheMeshPeer(r.RemoteAddr, s.config.MeshRegionID)
	if err != nil {
		http.Error(w, "cache unavailable", 403)
		return
	}
	if r.URL.Path != "/setup" {
		s.mu.Lock()
		host := s.sessions[address.String()]
		s.mu.Unlock()
		if host == nil {
			s.translatorErrors.Add(1)
			http.Error(w, "cache unavailable", 503)
			return
		}
		host.session.translate(w, r, "")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method refused", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cacheSessionSetupBudget)
	defer cancel()
	deadline := time.Now().Add(cacheSessionSetupBudget)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	request, err := grant.ReadSetup(http.MaxBytesReader(w, r.Body, 32773))
	if err != nil {
		http.Error(w, "setup refused", 400)
		return
	}
	lease, err := s.central.lease(ctx, address)
	if err != nil || !s.validLease(lease, address, tenant) {
		_ = grant.WriteSetupResponse(w, request, false)
		return
	}
	s.mu.Lock()
	host := s.sessions[address.String()]
	if host != nil && host.leaseID != lease.Lease { // Retain the old lease until its deadline to fence address reuse.
		if !host.session.done() {
			s.mu.Unlock()
			_ = grant.WriteSetupResponse(w, request, false)
			return
		}
		host = nil
	}
	if host == nil {
		// Expired lease state is bounded; never evict a live replay/setup fence.
		for key, h := range s.sessions {
			if h.session.done() {
				h.session.close()
				delete(s.sessions, key)
			}
		}
		if len(s.sessions) >= 4096 {
			s.mu.Unlock()
			_ = grant.WriteSetupResponse(w, request, false)
			return
		}
		binding := cacheGuestBinding{service: true, TenantID: tenant, VMID: lease.VMID, RunnerID: lease.RunnerID, RunnerName: lease.RunnerName, Repository: lease.Repository, ExpiresAt: lease.Deadline}
		config := s.template
		config.id, config.deadline = lease.Lease, lease.Deadline
		config.nonceLimit, config.bindTickets = 4096, true
		config.translationError = func() { s.translatorErrors.Add(1) }
		config.authority = &cacheServiceAuthority{regionID: s.config.RegionID, central: s.central, address: address, lease: lease}
		host = &cacheServiceSession{leaseID: lease.Lease, session: newCacheSession(s.ctx, config), authenticate: func() (cacheGuestBinding, string) {
			if !time.Now().Before(binding.ExpiresAt) {
				return binding, "binding_expired"
			}
			return binding, ""
		}}
		s.sessions[address.String()] = host
	}
	s.mu.Unlock()
	// The Central lookup and the existing setup share one budget.
	reason := host.session.enable(ctx, request, host.authenticate)
	_ = grant.WriteSetupResponse(w, request, reason == "")
	s.template.log(map[string]any{"event": "setup", "lease": lease.Lease, "tenant_id": tenant, "enabled": reason == "", "reason": reason})
}
func (s *cacheService) validLease(l cacheServiceLease, address netip.Addr, tenant uint32) bool {
	ip, err := netip.ParseAddr(l.MeshAddress)
	j := l.Job
	b := cacheGuestBinding{service: true, TenantID: l.TenantID, VMID: l.VMID, RunnerID: l.RunnerID, RunnerName: l.RunnerName, Repository: l.Repository, ExpiresAt: l.Deadline}
	return err == nil && ip == address && l.TenantID == tenant && l.Region == s.config.RegionID && l.Lease != "" && l.Team != "" && b.validate() == nil && time.Now().Before(l.Deadline) && j != nil && canonicalCacheUUID(j.JobID) && j.RequestID >= 0 && j.RunID > 0 && j.Verified && j.RunAttempt > 0 && l.RepositoryID > 0 && l.DefaultBranch != "" && j.DisplayName != "" && strings.HasPrefix(j.WorkflowRef, l.Repository+"/.github/workflows/") && (j.EventName == "push" || j.EventName == "workflow_dispatch")
}
func runCacheService(path string) error {
	data, err := readProtectedCacheFile(path, true)
	if err != nil {
		return err
	}
	c, err := decodeCacheServiceConfig(data)
	if err != nil {
		return err
	}
	key, err := ensureCacheServiceKey(c.SigningKeyFile)
	if err != nil {
		return err
	}
	cert, err := readProtectedCacheFile(c.CertificateFile, false)
	if err != nil {
		return err
	}
	private, err := readProtectedCacheFile(c.PrivateKeyFile, true)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(cert, private)
	if err != nil {
		return errors.New("cache-service TLS refused")
	}
	ca, err := readProtectedCacheFile(c.CacheDataCAFile, false)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return grant.ErrInvalid
	}
	site, err := cacheCentralClient(c)
	if err != nil {
		return err
	}
	authority := cacheSiteLeaseClient{site: cacheCentralHTTPS{origin: c.CentralURL, client: site}, credential: func() (string, error) {
		return readCacheCredential(c.CentralTokenFile)
	}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service := &cacheService{config: c, ctx: ctx, central: authority, sessions: map[string]*cacheServiceSession{}, slots: make(chan struct{}, 64), template: cacheSessionConfig{key: key, roots: roots, clock: newCacheHostClock().observe, audience: c.Audience, archiveBytes: c.ArchiveBytes, log: cacheHostLogger(os.Stdout)}}
	closeMetrics, err := startMetrics(c.MetricsListen, service.writeMetrics)
	if err != nil {
		return err
	}
	defer closeMetrics()
	listener, err := net.Listen("tcp6", c.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: service, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: cacheSessionRequestLimit, WriteTimeout: cacheSessionRequestLimit, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		_ = server.Close()
		service.mu.Lock()
		defer service.mu.Unlock()
		for _, h := range service.sessions {
			h.session.close()
		}
	}()
	err = server.ServeTLS(listener, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *cacheService) writeMetrics(w io.Writer) {
	metric(w, "cache_translator_errors_total", "counter", "", s.translatorErrors.Load())
}
