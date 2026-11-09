// Package store is the regional Actions cache data service: the v1 cache API
// over TLS, signed-grant authorization, disk staging and Garage storage with a
// durable journal. Garage is not the source of truth; a lost object is a miss.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/BreadGenie/runway/cache/grant"
)

// Config bounds one service. Garage selects standalone mode; Buckets selects
// managed mode. StorageBudget is optional.
type Config struct {
	PublicOrigin      string // fixed HTTPS origin; the grant audience
	Verifier          *grant.Verifier
	Clock             func(context.Context) (grant.Clock, error)
	StateDir          string
	Disk              DiskConfig
	Buckets           BucketProvider // nil selects standalone storage
	CleanupStaleAfter time.Duration  // zero defaults to 24 hours; must exceed two hours
	Garage            GarageConfig
	StorageBudget     *StorageBudget // nil selects DefaultStorageBudget
	QuotaBytes        int64          // per repository
	TotalBytes        int64
	MaxArchiveBytes   int64
}

// DiskConfig is the private staging directory and its physical byte budget.
type DiskConfig struct {
	Directory     string `json:"directory"`
	PhysicalBytes int64  `json:"physical_bytes"`
}

const (
	maxArchiveBytes  = 1 << 30
	maxTotalBytes    = 4 << 30
	maxPhysicalBytes = 64 << 30
	maxEntries       = 256
	maxTickets       = 1024
	maxChunkBytes    = 32 << 20
	ticketLifetime   = time.Minute
	idleLifetime     = 7 * 24 * time.Hour
)

func (config Config) validate() error {
	origin, err := url.Parse(config.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" ||
		config.Verifier == nil || config.Verifier.Audience() != config.PublicOrigin || config.Clock == nil || config.StateDir == "" ||
		config.Disk.Directory == "" || config.Disk.PhysicalBytes < config.MaxArchiveBytes || config.Disk.PhysicalBytes > maxPhysicalBytes ||
		config.QuotaBytes <= 0 || config.MaxArchiveBytes <= 0 || config.MaxArchiveBytes > config.QuotaBytes || config.MaxArchiveBytes > maxArchiveBytes ||
		config.TotalBytes < config.QuotaBytes || config.TotalBytes > maxTotalBytes {
		return errors.New("invalid cache store configuration")
	}
	if config.Buckets != nil {
		if config.CleanupStaleAfter != 0 && config.CleanupStaleAfter <= 2*time.Hour {
			return errors.New("cleanup threshold must exceed two hours")
		}
	} else if err := config.Garage.validate(); err != nil {
		return err
	}
	if config.StorageBudget != nil {
		return config.StorageBudget.Validate()
	}
	return nil
}

// Scope is the verified grant: never guest request fields. Tenant is the
// signed Central team from the grant; zero is standalone.
type Scope struct {
	Subject, Repository string
	WriteRef            string
	ReadRefs            []string
	ExpiresAt           time.Time
	Tenant              int64
}

type entry struct {
	id, size                               int64
	repository, ref, key, version, subject string
	created, accessed                      time.Time
	published, hidden, finalizing          bool
	objectKey                              string
	digest                                 [32]byte
	spool                                  *spool // upload staging; released after publication
	pins                                   int    // requests using the entry outside the lock
}

// uploadDeadline bounds an unpublished reservation: a minute plus time to
// receive the bytes and then hash and store them at the budget's minimum rate.
func (s *Server) uploadDeadline(e *entry) time.Time {
	return e.created.Add(time.Minute + 2*s.budget.operation(e.size))
}

type readTicket struct {
	entryID int64
	tenant  int64 // signed tenant bound at mint; checked at redeem
	expires time.Time
}

// Server holds the index under mu. Garage I/O, hashing and streaming happen
// outside mu on pinned entries, so a slow reader never blocks other requests.
type Server struct {
	config Config
	origin *url.URL
	budget StorageBudget
	store  *garageStore
	pool   *spoolPool

	mu        sync.Mutex
	clock     grant.Clock // latest observation, and when it was taken
	clockAt   time.Time
	namespace string
	nextID    int64
	entries   map[int64]*entry
	tickets   map[string]readTicket
	stateDir  *os.File
	stateLock *os.File
	stateErr  error // a failed journal write fences the service until restart
	closed    bool

	sweep sweeper
}

// New opens the journal, wipes stale staging files, verifies every indexed
// object against Garage within the recovery budget and starts the sweeper.
func New(config Config) (*Server, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	origin, _ := url.Parse(config.PublicOrigin)
	budget := DefaultStorageBudget
	if config.StorageBudget != nil {
		budget = *config.StorageBudget
	}
	s := &Server{config: config, origin: origin, budget: budget, entries: make(map[int64]*entry), tickets: make(map[string]readTicket)}
	if _, err := s.observe(context.Background()); err != nil {
		return nil, errors.New("cache time observation unavailable")
	}
	if config.Buckets != nil {
		s.store = &garageStore{provider: config.Buckets, budget: budget, staleAfter: config.CleanupStaleAfter, maintenance: make(map[string]time.Time), maintenanceRunning: make(map[string]bool)}
	} else {
		s.store = newGarageStore(config.Garage, budget)
	}
	pool, err := openSpoolPool(config.Disk.Directory, config.Disk.PhysicalBytes)
	if err != nil {
		s.store.close()
		return nil, err
	}
	s.pool = pool
	if err := s.openState(); err != nil {
		s.pool.close()
		s.store.close()
		return nil, err
	}
	s.sweep.start(s)
	return s, nil
}

// Close stops the sweeper and persists the index; objects stay in Garage.
func (s *Server) Close(context.Context) error {
	s.sweep.stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.stateErr
	}
	s.closed = true
	err := s.persist()
	err = errors.Join(err, s.stateLock.Close(), s.stateDir.Close(), s.pool.close())
	s.store.close()
	return err
}

// observe reads the host clock; requests and expiry use its latest bound.
func (s *Server) observe(ctx context.Context) (grant.Clock, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	clock, err := s.config.Clock(ctx)
	if err != nil || !clock.Valid() || ctx.Err() != nil {
		return grant.Clock{}, errors.New("cache time refused")
	}
	s.mu.Lock()
	s.clock, s.clockAt = clock, time.Now()
	s.mu.Unlock()
	return clock, nil
}

// now is the latest observed clock bound advanced by monotonic time. Caller holds mu.
func (s *Server) now() time.Time { return s.clock.Latest.Add(time.Since(s.clockAt)) }

func fail(w http.ResponseWriter, status int) { http.Error(w, http.StatusText(status), status) }

func validKey(key string) bool {
	// The pinned JavaScript client counts UTF-16 code units, not UTF-8 bytes.
	return key != "" && len(utf16.Encode([]rune(key))) <= 512 && !strings.Contains(key, ",")
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		fail(w, http.StatusBadRequest)
		return false
	}
	return true
}

// ServeHTTP serves only the configured TLS origin. Forwarded headers never
// select an origin or authorize a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || r.Host != s.origin.Host {
		fail(w, http.StatusBadRequest)
		return
	}
	clock, err := s.observe(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	unavailable := s.closed || s.stateErr != nil
	if !unavailable {
		s.collect()
	}
	s.mu.Unlock()
	if unavailable {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/download/") {
		s.download(w, r, clock)
		return
	}
	scope, ok := s.authorize(r, clock)
	if !ok {
		fail(w, http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/_apis/artifactcache/cache" && r.Method == http.MethodGet:
		s.lookup(w, r, scope)
	case r.URL.Path == "/_apis/artifactcache/caches" && r.Method == http.MethodPost:
		s.reserve(w, r, scope)
	case strings.HasPrefix(r.URL.Path, "/_apis/artifactcache/caches/") && r.Method == http.MethodPatch:
		s.uploadChunk(w, r, scope)
	case strings.HasPrefix(r.URL.Path, "/_apis/artifactcache/caches/") && r.Method == http.MethodPost:
		s.finalize(w, r, scope)
	default:
		fail(w, http.StatusNotFound)
	}
}

// scopeTenant is the persisted tenant component of every repository scope key;
// entries already saved on blr-1 are keyed with it.
const scopeTenant = "standalone"

func (s *Server) authorize(r *http.Request, clock grant.Clock) (Scope, bool) {
	if len(r.Header.Values("Authorization")) != 1 {
		return Scope{}, false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return Scope{}, false
	}
	claims, err := s.config.Verifier.Verify(token, clock)
	if err != nil {
		return Scope{}, false
	}
	// Canonical tuples prevent separator collisions and keep the stable numeric
	// repository identity independent of mutable owner/repository names.
	repository, _ := json.Marshal([]string{scopeTenant, claims.RegionID, strconv.FormatInt(claims.RepositoryID, 10)})
	if claims.TenantID != 0 {
		if s.config.Buckets == nil {
			return Scope{}, false
		}
		if _, err := s.config.Buckets.Bucket(r.Context(), claims.TenantID); err != nil {
			return Scope{}, false
		}
		repository, _ = json.Marshal([]string{"managed", strconv.FormatInt(claims.TenantID, 10), strconv.FormatInt(claims.RepositoryID, 10)})
	} else if s.config.Buckets != nil {
		return Scope{}, false
	}
	subject, _ := json.Marshal([]string{claims.JobID, strconv.FormatInt(claims.RequestID, 10), strconv.FormatInt(claims.RunID, 10), strconv.FormatInt(claims.RunAttempt, 10)})
	return Scope{Subject: string(subject), Repository: string(repository), ReadRefs: claims.ReadRefs, WriteRef: claims.WriteRef, ExpiresAt: time.Unix(claims.ExpiresAt, 0), Tenant: claims.TenantID}, true
}

// collect expires tickets and hides abandoned or idle entries; the sweeper
// deletes hidden ones. Caller holds mu.
func (s *Server) collect() {
	now := s.now()
	for token, ticket := range s.tickets {
		if !now.Before(ticket.expires) {
			delete(s.tickets, token)
		}
	}
	hidden := false
	for _, e := range s.entries {
		if e.pins == 0 && !e.hidden && (!e.published && !now.Before(s.uploadDeadline(e)) || e.published && now.Sub(e.accessed) >= idleLifetime) {
			e.hidden = true
		}
		hidden = hidden || e.hidden
	}
	if hidden {
		s.sweep.wake()
	}
}

func (s *Server) hasReaders(id int64) bool {
	for _, ticket := range s.tickets {
		if ticket.entryID == id {
			return true
		}
	}
	return false
}

// usage charges every entry until the sweeper deletes it, hidden ones included.
func (s *Server) usage(repository string) (repo, total int64) {
	for _, e := range s.entries {
		total += e.size
		if e.repository == repository {
			repo += e.size
		}
	}
	return
}

// evict hides least-recently used published entries until size fits. It
// reports whether the space is free now (true) or after a sweep (false).
func (s *Server) evict(repository string, size int64) (fits, possible bool) {
	for {
		repo, total := s.usage(repository)
		var visibleRepo, visibleTotal int64
		for _, e := range s.entries {
			if !e.hidden {
				visibleTotal += e.size
				if e.repository == repository {
					visibleRepo += e.size
				}
			}
		}
		if repo+size <= s.config.QuotaBytes && total+size <= s.config.TotalBytes {
			return true, true
		}
		if visibleRepo+size <= s.config.QuotaBytes && visibleTotal+size <= s.config.TotalBytes {
			return false, true
		}
		var oldest *entry
		for _, e := range s.entries {
			if e.hidden || !e.published || e.pins > 0 || s.hasReaders(e.id) || visibleRepo+size > s.config.QuotaBytes && e.repository != repository {
				continue
			}
			if oldest == nil || e.accessed.Before(oldest.accessed) || e.accessed.Equal(oldest.accessed) && e.id < oldest.id {
				oldest = e
			}
		}
		if oldest == nil {
			return false, false
		}
		oldest.hidden = true
	}
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request, scope Scope) {
	if scope.WriteRef == "" {
		fail(w, http.StatusForbidden)
		return
	}
	var input struct {
		Key     string `json:"key"`
		Version string `json:"version"`
		Size    int64  `json:"cacheSize"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validKey(input.Key) || input.Version == "" || len(input.Version) > 128 || input.Size <= 0 || input.Size > s.config.MaxArchiveBytes {
		fail(w, http.StatusBadRequest)
		return
	}
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		for _, e := range s.entries {
			if !e.hidden && e.repository == scope.Repository && e.ref == scope.WriteRef && e.key == input.Key && e.version == input.Version {
				s.mu.Unlock()
				fail(w, http.StatusConflict)
				return
			}
		}
		fits, possible := s.evict(scope.Repository, input.Size)
		if fits && len(s.entries) < maxEntries {
			s.reserveLocked(w, r, scope, input.Key, input.Version, input.Size)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		// Eviction frees space only once the sweeper deletes the objects. Run
		// one sweep now (it honours the outage backoff), then retry once.
		if attempt > 0 || !possible && len(s.entries) < maxEntries {
			fail(w, http.StatusConflict)
			return
		}
		s.sweep.run(r.Context())
	}
}

func (s *Server) reserveLocked(w http.ResponseWriter, r *http.Request, scope Scope, key, version string, size int64) {
	staged, err := s.pool.create(size)
	if err != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	s.nextID++
	now := s.now()
	e := &entry{id: s.nextID, size: size, repository: scope.Repository, ref: scope.WriteRef, subject: scope.Subject, key: key, version: version, created: now, accessed: now, spool: staged}
	s.entries[e.id] = e
	if s.persist() != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]int64{"cacheId": e.id})
}

// ownUpload returns the caller's live upload. Caller holds mu.
func (s *Server) ownUpload(w http.ResponseWriter, r *http.Request, scope Scope) *entry {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/_apis/artifactcache/caches/"), 10, 64)
	e := s.entries[id]
	if err != nil || e == nil || e.hidden {
		fail(w, http.StatusGone)
		return nil
	}
	if e.repository != scope.Repository || e.subject != scope.Subject || e.ref != scope.WriteRef {
		fail(w, http.StatusForbidden)
		return nil
	}
	return e
}

func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request, scope Scope) {
	var start, end int64
	text := r.Header.Get("Content-Range")
	n, err := fmt.Sscanf(text, "bytes %d-%d/*", &start, &end)
	if err != nil || n != 2 || text != fmt.Sprintf("bytes %d-%d/*", start, end) || start < 0 || end < start || end-start >= maxChunkBytes {
		fail(w, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	e := s.ownUpload(w, r, scope)
	if e != nil && (e.published || e.finalizing) {
		fail(w, http.StatusConflict)
		e = nil
	} else if e != nil && end >= e.size {
		fail(w, http.StatusBadRequest)
		e = nil
	}
	if e != nil {
		e.pins++
	}
	s.mu.Unlock()
	if e == nil {
		return
	}
	defer s.unpin(e)
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, end-start+2))
	if err != nil || int64(len(data)) != end-start+1 {
		fail(w, http.StatusBadRequest)
		return
	}
	if err := e.spool.writeChunk(r.Context(), start, data); err != nil {
		fail(w, http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unpin(e *entry) {
	s.mu.Lock()
	e.pins--
	s.mu.Unlock()
}

// finalize seals the staged bytes, stores them in Garage and publishes. One
// authorization check precedes the PUT and one precedes publication.
func (s *Server) finalize(w http.ResponseWriter, r *http.Request, scope Scope) {
	var input struct {
		Size int64 `json:"size"`
	}
	if !decode(w, r, &input) {
		return
	}
	s.mu.Lock()
	e := s.ownUpload(w, r, scope)
	switch {
	case e == nil:
	case input.Size != e.size:
		fail(w, http.StatusConflict)
		e = nil
	case e.published:
		w.WriteHeader(http.StatusNoContent) // a retried finalize after success
		e = nil
	case e.finalizing:
		fail(w, http.StatusConflict)
		e = nil
	default:
		e.finalizing = true
		e.pins++
	}
	s.mu.Unlock()
	if e == nil {
		return
	}
	status := s.publish(r.Context(), e, scope)
	s.mu.Lock()
	e.finalizing = false
	e.pins--
	s.mu.Unlock()
	if status != http.StatusNoContent {
		fail(w, status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// publish runs finalize's storage work on a pinned entry and returns the status.
func (s *Server) publish(ctx context.Context, e *entry, scope Scope) int {
	digest, err := e.spool.seal(ctx)
	if err != nil {
		return http.StatusConflict
	}
	if _, err := s.observe(ctx); err != nil {
		return http.StatusServiceUnavailable
	}
	s.mu.Lock()
	if e.hidden || !s.now().Before(scope.ExpiresAt) || !s.now().Before(s.uploadDeadline(e)) {
		e.hidden = true
		s.mu.Unlock()
		return http.StatusUnauthorized
	}
	// Journal the object intent before the PUT so a crash leaves it deletable.
	e.digest, e.objectKey = digest, s.objectKey(e.repository, e.id)
	persisted := s.persist() == nil
	s.mu.Unlock()
	if !persisted {
		return http.StatusServiceUnavailable
	}
	putCtx, cancel := context.WithTimeout(ctx, s.budget.operation(e.size))
	err = s.store.put(putCtx, e.objectKey, e.spool.file, e.size)
	cancel()
	if _, observeErr := s.observe(ctx); observeErr != nil {
		err = observeErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		e.hidden = true
		return http.StatusServiceUnavailable
	}
	if e.hidden || !s.now().Before(scope.ExpiresAt) {
		e.hidden = true
		return http.StatusUnauthorized
	}
	e.published = true
	if s.persist() != nil {
		return http.StatusServiceUnavailable
	}
	// The published bytes live in Garage; release the upload's physical charge.
	s.pool.remove(e.spool)
	e.spool = nil
	return http.StatusNoContent
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request, scope Scope) {
	rawKeys := r.URL.Query().Get("keys")
	literalKeys := rawKeys
	// actions/cache v4 applies encodeURIComponent before its URL builder
	// escapes the query again. Accept its encoded list delimiter as well as
	// the ordinary v5+ comma. Cache keys themselves cannot contain commas.
	rawKeys = strings.ReplaceAll(strings.ReplaceAll(rawKeys, "%2C", ","), "%2c", ",")
	keys := strings.Split(rawKeys, ",")
	version := r.URL.Query().Get("version")
	if len(keys) > 10 || version == "" || len(version) > 128 {
		fail(w, http.StatusBadRequest)
		return
	}
	for _, key := range keys {
		if !validKey(key) {
			fail(w, http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ref := range scope.ReadRefs {
		candidates := keys
		// A literal percent sequence is also a valid cache key. Prefer that
		// spelling before interpreting the legacy escaped list delimiter.
		if literalKeys != rawKeys {
			candidates = append([]string{literalKeys}, keys...)
		}
		for _, key := range candidates {
			var match *entry
			for _, e := range s.entries {
				if e.hidden || !e.published || e.repository != scope.Repository || e.ref != ref || e.version != version || !strings.HasPrefix(e.key, key) {
					continue
				}
				// An exact key wins; otherwise the newest prefix match.
				if match == nil || e.key == key && match.key != key || (e.key == key) == (match.key == key) && e.id > match.id {
					match = e
				}
			}
			if match != nil {
				s.issueTicket(w, scope, ref, match)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// issueTicket persists a one-minute read ticket (capped at the grant) for e,
// bound to the grant's signed tenant. Caller holds mu.
func (s *Server) issueTicket(w http.ResponseWriter, scope Scope, ref string, e *entry) {
	if len(s.tickets) >= maxTickets {
		fail(w, http.StatusTooManyRequests)
		return
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		fail(w, http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(random)
	now := s.now()
	e.accessed = now
	expires := now.Add(ticketLifetime)
	if scope.ExpiresAt.Before(expires) {
		expires = scope.ExpiresAt
	}
	s.tickets[ticketKey(token)] = readTicket{e.id, scope.Tenant, expires}
	if s.persist() != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cacheKey": e.key, "scope": ref, "archiveLocation": fmt.Sprintf("%s/download/%d?ticket=%s", s.config.PublicOrigin, e.id, token)})
}

// repositoryTenant returns the tenant routed by a repository scope key:
// the managed tuple's tenant, or zero for standalone. It mirrors objectKey's
// strict canonical-tuple check so a ticket can never cover another tenant's
// object, whatever entry ID the download URL names.
func repositoryTenant(repository string) (int64, bool) {
	var parts []string
	if json.Unmarshal([]byte(repository), &parts) != nil || len(parts) != 3 {
		return 0, false
	}
	canonical, _ := json.Marshal(parts)
	if string(canonical) != repository {
		return 0, false
	}
	switch parts[0] {
	case "managed":
		tenant, err := strconv.ParseInt(parts[1], 10, 64)
		repo, repoErr := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || repoErr != nil || tenant <= 0 || tenant > 4294967295 || repo <= 0 || parts[1] != strconv.FormatInt(tenant, 10) || parts[2] != strconv.FormatInt(repo, 10) {
			return 0, false
		}
		return tenant, true
	case "standalone":
		return 0, true
	default:
		return 0, false
	}
}

// download checks the bearer grant and the ticket before fetching, stages the
// object while hashing it, and serves bytes only after the size and digest
// match the journal. The ticket must carry the redeeming grant's signed
// tenant, the entry's routed tenant must match the ticket, and the entry must
// belong to the redeeming grant's repository, as lookups already require:
// cross-tenant and cross-repository replays fail closed with the same opaque
// refusal as any other invalid ticket, even if the translator's session
// binding regresses.
func (s *Server) download(w http.ResponseWriter, r *http.Request, clock grant.Clock) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, http.StatusMethodNotAllowed)
		return
	}
	scope, ok := s.authorize(r, clock)
	if !ok {
		fail(w, http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/download/"), 10, 64)
	s.mu.Lock()
	ticket, ok := s.tickets[ticketKey(r.URL.Query().Get("ticket"))]
	e := s.entries[id]
	entryTenant, tenantOK := int64(0), true
	if e != nil {
		entryTenant, tenantOK = repositoryTenant(e.repository)
	}
	if err != nil || !ok || ticket.entryID != id || e == nil || !e.published || e.hidden || !s.now().Before(ticket.expires) || !tenantOK || ticket.tenant != scope.Tenant || entryTenant != ticket.tenant || e.repository != scope.Repository {
		s.mu.Unlock()
		fail(w, http.StatusNotFound)
		return
	}
	e.pins++
	s.mu.Unlock()
	defer s.unpin(e)
	staged, err := s.pool.create(e.size)
	if err != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	defer s.pool.remove(staged)
	ctx, cancel := context.WithTimeout(r.Context(), s.budget.operation(e.size))
	err = s.store.get(ctx, e.objectKey, staged, e.digest)
	cancel()
	if err != nil {
		if r.Context().Err() == nil && lostArchive(err) {
			s.mu.Lock()
			e.hidden = true // a lost or different object becomes a miss
			s.mu.Unlock()
		}
		fail(w, http.StatusServiceUnavailable)
		return
	}
	http.ServeContent(w, r, "cache", e.created, io.NewSectionReader(staged.file, 0, e.size))
}

func (s *Server) objectKey(repository string, id int64) string {
	if s.config.Buckets != nil {
		var parts []string
		if json.Unmarshal([]byte(repository), &parts) != nil || len(parts) != 3 || parts[0] != "managed" {
			return ""
		}
		tenant, err := strconv.ParseInt(parts[1], 10, 64)
		repo, repoErr := strconv.ParseInt(parts[2], 10, 64)
		canonical, _ := json.Marshal(parts)
		if err != nil || repoErr != nil || tenant <= 0 || tenant > 4294967295 || repo <= 0 || parts[1] != strconv.FormatInt(tenant, 10) || parts[2] != strconv.FormatInt(repo, 10) || string(canonical) != repository {
			return ""
		}
		return s.namespace + parts[1] + "/" + strconv.FormatInt(id, 10)
	}
	return s.namespace + strconv.FormatInt(id, 10)
}
