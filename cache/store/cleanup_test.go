package store

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCleanupIsolation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-48 * time.Hour)
	objects := map[string]time.Time{
		"managed/current/1/a":      old,
		"managed/stale/.owner":     old,
		"managed/stale/1/a":        old,
		"managed/fresh/.owner":     now,
		"managed/fresh/1/a":        old,
		"managed/unmarked-old/1/a": old,
		"managed/unmarked-new/1/a": now,
		"prototype/old/a":          old,
		"unrelated/a":              old,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/cache/")
		switch r.Method {
		case "HEAD":
			modified, ok := objects[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		case "DELETE":
			delete(objects, key)
			w.WriteHeader(204)
		case "GET":
			type item struct {
				Key          string
				LastModified string
			}
			type prefix struct{ Prefix string }
			result := struct {
				XMLName        xml.Name
				Contents       []item
				CommonPrefixes []prefix
			}{XMLName: xml.Name{Local: "ListBucketResult"}}
			seen := map[string]bool{}
			for key, modified := range objects {
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
					result.Contents = append(result.Contents, item{key, modified.Format(time.RFC3339)})
				}
			}
			xml.NewEncoder(w).Encode(result)
		}
	}))
	defer server.Close()
	target := newGarageStore(GarageConfig{Endpoint: server.URL, Region: "test", Bucket: "cache", AccessKey: "key", SecretKey: "secret"}, DefaultStorageBudget)
	defer target.close()
	n, err := target.cleanup(context.Background(), "managed/current/", now.Add(-24*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("cleanup: count=%d err=%v", n, err)
	}
	for _, key := range []string{"managed/current/1/a", "managed/fresh/.owner", "managed/fresh/1/a", "managed/unmarked-new/1/a", "prototype/old/a", "unrelated/a"} {
		if _, ok := objects[key]; !ok {
			t.Errorf("deleted protected key %s", key)
		}
	}
}

func TestOwnerRefreshBoundedAfterCacheEviction(t *testing.T) {
	var modified time.Time
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			if modified.IsZero() {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		case "PUT":
			if r.URL.Path != "/cache/managed/current/.owner" {
				t.Error("incorrect marker scope", r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			if _, err := time.Parse(time.RFC3339, string(body)); err != nil {
				t.Error("marker is not a timestamp", err)
			}
			modified = time.Now().UTC()
			puts++
		case "GET":
			w.Write([]byte("<ListBucketResult/>"))
		}
	}))
	defer server.Close()
	config := GarageConfig{Endpoint: server.URL, Region: "test", Bucket: "cache", AccessKey: "key", SecretKey: "secret"}
	managed := &garageStore{prefix: "managed/current/", budget: DefaultStorageBudget, maintenance: map[string]time.Time{}, maintenanceRunning: map[string]bool{}}
	managed.touch(config, "managed/current/1/1")
	managed.maintenanceWG.Wait()
	// Eviction must not cause another marker write within the refresh interval.
	managed.maintenanceMu.Lock()
	clear(managed.maintenance)
	managed.maintenanceMu.Unlock()
	managed.touch(config, "managed/current/1/2")
	managed.close()
	if puts != 1 {
		t.Fatal("unbounded marker writes", puts)
	}
}
