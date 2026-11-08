package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/transport"
	"golang.org/x/sys/unix"
)

// The standalone cache host serves one Firecracker guest's guest-initiated
// vsock ports through that VM's own Unix sockets (<vm_dir>/<vm-id>/v.sock_PORT):
//
//	1026 cache setup: GitHub-verified job authority, host clock, signed grant
//	1027 job-bound translator to the regional cache-data service
//
// Every connection is authenticated by its socket peer: the connecting process
// must be the Firecracker process recorded in the root-written launcher binding
// for this VM. Request bodies never select the VM, runner or repository.
//
// Job identity is host-held: the first setup resolves the job GitHub assigned to
// the binding's runner ID and pins it as the only job this VM's ephemeral
// runner may set up a cache for, until the binding's expiry. The guest runs the
// stock Actions runner and never names its job. Admission is GitHub's
// assignment of a job to the JIT runner the launcher registered; the host adds
// no admission policy, quarantine or hold.
type cacheHostConfig struct {
	Version         int    `json:"version"`
	RegionID        string `json:"region_id"`
	Audience        string `json:"audience"`
	CacheDataCAFile string `json:"cache_data_ca_file"`
	SigningKeyFile  string `json:"signing_key_file"`
	GitHubTokenFile string `json:"github_token_file"`
	BindingDir      string `json:"binding_dir"`
	VMDir           string `json:"vm_dir"`
	AssignmentDir   string `json:"assignment_dir"`
	ArchiveBytes    int64  `json:"archive_bytes"`
}

const (
	cacheHostSetupBudget  = 2500 * time.Millisecond // inside the guest helper's 3 s exchange
	cacheHostRequestLimit = 5 * time.Minute
	cacheHostRenewBudget  = 5 * time.Second
)

const (
	cacheHostRenewWindow  = 6 * time.Minute
	cacheHostRenewBackoff = 5 * time.Second
)

func decodeCacheHostConfig(data []byte) (cacheHostConfig, error) {
	var config cacheHostConfig
	if readCacheObject(data, []string{"version", "region_id", "audience", "cache_data_ca_file", "signing_key_file", "github_token_file", "binding_dir", "vm_dir", "assignment_dir", "archive_bytes"}) != nil || json.Unmarshal(data, &config) != nil {
		return cacheHostConfig{}, grant.ErrInvalid
	}
	if _, err := grant.NewVerifier(map[string]ed25519.PublicKey{grant.KeyID(make([]byte, 32)): make([]byte, 32)}, config.Audience, config.RegionID); err != nil {
		return cacheHostConfig{}, grant.ErrInvalid
	}
	for _, path := range []string{config.CacheDataCAFile, config.SigningKeyFile, config.GitHubTokenFile, config.BindingDir, config.VMDir, config.AssignmentDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return cacheHostConfig{}, grant.ErrInvalid
		}
	}
	if config.Version != 1 || config.ArchiveBytes <= 0 || config.ArchiveBytes > 1<<30 {
		return cacheHostConfig{}, grant.ErrInvalid
	}
	return config, nil
}

func readCacheSigningKey(path string) (ed25519.PrivateKey, error) {
	data, err := readProtectedCacheFile(path, true)
	if err != nil {
		return nil, grant.ErrInvalid
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, grant.ErrInvalid
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return nil, grant.ErrInvalid
	}
	return key, nil
}

// runCacheHost serves one VM until SIGINT/SIGTERM (launcher teardown). A
// missing key, CA or credential only disables cache setup (the job keeps
// GitHub's cache); it never stops the host.
func runCacheHost(configPath, vmID string) error {
	if !cacheVMIDPattern.MatchString(vmID) {
		return errors.New("cache-host requires a valid --vm-id")
	}
	data, err := readProtectedCacheFile(configPath, true)
	if err != nil || len(data) > 4096 {
		return errors.New("cache-host configuration unavailable")
	}
	config, err := decodeCacheHostConfig(data)
	if err != nil {
		return errors.New("cache-host configuration refused")
	}
	vmDir := filepath.Join(config.VMDir, vmID)
	if _, err := cacheSocketDirectoryGroup(vmDir); err != nil {
		return errors.New("cache-host VM socket directory must be root-owned with mode 3770")
	}
	host := newCacheHost(config, vmID, cacheHostLogger(os.Stdout))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return host.serve(ctx)
}

// newCacheHost loads the grant key, the cache-data CA and the GitHub
// credential. A failure is recorded as the reason every setup is disabled.
func newCacheHost(config cacheHostConfig, vmID string, log func(map[string]any)) *cacheHost {
	host := &cacheHost{
		vmID: vmID, uds: filepath.Join(config.VMDir, vmID, "v.sock"), bindings: cacheBindingFiles{dir: config.BindingDir},
		clock: newCacheHostClock().observe, audience: config.Audience, archiveBytes: config.ArchiveBytes, peer: cacheUnixPeerCredentials, log: log,
	}
	key, keyErr := readCacheSigningKey(config.SigningKeyFile)
	ca, caErr := readProtectedCacheFile(config.CacheDataCAFile, false)
	roots := x509.NewCertPool()
	client, clientErr := newCacheRepositoryClient(config.GitHubTokenFile)
	switch {
	case keyErr != nil:
		host.unavailable = "signing_key_unavailable"
	case caErr != nil || !roots.AppendCertsFromPEM(ca):
		host.unavailable = "cache_data_ca_unavailable"
	case clientErr != nil:
		host.unavailable = "github_credential_unavailable"
	default:
		host.key, host.roots = key, roots
		host.authority = &githubCacheJobAuthority{client: client, assignments: cacheAssignmentFiles{dir: config.AssignmentDir}, regionID: config.RegionID, interval: time.Second, now: time.Now}
	}
	if host.unavailable != "" {
		log(map[string]any{"event": "setup_unavailable", "vm_id": vmID, "reason": host.unavailable})
	}
	return host
}

type cacheHost struct {
	authenticateService func() (cacheGuestBinding, string)
	vmID, uds           string
	bindings            cacheBindingSource
	authority           cacheJobAuthority // nil when unavailable is set
	unavailable         string            // why setup is disabled for this whole VM
	clock               func(context.Context) (grant.Clock, error)
	key                 ed25519.PrivateKey
	audience            string
	roots               *x509.CertPool
	archiveBytes        int64
	peer                func(net.Conn) (*unix.Ucred, error)
	directoryGroup      func(string) (uint32, error)
	log                 func(map[string]any)
	renewWindow         time.Duration // renew this long before expiry; zero is cacheHostRenewWindow

	ctx        context.Context
	mu         sync.Mutex
	release    *cacheJobRelease // the resolved job, pinned for this VM
	confirming chan struct{}    // closed when the in-flight resolution ends
	nonces     map[string]bool
	setup      *grant.SetupRequest // the one enabled setup of this job
	current    *transport.Handler
	expires    time.Time
}

// Connection slots per port: setup is one exchange per job, and the relay
// opens at most eight translator connections.
var cacheHostSlots = map[int]int{1026: 2, 1027: 16}

func cacheHostLogger(out io.Writer) func(map[string]any) {
	var mu sync.Mutex
	return func(fields map[string]any) {
		fields["t"] = time.Now().UTC().Format(time.RFC3339Nano)
		data, _ := json.Marshal(fields)
		mu.Lock()
		out.Write(append(data, '\n'))
		mu.Unlock()
	}
}

// cacheUnixPeerCredentials identifies the connecting Firecracker process (SO_PEERCRED).
func cacheUnixPeerCredentials(conn net.Conn) (*unix.Ucred, error) {
	if slot, ok := conn.(*cacheHostSlotConn); ok {
		conn = slot.Conn
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, grant.ErrInvalid
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var credential *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credential, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if credentialErr != nil || credential == nil || credential.Pid <= 0 {
		return nil, grant.ErrInvalid
	}
	return credential, nil
}

// authenticate binds a connection to this VM's launcher binding by peer PID, uid and gid.
func (host *cacheHost) authenticate(conn net.Conn) (cacheGuestBinding, string) {
	if host.authenticateService != nil {
		return host.authenticateService()
	}
	peer, err := host.peer(conn)
	if err != nil {
		return cacheGuestBinding{}, "peer_unknown"
	}
	binding, err := host.bindings.lookup(host.vmID)
	switch {
	case errors.Is(err, errCacheUnbound):
		return cacheGuestBinding{}, "unbound"
	case err != nil:
		return cacheGuestBinding{}, "binding_invalid"
	case binding.VsockUDS != host.uds:
		return cacheGuestBinding{}, "binding_socket_mismatch"
	case binding.FirecrackerPID != int(peer.Pid) || binding.JailUID != peer.Uid || binding.JailGID != peer.Gid:
		return cacheGuestBinding{}, "peer_mismatch"
	case !time.Now().Before(binding.ExpiresAt):
		return cacheGuestBinding{}, "binding_expired"
	}
	group, err := host.socketDirectoryGroup()
	if err != nil || group != binding.JailGID {
		return cacheGuestBinding{}, "peer_mismatch"
	}
	return binding, ""
}

func (host *cacheHost) socketDirectoryGroup() (uint32, error) {
	if host.directoryGroup != nil {
		return host.directoryGroup(filepath.Dir(host.uds))
	}
	return cacheSocketDirectoryGroup(filepath.Dir(host.uds))
}

func (host *cacheHost) serve(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	host.ctx = ctx
	host.nonces = map[string]bool{}
	group, err := host.socketDirectoryGroup()
	if err != nil {
		return errors.New("cache-host VM socket directory refused")
	}
	listeners := map[int]net.Listener{}
	old := syscall.Umask(0077)
	for _, port := range []int{1026, 1027} {
		path := host.uds + "_" + strconv.Itoa(port)
		_ = os.Remove(path)
		listener, err := net.Listen("unix", path)
		if err != nil {
			syscall.Umask(old)
			return errors.New("cache-host vsock listener unavailable")
		}
		defer os.Remove(path)
		// The setgid directory already gave the socket its group; the unit has no CAP_CHOWN.
		if info, err := os.Lstat(path); err != nil || info.Sys().(*syscall.Stat_t).Gid != group {
			listener.Close()
			syscall.Umask(old)
			return errors.New("cache-host vsock socket did not inherit the jail group")
		}
		if err := os.Chmod(path, 0660); err != nil {
			listener.Close()
			syscall.Umask(old)
			return err
		}
		defer listener.Close()
		listeners[port] = &cacheHostLimitedListener{Listener: listener, slots: make(chan struct{}, cacheHostSlots[port])}
	}
	syscall.Umask(old)
	keyID := ""
	if host.key != nil {
		keyID = grant.KeyID(host.key.Public().(ed25519.PublicKey))
	}
	host.log(map[string]any{"event": "listening", "vm_id": host.vmID, "uds": host.uds, "key_id": keyID})
	go host.accept(listeners[1026], host.serveSetup, 5*time.Second)
	server := &http.Server{
		Handler: http.HandlerFunc(host.translate), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16384,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			binding, reason := host.authenticate(conn)
			return context.WithValue(ctx, cacheHostPeerKey{}, cacheHostPeer{binding: binding, reason: reason})
		},
	}
	// Firecracker does not forward the guest's write half-close; close after each response.
	server.SetKeepAlivesEnabled(false)
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	err = server.Serve(listeners[1027])
	host.mu.Lock()
	if host.current != nil {
		host.current.Close()
	}
	host.mu.Unlock()
	host.mu.Lock()
	enabled := host.setup != nil
	host.mu.Unlock()
	// A guest that never enabled setup ran its job on GitHub's cache.
	host.log(map[string]any{"event": "stopped", "vm_id": host.vmID, "setup_enabled": enabled})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (host *cacheHost) accept(listener net.Listener, serve func(net.Conn), deadline time.Duration) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(deadline))
			serve(conn)
		}()
	}
}

// cacheHostLimitedListener closes connections beyond its slots at once, so a
// guest cannot queue unbounded work (or GitHub reads) on the host.
type cacheHostLimitedListener struct {
	net.Listener
	slots chan struct{}
}

func (listener *cacheHostLimitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case listener.slots <- struct{}{}:
			return &cacheHostSlotConn{Conn: conn, release: func() { <-listener.slots }}, nil
		default:
			conn.Close()
		}
	}
}

type cacheHostSlotConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (conn *cacheHostSlotConn) Close() error {
	conn.once.Do(conn.release)
	return conn.Conn.Close()
}

func (host *cacheHost) serveSetup(conn net.Conn) {
	request, err := grant.ReadSetup(conn)
	if err != nil {
		host.log(map[string]any{"event": "setup_malformed", "vm_id": host.vmID})
		return
	}
	reason := host.enable(conn, request)
	if err := grant.WriteSetupResponse(conn, request, reason == ""); err != nil && reason == "" {
		reason = "write_failed"
	}
	text := func(value *string) any {
		if value == nil {
			return nil
		}
		return *value
	}
	host.mu.Lock()
	jobID, requestID := "", int64(0)
	if host.release != nil {
		jobID, requestID = host.release.JobID, host.release.RequestID
	}
	host.mu.Unlock()
	c := request.Context
	host.log(map[string]any{"event": "setup", "vm_id": host.vmID, "enabled": reason == "", "reason": reason, "job_id": jobID, "request_id": requestID, "repository": text(c.Repository), "run_id": text(c.RunID), "run_attempt": text(c.RunAttempt), "ref": text(c.Ref), "event_name": text(c.EventName), "sha": text(c.SHA)})
}

// enable returns "" after installing a translator, or the refusal reason. A
// job gets at most one enabled setup; refused attempts cost the guest a
// connection slot and share one resolution.
func (host *cacheHost) enable(conn net.Conn, request grant.SetupRequest) string {
	return host.enableWithin(host.ctx, conn, request)
}

func (host *cacheHost) enableWithin(parent context.Context, conn net.Conn, request grant.SetupRequest) string {
	binding, reason := host.authenticate(conn)
	if reason != "" {
		return reason
	}
	if host.unavailable != "" {
		return host.unavailable
	}
	host.mu.Lock()
	enabled, replay := host.setup != nil, host.nonces[request.Nonce]
	if host.authenticateService != nil && len(host.nonces) >= 4096 {
		host.mu.Unlock()
		return "nonce_limit"
	}
	if !replay && !enabled {
		host.nonces[request.Nonce] = true
	}
	host.mu.Unlock()
	switch {
	case enabled:
		return "already_enabled"
	case replay:
		return "nonce_replayed"
	}
	ctx, cancel := context.WithTimeout(parent, cacheHostSetupBudget)
	defer cancel()
	release := host.resolveWithin(ctx, binding, cacheRunHint(request))
	if release == nil {
		return "unconfirmed"
	}
	handler, expires, reason := host.issue(ctx, binding, *release, request)
	if reason != "" {
		return reason
	}
	// Teardown may remove the binding while GitHub was being asked.
	if _, reason := host.authenticate(conn); reason != "" {
		handler.Close()
		return reason
	}
	host.mu.Lock()
	if host.setup != nil {
		host.mu.Unlock()
		handler.Close()
		return "already_enabled"
	}
	accepted := request
	host.current, host.expires, host.setup = handler, expires, &accepted
	host.mu.Unlock()
	go host.renew(binding, *release, request, expires)
	return ""
}

// cacheRunHint is the guest-reported run ID, or 0. It only selects which run's
// REST jobs to read; the bound runner ID decides which job is this VM's.
func cacheRunHint(request grant.SetupRequest) int64 {
	if request.Context.RunID == nil || !cacheCanonicalPositive(*request.Context.RunID) {
		return 0
	}
	runID, _ := strconv.ParseInt(*request.Context.RunID, 10, 64)
	return runID
}

// resolveWithin returns the pinned job, resolving it first if needed.
// Concurrent callers share one resolution instead of each polling GitHub.
func (host *cacheHost) resolveWithin(ctx context.Context, binding cacheGuestBinding, runHint int64) *cacheJobRelease {
	host.mu.Lock()
	release, inFlight := host.release, host.confirming
	if release != nil || inFlight != nil {
		host.mu.Unlock()
		if inFlight != nil {
			select {
			case <-inFlight:
			case <-ctx.Done():
			}
			host.mu.Lock()
			defer host.mu.Unlock()
			return host.release
		}
		return release
	}
	done := make(chan struct{})
	host.confirming = done
	host.mu.Unlock()
	defer func() {
		host.mu.Lock()
		host.confirming = nil
		close(done)
		host.mu.Unlock()
	}()
	started := time.Now()
	resolved, err := host.authority.resolve(ctx, binding, runHint)
	if err != nil {
		host.log(map[string]any{"event": "confirm_refused", "vm_id": host.vmID, "reason": cacheAuthorityReason(err), "ms": time.Since(started).Milliseconds()})
		return nil
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.release == nil {
		host.release = &resolved // an ephemeral runner runs exactly one job
	}
	confirmed := host.release.Confirmed
	host.log(map[string]any{"event": "confirmed", "vm_id": host.vmID, "job_id": host.release.JobID, "request_id": host.release.RequestID, "source": confirmed.Source, "run_id": confirmed.RunID, "rest_job_id": confirmed.RESTJobID, "workflow_ref": confirmed.WorkflowRef, "ms": time.Since(started).Milliseconds()})
	return host.release
}

// issue asks the job authority, observes the host clock and signs one grant.
func (host *cacheHost) issue(ctx context.Context, binding cacheGuestBinding, release cacheJobRelease, request grant.SetupRequest) (handler *transport.Handler, expires time.Time, reason string) {
	verified, err := host.authority.authorize(ctx, binding, release, request)
	if err != nil {
		return nil, time.Time{}, cacheAuthorityReason(err)
	}
	clock, err := host.clock(ctx)
	if err != nil {
		return nil, time.Time{}, "clock_unavailable"
	}
	bearer, err := grant.IssueSetup(host.key, host.audience, verified.Authorization, verified.Policy, request, clock)
	if err != nil {
		return nil, time.Time{}, "grant_refused"
	}
	expires = verified.Authorization.Deadline
	if limit := clock.Earliest.Add(30 * time.Minute); expires.After(limit) {
		expires = limit
	}
	// Stop using the grant slightly before cache-data would refuse it.
	expires = time.Unix(expires.Unix(), 0).Add(-5 * time.Second)
	handler, err = transport.New(host.ctx, transport.Config{
		BindTickets: binding.service,
		Grant:       transport.Grant{Bearer: bearer, Expires: expires}, Origin: host.audience, Roots: host.roots,
		Limits: transport.Limits{RequestBytes: 64 << 10, ChunkBytes: 64 << 20, ResponseBytes: 64 << 10, HeaderBytes: 16384, ArchiveBytes: host.archiveBytes, URLBytes: 4096, Concurrent: 8, Timeout: cacheHostRequestLimit},
	})
	if err != nil {
		return nil, time.Time{}, "translator_refused"
	}
	return handler, expires, ""
}

func cacheAuthorityReason(err error) string {
	switch {
	case errors.Is(err, errCacheJobNotFound):
		return "assignment_unconfirmed"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "github_timeout"
	case errors.Is(err, errCacheJobCompleted):
		return "github_job_not_in_progress"
	case errors.Is(err, errCacheJobMismatch):
		return "github_job_mismatch"
	default:
		return "github_refused"
	}
}

type cacheHostPeerKey struct{}

type cacheHostPeer struct {
	binding cacheGuestBinding
	reason  string
}

// handler returns the current translator until its grant expires.
func (host *cacheHost) handler() *transport.Handler {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.current == nil || !time.Now().Before(host.expires) {
		return nil
	}
	return host.current
}

// renew reissues the grant shortly before it expires, after a fresh check that
// the job is still running, retrying with backoff. Requests keep using the
// current translator meanwhile; it serves its in-flight requests to expiry.
func (host *cacheHost) renew(binding cacheGuestBinding, release cacheJobRelease, request grant.SetupRequest, expires time.Time) {
	window := cacheHostRenewWindow
	if host.renewWindow > 0 {
		window = host.renewWindow
	}
	backoff := time.Duration(0)
	for wait := time.Until(expires) - window; ; {
		select {
		case <-host.ctx.Done():
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(host.ctx, cacheHostRenewBudget)
		handler, next, reason := host.issue(ctx, binding, release, request)
		cancel()
		host.log(map[string]any{"event": "renew", "vm_id": host.vmID, "ok": reason == "", "reason": reason})
		if reason == "" && !next.After(expires) {
			handler.Close() // capped by the binding: renewal cannot extend the grant
			return
		}
		if reason == "" {
			host.mu.Lock()
			previous := host.current
			host.current, host.expires = handler, next
			host.mu.Unlock()
			time.AfterFunc(time.Until(expires)+time.Second, previous.Close)
			expires, wait, backoff = next, time.Until(next)-window, 0
			continue
		}
		backoff = min(max(2*backoff, cacheHostRenewBackoff), time.Minute)
		if reason == "github_job_not_in_progress" || !time.Now().Add(backoff).Before(expires) {
			return
		}
		wait = backoff
	}
}

type cacheHostRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *cacheHostRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *cacheHostRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *cacheHostRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (host *cacheHost) translate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	recorder := &cacheHostRecorder{ResponseWriter: w}
	path := r.URL.Path // never log the query: it carries download tickets
	reason := ""
	defer func() {
		recovered := recover()
		host.log(map[string]any{"event": "http", "vm_id": host.vmID, "method": r.Method, "path": path, "status": recorder.status, "request_bytes": r.ContentLength, "response_bytes": recorder.bytes, "ms": time.Since(start).Milliseconds(), "reason": reason, "aborted": recovered != nil})
		if recovered != nil {
			panic(recovered)
		}
	}()
	peer, _ := r.Context().Value(cacheHostPeerKey{}).(cacheHostPeer)
	if peer.reason != "" || peer.binding.VMID == "" {
		reason = peer.reason
		http.Error(recorder, "cache unavailable", http.StatusForbidden)
		return
	}
	handler := host.handler()
	if handler == nil {
		reason = "no_grant"
		http.Error(recorder, "cache unavailable", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(recorder, r)
}
