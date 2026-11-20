package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const cacheAtlasVMs = "/api/atlas/virtual-machines"

type cacheVMShape struct {
	ImageID string `json:"image_id"`
	CPU     int    `json:"cpu_millicores"`
	Memory  int    `json:"memory_mib"`
	Disk    int    `json:"disk_mib"`
}

type cacheFirewallRule struct {
	Protocol string   `json:"protocol"`
	Ports    string   `json:"ports,omitempty"`
	CIDRs    []string `json:"cidrs"`
}

type cacheFirewall struct {
	Enabled  bool                `json:"enabled"`
	Inbound  []cacheFirewallRule `json:"inbound"`
	Outbound []cacheFirewallRule `json:"outbound"`
}

type cacheVMIntent struct {
	cacheVMShape
	Firewall     cacheFirewall     `json:"firewall"`
	Tags         map[string]string `json:"tags"`
	IsPrivileged bool              `json:"is_privileged"`
}

type cacheAtlasVM struct {
	cacheVMShape
	ID        string            `json:"id"`
	Tenant    *int              `json:"tenant_id"`
	Tags      map[string]string `json:"tags"`
	CreatedAt int64             `json:"created_at"`
}

func (vm cacheAtlasVM) tagged(region string) bool {
	return validCacheGitHubPart(vm.ID) && vm.Tenant != nil && *vm.Tenant == 0 &&
		vm.Tags["role"] == "runway-cache" && vm.Tags["region"] == region
}

type cacheAtlasVMDetail struct {
	cacheAtlasVM
	State   string `json:"current_state"`
	Network struct {
		Mesh string `json:"mesh_ipv6"`
	} `json:"network"`
}

type cacheProvisionStatus struct {
	Enrolled     *bool      `json:"enrolled"`
	Mesh         string     `json:"mesh_address"`
	CA           string     `json:"ca_fingerprint"`
	TokenPending *bool      `json:"token_pending"`
	TokenExpires *time.Time `json:"token_expires_at"`
}

type cacheProvisionAPI struct {
	origin     string
	credential func() ([]byte, error)
	client     *http.Client
	atlas      bool
}

func cacheProvisionClient(ca string) (*http.Client, error) {
	var roots *x509.CertPool
	if ca != "" {
		data, err := base64.StdEncoding.Strict().DecodeString(ca)
		if err != nil {
			return nil, errCacheInvalid
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, errCacheInvalid
		}
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	}
	return &http.Client{
		Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

var errCacheVMAbsent = errors.New("atlas_vm_absent")

func (api cacheProvisionAPI) request(ctx context.Context, method, path string, body, result any) error {
	class, scheme := "runway", "token "
	if api.atlas {
		class, scheme = "atlas", "Bearer "
	}
	raw, err := api.credential()
	token := strings.TrimSpace(string(raw))
	if err != nil || len(raw) > 4096 || token == "" || strings.ContainsAny(token, " \t\r\n") ||
		(!api.atlas && strings.Count(token, ":") != 1) {
		return errors.New(class + "_credential")
	}
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errors.New(class + "_request")
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, api.origin+path, input)
	if err != nil {
		return errors.New(class + "_request")
	}
	req.Header.Set("Authorization", scheme+token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if api.atlas {
		req.Header.Set("X-Tenant-ID", "0")
	}
	resp, err := api.client.Do(req)
	if err != nil {
		return errors.New(class + "_transport")
	}
	defer resp.Body.Close()

	if api.atlas && resp.StatusCode == http.StatusNotFound && strings.HasPrefix(path, cacheAtlasVMs+"/") {
		var answer struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&answer) == nil && answer.Error.Code == "not_found" {
			return errCacheVMAbsent
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errors.New(class + "_authorization")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New(class + "_http")
	}
	if result == nil {
		return nil
	}
	const limit = 1 << 20
	raw, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit || json.Unmarshal(raw, result) != nil {
		return errors.New(class + "_response")
	}
	return nil
}

func (api cacheProvisionAPI) list(ctx context.Context, region, attempt string) ([]cacheAtlasVM, error) {
	const pageSize = 100
	tags := "role:runway-cache,region:" + region
	if attempt != "" {
		tags += ",attempt:" + attempt
	}
	var found []cacheAtlasVM
	seen := make(map[string]bool)
	for offset := 0; offset < 10000; offset += pageSize {
		query := url.Values{"tag": {tags}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(pageSize)}}
		var page struct {
			Items   []cacheAtlasVM `json:"items"`
			HasMore *bool          `json:"has_more"`
		}
		if err := api.request(ctx, http.MethodGet, cacheAtlasVMs+"?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.HasMore == nil || (*page.HasMore && len(page.Items) != pageSize) {
			return nil, errors.New("atlas_response")
		}
		for _, vm := range page.Items {
			if !vm.tagged(region) || attempt != "" && vm.Tags["attempt"] != attempt || seen[vm.ID] {
				return nil, errors.New("atlas_scope")
			}
			seen[vm.ID] = true
			found = append(found, vm)
		}
		if !*page.HasMore {
			return found, nil
		}
	}
	return nil, errors.New("atlas_response")
}

func (api cacheProvisionAPI) detail(ctx context.Context, id string) (cacheAtlasVMDetail, error) {
	var vm cacheAtlasVMDetail
	err := api.request(ctx, http.MethodGet, cacheAtlasVMs+"/"+url.PathEscape(id), nil, &vm)
	return vm, err
}

func (api cacheProvisionAPI) cacheStatus(ctx context.Context, region string) (cacheProvisionStatus, error) {
	var answer struct {
		Status cacheProvisionStatus `json:"message"`
	}
	query := url.Values{"region": {region}}
	err := api.request(ctx, http.MethodGet, "/api/method/runway.api.cache_status?"+query.Encode(), nil, &answer)
	if err == nil && (answer.Status.Enrolled == nil || answer.Status.TokenPending == nil) {
		err = errors.New("runway_response")
	}
	return answer.Status, err
}

func (api cacheProvisionAPI) issueEnrolment(ctx context.Context, region string) (string, error) {
	var answer struct {
		Token string `json:"message"`
	}
	err := api.request(ctx, http.MethodPost, "/api/method/runway.api.cache_issue_enrolment", map[string]string{"region": region}, &answer)
	if err == nil && !validCacheCredential(answer.Token) {
		err = errors.New("runway_response")
	}
	return answer.Token, err
}

func cacheVMUserData(c cacheReconcileRegion, token string) string {
	var env strings.Builder
	fmt.Fprintf(&env, "RUNWAY_URL='%s'\nRUNWAY_REGION='%s'\nRUNWAY_ENROLMENT_TOKEN='%s'\n", c.RunwayURL, c.Region, token)
	if c.SiteCA != "" {
		fmt.Fprintf(&env, "RUNWAY_SITE_CA_BASE64='%s'\n", c.SiteCA)
	}
	if c.StorageCA != "" {
		fmt.Fprintf(&env, "RUNWAY_STORAGE_CA_BASE64='%s'\n", c.StorageCA)
	}
	return "#cloud-config\nwrite_files:\n  - path: /etc/one-click/inputs.env\n    owner: root:root\n    permissions: '0600'\n    encoding: b64\n    content: " +
		base64.StdEncoding.EncodeToString([]byte(env.String())) + "\n"
}
