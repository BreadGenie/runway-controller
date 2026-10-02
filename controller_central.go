package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type controllerConfig struct {
	CentralURL              string   `json:"central_url"`
	CentralHost             string   `json:"central_host,omitempty"`
	CredentialFile          string   `json:"central_credential_file"`
	Pool                    string   `json:"pool"`
	OwnerID                 string   `json:"owner_id,omitempty"`
	TransportCommand        []string `json:"transport_command"`
	TransportTimeoutSeconds int      `json:"transport_timeout_seconds,omitempty"`
}

type pilotPool struct {
	ID             string `json:"id"`
	Region         string `json:"region"`
	Team           string `json:"team"`
	RepositoryURL  string `json:"repository_url"`
	ScaleSetID     int    `json:"scale_set_id"`
	MaxConcurrency int    `json:"max_concurrency"`
	MaxJobSeconds  int    `json:"max_job_seconds"`
	Enabled        bool   `json:"enabled"`
}

type pilotLease struct {
	ID            string    `json:"id"`
	Phase         string    `json:"phase"`
	RunnerName    string    `json:"runner_name"`
	RunnerID      int       `json:"runner_id"`
	JobID         string    `json:"job_id"`
	WorkflowRunID string    `json:"workflow_run_id"`
	VMID          string    `json:"vm_id"`
	VMState       string    `json:"vm_state"`
	MeshAddress   string    `json:"mesh_address"`
	Deadline      time.Time `json:"deadline"`
	LastError     string    `json:"last_error"`
	JITState      string    `json:"jit_state"`
	RequestID     int64     `json:"request_id"`
	AcquireState  string    `json:"acquire_state"`
}

type pilotSnapshot struct {
	Fence  int64        `json:"fence"`
	Pool   pilotPool    `json:"pool"`
	Leases []pilotLease `json:"leases"`
}

type leaseEvent struct {
	RunnerID      int    `json:"runner_id,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
	RequestID     int64  `json:"request_id,omitempty"`
	Result        string `json:"result,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Central is the sole authority for ownership, capacity, lease state and VMs.
// Methods include the immutable fence acquired for this process instance.
type pilotStore interface {
	Renew(context.Context, string) (pilotSnapshot, error)
	Reserve(context.Context, string) (pilotLease, error)
	Update(context.Context, string, string, leaseEvent) (pilotLease, error)
	EnsureVM(context.Context, string) (pilotLease, error)
	DeleteVM(context.Context, string) (pilotLease, error)
}

type centralRPC struct {
	baseURL, host, authorization, pool, owner string
	fence                                     int64
	claimedPool                               pilotPool
	http                                      *http.Client
}

// Every call carries the pool's API credentials, so TLS is required unless
// the endpoint is an exact loopback host used by local testing and tunnels.
// Names and addresses that only resemble loopback are not loopback.
func validateCentralEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("central_url must be an endpoint without credentials, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return errors.New("central_url must not target an unspecified address")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return errors.New("central_url requires HTTPS unless it is localhost, 127.0.0.1 or [::1]")
	default:
		return errors.New("central_url must be an HTTP(S) endpoint")
	}
}

func loadControllerConfig(path string) (controllerConfig, error) {
	var cfg controllerConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, errors.New("cannot read controller configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, errors.New("invalid controller configuration")
	}
	if err := validateCentralEndpoint(cfg.CentralURL); err != nil {
		return cfg, err
	}
	if cfg.Pool == "" || cfg.CredentialFile == "" || len(cfg.TransportCommand) == 0 || cfg.TransportCommand[0] == "" {
		return cfg, errors.New("controller requires pool, central_credential_file and transport_command")
	}
	if cfg.TransportTimeoutSeconds == 0 {
		cfg.TransportTimeoutSeconds = 90
	}
	if cfg.TransportTimeoutSeconds < 1 || cfg.TransportTimeoutSeconds > 600 {
		return cfg, errors.New("transport timeout must be between 1 and 600 seconds")
	}
	return cfg, nil
}

func newCentralRPC(cfg controllerConfig) (*centralRPC, error) {
	// Re-check here so no caller can attach the token to an endpoint the
	// configuration loader would have refused.
	if err := validateCentralEndpoint(cfg.CentralURL); err != nil {
		return nil, err
	}
	info, err := os.Lstat(cfg.CredentialFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Central credential must be a private regular file")
	}
	data, err := os.ReadFile(cfg.CredentialFile)
	if err != nil {
		return nil, errors.New("cannot read Central credential")
	}
	credential := strings.TrimSpace(string(data))
	if !strings.Contains(credential, ":") || strings.ContainsAny(credential, "\r\n \t") {
		return nil, errors.New("Central credential file must contain api_key:api_secret")
	}
	return &centralRPC{baseURL: strings.TrimRight(cfg.CentralURL, "/"), host: cfg.CentralHost, authorization: "token " + credential, pool: cfg.Pool, owner: cfg.OwnerID, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *centralRPC) call(ctx context.Context, method string, fields map[string]any, out any) error {
	payload := map[string]any{"pool": c.pool, "owner_id": c.owner}
	if method != "claim_pool" {
		payload["fence"] = c.fence
	}
	for k, v := range fields {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/method/central.api.runway."+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Central request")
	}
	req.Header.Set("Authorization", c.authorization)
	req.Header.Set("Content-Type", "application/json")
	if c.host != "" {
		req.Host = c.host
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Central %s transport failed; outcome may be uncertain", method)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Central %s rejected (HTTP %d)", method, response.StatusCode)
	}
	var envelope struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Central %s returned invalid JSON", method)
	}
	if len(envelope.Message) == 0 || string(envelope.Message) == "null" {
		return fmt.Errorf("Central %s returned no record", method)
	}
	if err := json.Unmarshal(envelope.Message, out); err != nil {
		return fmt.Errorf("Central %s record does not match controller contract", method)
	}
	return nil
}

func (c *centralRPC) Claim(ctx context.Context) (pilotSnapshot, error) {
	var result pilotSnapshot
	err := c.call(ctx, "claim_pool", nil, &result)
	if err == nil {
		c.fence = result.Fence
		c.claimedPool = result.Pool
	}
	return result, err
}
func (c *centralRPC) Renew(ctx context.Context, lastError string) (pilotSnapshot, error) {
	var result pilotSnapshot
	err := c.call(ctx, "renew_pool", map[string]any{"last_error": lastError}, &result)
	if err == nil && (result.Fence != c.fence || result.Pool.ID != c.claimedPool.ID || result.Pool.RepositoryURL != c.claimedPool.RepositoryURL || result.Pool.ScaleSetID != c.claimedPool.ScaleSetID || result.Pool.MaxConcurrency != c.claimedPool.MaxConcurrency || result.Pool.Region != c.claimedPool.Region || result.Pool.Team != c.claimedPool.Team) {
		err = errors.New("Central pool identity changed; restart controller before further side effects")
	}
	return result, err
}
func (c *centralRPC) Reserve(ctx context.Context, id string) (pilotLease, error) {
	var result pilotLease
	err := c.call(ctx, "reserve_lease", map[string]any{"lease_id": id}, &result)
	return result, err
}
func (c *centralRPC) Update(ctx context.Context, id, event string, fields leaseEvent) (pilotLease, error) {
	var result pilotLease
	values := map[string]any{"lease_id": id, "event": event, "runner_id": fields.RunnerID, "job_id": fields.JobID, "workflow_run_id": fields.WorkflowRunID, "request_id": fields.RequestID, "result": fields.Result, "error": fields.Error}
	err := c.call(ctx, "update_lease", values, &result)
	return result, err
}
func (c *centralRPC) EnsureVM(ctx context.Context, id string) (pilotLease, error) {
	var result pilotLease
	err := c.call(ctx, "ensure_vm", map[string]any{"lease_id": id}, &result)
	return result, err
}
func (c *centralRPC) DeleteVM(ctx context.Context, id string) (pilotLease, error) {
	var result pilotLease
	err := c.call(ctx, "request_delete", map[string]any{"lease_id": id}, &result)
	return result, err
}
