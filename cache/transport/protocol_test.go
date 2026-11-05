package transport

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/store/storetest"
)

// storeFront runs a translator for bearer in front of a real store.
func storeFront(t *testing.T, service *storetest.Service, bearer string, edit func(*Config)) (*Handler, *httptest.Server) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(service.HTTP.Certificate())
	c := Config{Grant: Grant{Bearer: bearer, Expires: time.Now().Add(5 * time.Minute)}, Origin: service.Config.PublicOrigin, Roots: roots,
		Limits: Limits{RequestBytes: 4096, ChunkBytes: 1 << 20, ResponseBytes: 4096, HeaderBytes: 16384, URLBytes: 4096, ArchiveBytes: 1 << 20, Concurrent: 4, Timeout: 10 * time.Second}}
	if edit != nil {
		edit(&c)
	}
	h, err := New(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return h, front
}

// The host's real setup path: IssueSetup signs a grant from the frozen
// context, and the translator alone carries it to the store.
func TestCacheProtocolThroughTranslator(t *testing.T) {
	service := storetest.Start(t)
	const jobID = "afc2e663-0d50-409b-bf27-2254fcf2fe97"
	request, err := grant.DecodeSetup([]byte(`{"version":1,"job_id":"` + jobID + `","request_id":0,"nonce":"b2b00953-9291-4411-8536-348002a08a87","context":{"repository":"owner/repo","repository_id":"1","run_id":"12","run_attempt":"1","ref":"refs/heads/main","ref_type":"branch","sha":"` + strings.Repeat("a", 40) + `","event_name":"workflow_dispatch","base_ref":null,"head_ref":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	bearer, err := grant.IssueSetup(service.Signer.Key, service.Config.PublicOrigin,
		grant.Authorization{RegionID: service.Signer.Region, JobID: jobID, RunID: 12, RunAttempt: 1, Deadline: now.Add(time.Minute)},
		grant.RepositoryPolicy{RepositoryID: 1, Repository: "owner/repo", DefaultBranch: "main"}, request, grant.Clock{Earliest: now, Latest: now})
	if err != nil {
		t.Fatal(err)
	}
	inner := service.HTTP.Config.Handler
	service.HTTP.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := ""
		if strings.HasPrefix(r.URL.Path, "/_apis/artifactcache/") {
			want = "Bearer " + bearer
		}
		if r.Header.Get("Authorization") != want || r.Header.Get("Cookie") != "" {
			t.Error("translator did not replace guest credentials")
		}
		inner.ServeHTTP(w, r)
	})
	_, front := storeFront(t, service, bearer, nil)
	const lookup = "/_apis/artifactcache/cache?keys=key-a&version=v1"
	if code, _ := send(t, front, "GET", lookup, ""); code != 204 {
		t.Fatalf("initial miss: %d", code)
	}
	const reservation = `{"key":"key-a","version":"v1","cacheSize":3}`
	code, body := send(t, front, "POST", "/_apis/artifactcache/caches", reservation)
	var reserved struct {
		CacheID int64 `json:"cacheId"`
	}
	if code != 201 || json.Unmarshal([]byte(body), &reserved) != nil || reserved.CacheID <= 0 {
		t.Fatalf("reserve: %d", code)
	}
	// A duplicate writer is a protocol conflict, not an unavailable service.
	if code, _ := send(t, front, "POST", "/_apis/artifactcache/caches", reservation); code != 409 {
		t.Errorf("duplicate reserve lost protocol status: %d", code)
	}
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", reserved.CacheID)
	if code, _ := send(t, front, "PATCH", path, "abc"); code != 204 {
		t.Fatalf("upload: %d", code)
	}
	if code, _ := send(t, front, "POST", path, `{"size":3}`); code != 204 {
		t.Fatalf("finalize: %d", code)
	}
	code, body = send(t, front, "GET", lookup, "")
	var found struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &found) != nil || !strings.HasPrefix(found.ArchiveLocation, archiveRelayOrigin+"/download/") {
		t.Fatalf("lookup did not select the guest relay: %d %s", code, found.ArchiveLocation)
	}
	// The read ticket, not either bearer, authorizes the archive transfer.
	response, err := front.Client().Get(front.URL + strings.TrimPrefix(found.ArchiveLocation, archiveRelayOrigin))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(data) != "abc" {
		t.Fatalf("restore: status=%d err=%v", response.StatusCode, err)
	}
}

func TestDenialStatusSurvivesWithoutUpstreamSecrets(t *testing.T) {
	for _, status := range []int{401, 403, 404, 409, 410, 413, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			_, front := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Set-Cookie", "cache-only")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("cache-only github-secret"))
			}, nil)
			code, body := send(t, front, "GET", "/_apis/artifactcache/cache?keys=k&version=v", "")
			if code != status || strings.Contains(body, "cache-only") || strings.Contains(body, "github-secret") {
				t.Fatalf("unsafe or changed protocol failure: %d", code)
			}
		})
	}
}
