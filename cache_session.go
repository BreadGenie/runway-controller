package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/transport"
)

const (
	cacheSessionSetupBudget  = 2500 * time.Millisecond // inside the guest helper's 3 s exchange
	cacheSessionRequestLimit = 5 * time.Minute
	cacheSessionRenewBudget  = 5 * time.Second
)

const (
	cacheSessionRenewWindow  = 6 * time.Minute
	cacheSessionRenewBackoff = 5 * time.Second
)

// cacheSessionConfig supplies immutable dependencies and adapter-specific policy.
// Authentication stays with the adapter; the session rechecks it before accepting setup.
type cacheSessionConfig struct {
	deadline     time.Time // managed Runner Lease stop; zero uses the parent lifetime
	id           string
	authority    cacheJobAuthority
	unavailable  string
	clock        func(context.Context) (grant.Clock, error)
	key          ed25519.PrivateKey
	audience     string
	roots        *x509.CertPool
	archiveBytes int64
	log          func(map[string]any)
	renewWindow  time.Duration
	nonceLimit   int // zero preserves the standalone lifetime-bounded behavior
	bindTickets  bool
}

// cacheSession owns one job's cache access independently of its ingress transport.
// Callers enable setup, translate authenticated requests and close the session;
// replay fencing, evidence, renewal and translator lifetimes stay here.
type cacheSession struct {
	config     cacheSessionConfig
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	release    *cacheJobRelease
	confirming chan struct{}
	nonces     map[string]bool
	setup      *grant.SetupRequest
	current    *transport.Handler
	expires    time.Time
}

func newCacheSession(parent context.Context, config cacheSessionConfig) *cacheSession {
	var ctx context.Context
	var cancel context.CancelFunc
	if config.deadline.IsZero() {
		ctx, cancel = context.WithCancel(parent)
	} else {
		ctx, cancel = context.WithDeadline(parent, config.deadline)
	}
	return &cacheSession{config: config, ctx: ctx, cancel: cancel, nonces: map[string]bool{}}
}

func (session *cacheSession) close() {
	session.cancel()
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.current != nil {
		session.current.Close()
	}
}

func (session *cacheSession) done() bool { return session.ctx.Err() != nil }

func (session *cacheSession) enabled() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.setup != nil
}

func (session *cacheSession) jobIdentity() (string, int64) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.release == nil {
		return "", 0
	}
	return session.release.JobID, session.release.RequestID
}

// enable returns an empty refusal reason only after installing a translator.
func (session *cacheSession) enable(parent context.Context, request grant.SetupRequest, authenticate func() (cacheGuestBinding, string)) string {
	if session.done() {
		return "binding_expired"
	}
	binding, reason := authenticate()
	if reason != "" {
		return reason
	}
	if session.config.unavailable != "" {
		return session.config.unavailable
	}
	session.mu.Lock()
	enabled, replay := session.setup != nil, session.nonces[request.Nonce]
	if session.config.nonceLimit > 0 && len(session.nonces) >= session.config.nonceLimit {
		session.mu.Unlock()
		return "nonce_limit"
	}
	if !replay && !enabled {
		session.nonces[request.Nonce] = true
	}
	session.mu.Unlock()
	switch {
	case enabled:
		return "already_enabled"
	case replay:
		return "nonce_replayed"
	}
	ctx, cancel := context.WithTimeout(parent, cacheSessionSetupBudget)
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	release := session.resolveWithin(ctx, binding, cacheRunHint(request))
	if release == nil {
		return "unconfirmed"
	}
	handler, expires, reason := session.issue(ctx, binding, *release, request)
	if reason != "" {
		return reason
	}
	// Teardown may remove the binding while GitHub was being asked.
	if _, reason := authenticate(); reason != "" {
		handler.Close()
		return reason
	}
	session.mu.Lock()
	if session.done() {
		session.mu.Unlock()
		handler.Close()
		return "binding_expired"
	}
	if session.setup != nil {
		session.mu.Unlock()
		handler.Close()
		return "already_enabled"
	}
	accepted := request
	session.current, session.expires, session.setup = handler, expires, &accepted
	session.mu.Unlock()
	go session.renew(binding, *release, request, expires)
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
func (session *cacheSession) resolveWithin(ctx context.Context, binding cacheGuestBinding, runHint int64) *cacheJobRelease {
	session.mu.Lock()
	release, inFlight := session.release, session.confirming
	if release != nil || inFlight != nil {
		session.mu.Unlock()
		if inFlight != nil {
			select {
			case <-inFlight:
			case <-ctx.Done():
			}
			session.mu.Lock()
			defer session.mu.Unlock()
			return session.release
		}
		return release
	}
	done := make(chan struct{})
	session.confirming = done
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.confirming = nil
		close(done)
		session.mu.Unlock()
	}()
	started := time.Now()
	resolved, err := session.config.authority.resolve(ctx, binding, runHint)
	if err != nil {
		session.config.log(map[string]any{"event": "confirm_refused", "vm_id": session.config.id, "reason": cacheAuthorityReason(err), "ms": time.Since(started).Milliseconds()})
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.release == nil {
		session.release = &resolved // an ephemeral runner runs exactly one job
	}
	confirmed := session.release.Confirmed
	session.config.log(map[string]any{"event": "confirmed", "vm_id": session.config.id, "job_id": session.release.JobID, "request_id": session.release.RequestID, "source": confirmed.Source, "run_id": confirmed.RunID, "rest_job_id": confirmed.RESTJobID, "workflow_ref": confirmed.WorkflowRef, "ms": time.Since(started).Milliseconds()})
	return session.release
}

// issue asks the job authority, observes the host clock and signs one grant.
func (session *cacheSession) issue(ctx context.Context, binding cacheGuestBinding, release cacheJobRelease, request grant.SetupRequest) (handler *transport.Handler, expires time.Time, reason string) {
	verified, err := session.config.authority.authorize(ctx, binding, release, request)
	if err != nil {
		return nil, time.Time{}, cacheAuthorityReason(err)
	}
	clock, err := session.config.clock(ctx)
	if err != nil {
		return nil, time.Time{}, "clock_unavailable"
	}
	bearer, err := grant.IssueSetup(session.config.key, session.config.audience, verified.Authorization, verified.Policy, request, clock)
	if err != nil {
		return nil, time.Time{}, "grant_refused"
	}
	expires = verified.Authorization.Deadline
	if limit := clock.Earliest.Add(30 * time.Minute); expires.After(limit) {
		expires = limit
	}
	// Stop using the grant slightly before cache-data would refuse it.
	expires = time.Unix(expires.Unix(), 0).Add(-5 * time.Second)
	handler, err = transport.New(session.ctx, transport.Config{
		BindTickets: session.config.bindTickets,
		Grant:       transport.Grant{Bearer: bearer, Expires: expires}, Origin: session.config.audience, Roots: session.config.roots,
		Limits: transport.Limits{RequestBytes: 64 << 10, ChunkBytes: 64 << 20, ResponseBytes: 64 << 10, HeaderBytes: 16384, ArchiveBytes: session.config.archiveBytes, URLBytes: 4096, Concurrent: 8, Timeout: cacheSessionRequestLimit},
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

// handler returns the current translator until its grant expires.
func (session *cacheSession) handler() *transport.Handler {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.done() || session.current == nil || !time.Now().Before(session.expires) {
		return nil
	}
	return session.current
}

// renew reissues the grant shortly before it expires, after a fresh check that
// the job is still running, retrying with backoff. Requests keep using the
// current translator meanwhile; it serves its in-flight requests to expiry.
func (session *cacheSession) renew(binding cacheGuestBinding, release cacheJobRelease, request grant.SetupRequest, expires time.Time) {
	window := cacheSessionRenewWindow
	if session.config.renewWindow > 0 {
		window = session.config.renewWindow
	}
	backoff := time.Duration(0)
	for wait := time.Until(expires) - window; ; {
		select {
		case <-session.ctx.Done():
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(session.ctx, cacheSessionRenewBudget)
		handler, next, reason := session.issue(ctx, binding, release, request)
		cancel()
		session.config.log(map[string]any{"event": "renew", "vm_id": session.config.id, "ok": reason == "", "reason": reason})
		if reason == "" && !next.After(expires) {
			handler.Close() // capped by the binding: renewal cannot extend the grant
			return
		}
		if reason == "" {
			session.mu.Lock()
			if session.done() {
				session.mu.Unlock()
				handler.Close()
				return
			}
			previous := session.current
			session.current, session.expires = handler, next
			session.mu.Unlock()
			time.AfterFunc(time.Until(expires)+time.Second, previous.Close)
			expires, wait, backoff = next, time.Until(next)-window, 0
			continue
		}
		backoff = min(max(2*backoff, cacheSessionRenewBackoff), time.Minute)
		if reason == "github_job_not_in_progress" || !time.Now().Add(backoff).Before(expires) {
			return
		}
		wait = backoff
	}
}

type cacheSessionRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *cacheSessionRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *cacheSessionRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *cacheSessionRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (session *cacheSession) translate(w http.ResponseWriter, r *http.Request, refusal string) {
	start := time.Now()
	recorder := &cacheSessionRecorder{ResponseWriter: w}
	path := r.URL.Path // never log the query: it carries download tickets
	reason := ""
	defer func() {
		recovered := recover()
		session.config.log(map[string]any{"event": "http", "vm_id": session.config.id, "method": r.Method, "path": path, "status": recorder.status, "request_bytes": r.ContentLength, "response_bytes": recorder.bytes, "ms": time.Since(start).Milliseconds(), "reason": reason, "aborted": recovered != nil})
		if recovered != nil {
			panic(recovered)
		}
	}()
	if refusal != "" {
		reason = refusal
		http.Error(recorder, "cache unavailable", http.StatusForbidden)
		return
	}
	handler := session.handler()
	if handler == nil {
		reason = "no_grant"
		http.Error(recorder, "cache unavailable", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(recorder, r)
}
