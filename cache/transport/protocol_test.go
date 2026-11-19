package transport

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BreadGenie/runway-controller/cache/store/storetest"
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
