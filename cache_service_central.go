package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BreadGenie/runway/cache/grant"
)

type cacheServiceJob struct {
	JobID       string `json:"job_id"`
	RequestID   int64  `json:"request_id"`
	RunID       int64  `json:"run_id"`
	RunAttempt  int64  `json:"run_attempt"`
	DisplayName string `json:"display_name"`
	WorkflowRef string `json:"workflow_ref"`
	EventName   string `json:"event_name"`
	Started     bool   `json:"started"`
}
type cacheServiceLease struct {
	Lease       string           `json:"lease"`
	Team        string           `json:"team"`
	TenantID    uint32           `json:"tenant_id"`
	Region      string           `json:"region"`
	Repository  string           `json:"repository"`
	VMID        string           `json:"vm_id"`
	MeshAddress string           `json:"mesh_address"`
	RunnerID    int64            `json:"runner_id"`
	RunnerName  string           `json:"runner_name"`
	Job         *cacheServiceJob `json:"job"`
	Deadline    time.Time        `json:"deadline"`
}

type cacheCentral interface {
	lease(context.Context, netip.Addr) (cacheServiceLease, error)
}
type cacheCentralHTTPS struct {
	origin, token string
	client        *http.Client
}

func cacheHTTPSOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

func (central cacheCentralHTTPS) lease(ctx context.Context, address netip.Addr) (cacheServiceLease, error) {
	var lease cacheServiceLease
	if !cacheHTTPSOrigin(central.origin) || central.token == "" || strings.ContainsAny(central.token, " \t\r\n") {
		return lease, grant.ErrInvalid
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(central.origin, "/")+"/api/method/central.runway.cache_lease?address="+url.QueryEscape(address.String()), nil)
	if err != nil {
		return lease, err
	}
	request.Header.Set("Authorization", "token "+central.token)
	// Never forward the regional credential through a redirect.
	client := *central.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return lease, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return lease, errCacheJobNotFound
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || !utf8.Valid(data) {
		return lease, grant.ErrInvalid
	}
	// job is the contract's only nullable field. Validate its presence separately.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return lease, grant.ErrInvalid
	}
	job, ok := fields["job"]
	if !ok {
		return lease, grant.ErrInvalid
	}
	if string(job) == "null" {
		fields["job"] = json.RawMessage(`{}`)
	}
	normalized, _ := json.Marshal(fields)
	if readCacheObject(normalized, []string{"lease", "team", "tenant_id", "region", "repository", "vm_id", "mesh_address", "runner_id", "runner_name", "job", "deadline"}) != nil {
		return lease, grant.ErrInvalid
	}
	// A token decoder handles arbitrary whitespace around nullable job values.
	if err := cacheLeaseExact(data); err != nil {
		return lease, err
	}
	if json.Unmarshal(normalized, &lease) != nil {
		return lease, grant.ErrInvalid
	}
	if string(job) == "null" {
		lease.Job = nil
	} else if readCacheObject(job, []string{"job_id", "request_id", "run_id", "run_attempt", "display_name", "workflow_ref", "event_name", "started"}) != nil {
		return lease, grant.ErrInvalid
	}
	return lease, nil
}

// Exact top-level decoding, allowing only job to be null.
func cacheLeaseExact(data []byte) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return grant.ErrInvalid
	}
	keys := map[string]bool{}
	for d.More() {
		k, err := d.Token()
		name, ok := k.(string)
		if err != nil || !ok || keys[name] {
			return grant.ErrInvalid
		}
		keys[name] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return grant.ErrInvalid
		}
	}
	if _, err = d.Token(); err != nil {
		return grant.ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return grant.ErrInvalid
	}
	return nil
}

type cacheServiceAuthority struct {
	github  githubCacheJobAuthority
	central cacheCentral
	address netip.Addr
	lease   cacheServiceLease
}

func (a *cacheServiceAuthority) resolve(ctx context.Context, b cacheGuestBinding, _ int64) (cacheJobRelease, error) {
	j := a.lease.Job
	if j == nil {
		return cacheJobRelease{}, errCacheJobNotFound
	}
	attempt := j.RunAttempt
	confirmed, job, err := a.github.confirmREST(ctx, b, j.RunID, &attempt)
	if err != nil {
		return cacheJobRelease{}, err
	}
	if job.Name != j.DisplayName || attempt != j.RunAttempt {
		return cacheJobRelease{}, errCacheJobMismatch
	}
	confirmed.WorkflowRef = j.WorkflowRef
	return cacheJobRelease{JobID: j.JobID, RequestID: j.RequestID, Stop: b.ExpiresAt, Confirmed: &confirmed}, nil
}
func (a *cacheServiceAuthority) authorize(ctx context.Context, b cacheGuestBinding, r cacheJobRelease, s grant.SetupRequest) (cacheJobGrant, error) {
	current, err := a.central.lease(ctx, a.address)
	if err != nil || !cacheSameLease(current, a.lease) || !time.Now().Before(current.Deadline) {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	if s.Context.EventName == nil || *s.Context.EventName != a.lease.Job.EventName {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	return a.github.authorize(ctx, b, r, s)
}
func cacheSameLease(a, b cacheServiceLease) bool {
	if a.Job == nil || b.Job == nil {
		return false
	}
	aj, bj := *a.Job, *b.Job
	aj.Started = false
	bj.Started = false
	a.Job = nil
	b.Job = nil
	return a == b && aj == bj
}

// Service confirmations always use REST, including renewal; no assignment files.
type cacheNoAssignments struct{}

func (cacheNoAssignments) assignment(int64) (cacheAssignment, error) {
	return cacheAssignment{}, errCacheJobNotFound
}
func (cacheNoAssignments) job(string) (cacheJobRecord, error) {
	return cacheJobRecord{}, errCacheJobNotFound
}
func (cacheNoAssignments) jobs(int64) ([]cacheJobRecord, error) { return nil, errCacheJobNotFound }
