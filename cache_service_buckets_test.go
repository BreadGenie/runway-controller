package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/store"
	"github.com/BreadGenie/runway/cache/store/storetest"
)

func TestCentralManagedBuckets(t *testing.T) {
	var mu sync.Mutex
	access, fail, calls := "first", false, 0
	configs := map[string]store.GarageConfig{}
	garages := map[string]*storetest.Garage{}
	for _, team := range []string{"a", "b"} {
		g := storetest.NewGarage(t)
		g.Objects["/cache/managed/regional/old/1"] = []byte("orphan")
		g.Objects["/cache/foreign/1"] = []byte("foreign")
		tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			key := access
			mu.Unlock()
			if !strings.Contains(r.Header.Get("Authorization"), "Credential="+key+"/") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			g.Server.Config.Handler.ServeHTTP(w, r)
		}))
		t.Cleanup(tls.Close)
		c := g.Config()
		c.Endpoint = tls.URL
		c.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tls.Certificate().Raw}))
		configs[team], garages[team] = c, g
	}
	central := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Header.Get("Authorization") != "token regional" || r.URL.Path != "/api/method/central.runway.cache_bucket" {
			t.Error("Central contract")
			w.WriteHeader(403)
			return
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		team := map[string]string{"1": "a", "2": "b", "3": "c"}[r.URL.Query().Get("tenant_id")]
		c, ok := configs[team]
		if !ok {
			w.WriteHeader(503)
			return
		}
		id := 1
		if team == "b" {
			id = 2
		}
		if team == "c" {
			id = 3
		}
		json.NewEncoder(w).Encode(map[string]any{"team": team, "tenant_id": id, "bucket": c.Bucket, "endpoint": c.Endpoint, "region": c.Region, "access_key": access, "secret_key": c.SecretKey})
	}))
	defer central.Close()
	p := &cacheBucketProvider{central: cacheCentralHTTPS{origin: central.URL, token: "regional", client: central.Client()}, ca: configs["a"].CAPEM + configs["b"].CAPEM}
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = p })
	// Central can enable a new team after startup without service configuration.
	mu.Lock()
	configs["c"] = configs["a"]
	mu.Unlock()
	c := service.Signer.Grant(t, storetest.Job{TenantID: 3, Write: "refs/heads/main"})
	service.Save(t, c, "new-team", "team-c")
	if _, data := service.Restore(t, c, "new-team"); data != "team-c" {
		t.Fatal("new team unavailable", data)
	}

	a := service.Signer.Grant(t, storetest.Job{TenantID: 1, Write: "refs/heads/main"})
	b := service.Signer.Grant(t, storetest.Job{TenantID: 2, Write: "refs/heads/main"})
	service.Save(t, a, "same", "team-a")
	if status, _ := service.Lookup(t, b, "same"); status != 204 {
		t.Fatal("cross tenant hit", status)
	}
	if status, _ := service.Lookup(t, service.Signer.Reader(t), "same"); status != 401 {
		t.Fatal("missing tenant accepted", status)
	}
	service.Save(t, b, "same", "team-b")
	mu.Lock()
	access = "rotated"
	before := calls
	mu.Unlock()
	// The first download fails closed and invalidates the stale key.
	if status, _ := service.Restore(t, a, "same"); status != 503 {
		t.Fatal("stale credential accepted", status)
	}
	if _, data := service.Restore(t, a, "same"); data != "team-a" {
		t.Fatal("rotation not recovered", data)
	}
	mu.Lock()
	if calls <= before {
		t.Error("rotation did not re-read Central")
	}
	fail = true
	mu.Unlock()
	p.Invalidate(1)
	if status, _ := service.Lookup(t, a, "same"); status != 401 {
		t.Fatal("Central failure opened request", status)
	}
	if _, err := p.Bucket(context.Background(), 4); err == nil {
		t.Fatal("unknown tenant accepted")
	}

}

func TestCentralBucketExactContractAndExpiry(t *testing.T) {
	valid := `{"team":"a","tenant_id":1,"bucket":"cache","endpoint":"https://s3.example","region":"r","access_key":"a","secret_key":"s"}`
	body := valid
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer server.Close()
	p := &cacheBucketProvider{central: cacheCentralHTTPS{origin: server.URL, token: "regional", client: server.Client()}}
	if _, err := p.Bucket(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(valid, `"tenant_id":1`, `"tenant_id":2`, 1)
	if _, err := p.Bucket(context.Background(), 1); err != nil {
		t.Fatal("bounded cache not used")
	}
	p.entries[1] = cacheBucketEntry{until: time.Now().Add(-time.Second)}
	if _, err := p.Bucket(context.Background(), 1); err == nil {
		t.Fatal("expired identity accepted")
	}
	for _, mutation := range []string{
		strings.Replace(valid, `"team":"a"`, `"team":"a","team":"a"`, 1),
		strings.Replace(valid, `"team":"a"`, `"team":null`, 1),
		strings.Replace(valid, `"bucket":"cache",`, "", 1),
		strings.Replace(valid, `"region":"r"`, `"region":"r","extra":0`, 1),
	} {
		body = mutation
		if _, err := p.Bucket(context.Background(), 1); err == nil {
			t.Fatal("invalid exact contract accepted")
		}
	}
}

func TestManagedDataConfiguration(t *testing.T) {
	config, _ := cacheDataConfigFixture(t)
	data, _ := json.Marshal(config)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	delete(fields, "garage")
	fields["managed"] = map[string]any{"central_url": "https://central.example", "central_token_file": "/etc/token", "storage_ca_file": "/etc/ca"}
	data, _ = json.Marshal(fields)
	if _, _, err := decodeCacheDataServiceConfig(data); err != nil {
		t.Fatal("managed config refused", err)
	}
	fields["garage"] = config.Garage
	data, _ = json.Marshal(fields)
	if _, _, err := decodeCacheDataServiceConfig(data); err == nil {
		t.Fatal("mixed modes accepted")
	}
	delete(fields, "garage")
	m := fields["managed"].(map[string]any)
	m["teams"] = map[string]string{"1": "a"}
	data, _ = json.Marshal(fields)
	if _, _, err := decodeCacheDataServiceConfig(data); err == nil {
		t.Fatal("registry accepted")
	}

}

func TestCacheServiceBucketFailureDisablesSetup(t *testing.T) {
	for _, status := range []int{200, 404, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			service, fixture := serviceFixture(t, nil, 200, false)
			lease, err := service.central.lease(t.Context(), netip.MustParseAddr("fdaa:1:0:2::7"))
			if err != nil {
				t.Fatal(err)
			}
			central := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "cache_lease") {
					json.NewEncoder(w).Encode(lease)
					return
				}
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"team": lease.Team, "tenant_id": lease.TenantID, "bucket": "cache", "endpoint": "https://s3.example", "region": "r", "access_key": "a", "secret_key": "s"})
			}))
			defer central.Close()
			service.central = cacheCentralWithBuckets{cacheCentralHTTPS{origin: central.URL, token: "regional", client: central.Client()}}
			want := "disabled"
			if status == 200 {
				want = "enabled"
			}
			if got := serviceSetup(t, service, fixture.setup, servicePeer); got != want {
				t.Fatal("bucket status", status, got)
			}
		})
	}
}
