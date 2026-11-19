package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/store"
)

type cacheServiceJob struct {
	Ref         string `json:"ref"`
	SHA         string `json:"sha"`
	Verified    bool   `json:"verified"`
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
	RepositoryID  int64            `json:"repository_id"`
	DefaultBranch string           `json:"default_branch"`
	Lease         string           `json:"lease"`
	Team          string           `json:"team"`
	TenantID      uint32           `json:"tenant_id"`
	Region        string           `json:"region"`
	Repository    string           `json:"repository"`
	VMID          string           `json:"vm_id"`
	MeshAddress   string           `json:"mesh_address"`
	RunnerID      int64            `json:"runner_id"`
	RunnerName    string           `json:"runner_name"`
	Job           *cacheServiceJob `json:"job"`
	Deadline      time.Time        `json:"deadline"`
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

// read shares bounded, non-redirecting token transport across the two methods.
func (central cacheCentralHTTPS) read(ctx context.Context, method string, query url.Values) ([]byte, error) {
	if !cacheHTTPSOrigin(central.origin) || central.client == nil || central.token == "" || strings.ContainsAny(central.token, " \t\r\n") {
		return nil, grant.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, cacheSessionSetupBudget)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(central.origin, "/")+"/api/method/runway.api."+method+"?"+query.Encode(), nil)
	if err != nil {
		return nil, grant.ErrInvalid
	}
	request.Header.Set("Authorization", "token "+central.token)
	client := *central.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, grant.ErrInvalid
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errCacheJobNotFound
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || !utf8.Valid(data) {
		return nil, grant.ErrInvalid
	}
	return data, nil
}

func (central cacheCentralHTTPS) lease(ctx context.Context, address netip.Addr) (cacheServiceLease, error) {
	var lease cacheServiceLease
	data, err := central.read(ctx, "cache_lease", url.Values{"address": {address.String()}})
	if err != nil {
		return lease, err
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
	if readCacheObject(normalized, []string{"lease", "team", "tenant_id", "region", "repository", "repository_id", "default_branch", "vm_id", "mesh_address", "runner_id", "runner_name", "job", "deadline"}) != nil {
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
	} else if readCacheObject(job, []string{"job_id", "request_id", "run_id", "run_attempt", "display_name", "workflow_ref", "event_name", "started", "ref", "sha", "verified"}) != nil {
		return lease, grant.ErrInvalid
	}
	return lease, nil
}

func (central cacheCentralHTTPS) bucket(ctx context.Context, team string, tenant int64, ca string) (store.GarageConfig, error) {
	var answer struct {
		Team      string `json:"team"`
		TenantID  int64  `json:"tenant_id"`
		Bucket    string `json:"bucket"`
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	query := url.Values{"tenant_id": {strconv.FormatInt(tenant, 10)}}
	if team != "" {
		query = url.Values{"team": {team}}
	}
	data, err := central.read(ctx, "cache_bucket", query)
	if err != nil {
		return store.GarageConfig{}, err
	}
	if readCacheObject(data, []string{"team", "tenant_id", "bucket", "endpoint", "region", "access_key", "secret_key"}) != nil || json.Unmarshal(data, &answer) != nil ||
		answer.Team == "" || team != "" && answer.Team != team || answer.TenantID != tenant || tenant <= 0 || tenant > 4294967295 ||
		!cacheHTTPSOrigin(answer.Endpoint) || answer.Bucket == "" || strings.ContainsAny(answer.Bucket, "/\\") || answer.Region == "" || answer.AccessKey == "" || answer.SecretKey == "" {
		return store.GarageConfig{}, grant.ErrInvalid
	}
	return store.GarageConfig{Endpoint: answer.Endpoint, Region: answer.Region, Bucket: answer.Bucket, AccessKey: answer.AccessKey, SecretKey: answer.SecretKey, CAPEM: ca}, nil
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
	regionID string
	central  cacheCentral
	address  netip.Addr
	lease    cacheServiceLease
}

func (a *cacheServiceAuthority) resolve(ctx context.Context, b cacheGuestBinding, _ int64) (cacheJobRelease, error) {
	j := a.lease.Job
	if j == nil || !j.Verified {
		return cacheJobRelease{}, errCacheJobNotFound
	}
	return cacheJobRelease{JobID: j.JobID, RequestID: j.RequestID, Stop: b.ExpiresAt, Confirmed: &cacheJobConfirmation{RunID: j.RunID, RunAttempt: j.RunAttempt, WorkflowRef: j.WorkflowRef, Source: "controller_verified"}}, nil
}
func (a *cacheServiceAuthority) authorize(ctx context.Context, b cacheGuestBinding, r cacheJobRelease, s grant.SetupRequest) (cacheJobGrant, error) {
	current, err := a.central.lease(ctx, a.address)
	if err != nil || !cacheSameLease(current, a.lease) || !time.Now().Before(current.Deadline) || current.Job == nil || !current.Job.Verified {
		return cacheJobGrant{}, errCacheJobMismatch
	}
	j, c := current.Job, s.Context
	for _, pair := range []struct {
		got  *string
		want string
	}{{c.Repository, current.Repository}, {c.RunID, strconv.FormatInt(j.RunID, 10)}, {c.RunAttempt, strconv.FormatInt(j.RunAttempt, 10)}, {c.Ref, j.Ref}, {c.SHA, j.SHA}, {c.EventName, j.EventName}, {c.RefType, "branch"}, {c.BaseRef, ""}, {c.HeadRef, ""}} {
		if pair.got == nil || *pair.got != pair.want {
			return cacheJobGrant{}, errCacheJobMismatch
		}
	}
	return cacheJobGrant{Authorization: grant.Authorization{TenantID: int64(b.TenantID), RegionID: a.regionID, JobID: j.JobID, RequestID: j.RequestID, RunID: j.RunID, RunAttempt: j.RunAttempt, Deadline: current.Deadline}, Policy: grant.RepositoryPolicy{Repository: current.Repository, RepositoryID: current.RepositoryID, DefaultBranch: current.DefaultBranch}}, nil
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
