package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/pem"
	"github.com/BreadGenie/runway/cache/store"
)

// managedS3 is deliberately an in-memory S3 fake, not a signature verifier or
// Garage emulator. It checks credential/bucket routing and implements the exact
// metadata/list operations needed by production lazy maintenance.
type managedS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	modified map[string]time.Time
	bucket   string
}

func newManagedS3(t *testing.T, bucket string) (*managedS3, store.GarageConfig) {
	t.Helper()
	s := &managedS3{objects: map[string][]byte{}, modified: map[string]time.Time{}, bucket: bucket}
	server := httptest.NewTLSServer(s)
	t.Cleanup(server.Close)
	return s, store.GarageConfig{Endpoint: server.URL, Region: "garage", Bucket: bucket, AccessKey: "test-access-" + bucket, SecretKey: "test-secret-" + bucket, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
}
func (s *managedS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-access-"+s.bucket+"/") || !strings.HasPrefix(r.URL.Path, "/"+s.bucket+"/") && r.URL.Path != "/"+s.bucket {
		w.WriteHeader(403)
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"+s.bucket), "/")
	if r.Method == "PUT" {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		s.mu.Lock()
		s.objects[key] = raw
		s.modified[key] = time.Now().UTC().Truncate(time.Second)
		s.mu.Unlock()
		w.Header().Set("ETag", `"fake"`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case "HEAD", "GET":
		if r.URL.Query().Get("list-type") == "2" {
			type item struct {
				Key          string
				LastModified string
			}
			type prefix struct{ Prefix string }
			result := struct {
				XMLName        xml.Name
				Contents       []item
				CommonPrefixes []prefix
				IsTruncated    bool
			}{XMLName: xml.Name{Local: "ListBucketResult"}}
			seen := map[string]bool{}
			for key := range s.objects {
				root := r.URL.Query().Get("prefix")
				if !strings.HasPrefix(key, root) {
					continue
				}
				if r.URL.Query().Get("delimiter") == "/" {
					id, _, ok := strings.Cut(strings.TrimPrefix(key, root), "/")
					if ok && !seen[id] {
						result.CommonPrefixes = append(result.CommonPrefixes, prefix{root + id + "/"})
						seen[id] = true
					}
				} else {
					result.Contents = append(result.Contents, item{key, s.modified[key].Format(time.RFC3339)})
				}
			}
			w.Header().Set("Content-Type", "application/xml")
			xml.NewEncoder(w).Encode(result)
			return
		}
		raw, ok := s.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Header().Set("Last-Modified", s.modified[key].Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		if r.Method == "GET" {
			w.Write(raw)
		}
	case "DELETE":
		delete(s.objects, key)
		delete(s.modified, key)
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}
func (s *managedS3) age(prefix string, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			s.modified[key] = time.Now().Add(-age).UTC()
		}
	}
}
func (s *managedS3) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}
