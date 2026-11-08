package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/store"
	"github.com/BreadGenie/runway/cache/store/storetest"
)

type buckets map[int64]store.GarageConfig

func (b buckets) Bucket(_ context.Context, id int64) (store.GarageConfig, error) { return b[id], nil }

func teamGarage(t *testing.T) (*storetest.Garage, store.GarageConfig) {
	g := storetest.NewGarage(t)
	tls := httptest.NewTLSServer(g.Server.Config.Handler)
	t.Cleanup(tls.Close)
	c := g.Config()
	c.Endpoint = tls.URL
	c.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tls.Certificate().Raw}))
	return g, c
}

func TestManagedBucketsNamespaceAndRecreation(t *testing.T) {
	a, ac := teamGarage(t)
	b, bc := teamGarage(t)
	const prefix = "managed/region-cache/"
	for _, g := range []*storetest.Garage{a, b} {
		g.Objects["/cache/"+prefix+"old/1"] = []byte("orphan")
		g.Objects["/cache/foreign/1"] = []byte("foreign")
		g.Objects["/cache/managed/other/1"] = []byte("other service")
	}
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: ac, 2: bc} })
	main := service.Signer.Grant(t, storetest.Job{TenantID: 1, Write: "refs/heads/main"})
	other := service.Signer.Grant(t, storetest.Job{TenantID: 2, Write: "refs/heads/main"})
	service.Save(t, main, "deps-linux-1", "team-a")
	if status, _ := service.Lookup(t, other, "deps"); status != 204 {
		t.Fatal("cross-team hit", status)
	}
	if status, _ := service.Lookup(t, service.Signer.Reader(t), "deps"); status != 401 {
		t.Fatal("standalone entered managed service", status)
	}
	service.Save(t, other, "deps-linux-1", "team-b")
	service.Save(t, main, "deps-linux-2", "newer")
	if _, data := service.Restore(t, main, "deps-linux"); data != "newer" {
		t.Fatal("restore prefix", data)
	}
	feature := service.Signer.Grant(t, storetest.Job{TenantID: 1, Read: []string{"refs/heads/feature", "refs/heads/main"}, Write: "refs/heads/feature"})
	if _, data := service.Restore(t, feature, "deps-linux-1"); data != "team-a" {
		t.Fatal("default ref", data)
	}
	service.Save(t, feature, "deps-linux-1", "feature")
	if _, data := service.Restore(t, feature, "deps-linux-1"); data != "feature" {
		t.Fatal("own ref", data)
	}
	if status, _ := service.Do(t, "GET", "/_apis/artifactcache/cache?version=v2&keys=deps-linux", main, "", nil); status != 204 {
		t.Fatal("version crossed", status)
	}
	beforeRestart, err := os.ReadFile(filepath.Join(service.Config.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var before struct{ Namespace string }
	if err := json.Unmarshal(beforeRestart, &before); err != nil {
		t.Fatal(err)
	}
	service.Restart(t)
	afterRestart, _ := os.ReadFile(filepath.Join(service.Config.StateDir, "state.json"))
	var after struct{ Namespace string }
	if err := json.Unmarshal(afterRestart, &after); err != nil {
		t.Fatal(err)
	}
	if before.Namespace != after.Namespace {
		t.Fatal("restart changed service id")
	}
	if _, data := service.Restore(t, main, "deps-linux-1"); data != "team-a" {
		t.Fatal("journal recovery", data)
	}
	if _, data := service.Restore(t, other, "deps-linux-1"); data != "team-b" {
		t.Fatal("bucket routing", data)
	}
	oldJournal, err := os.ReadFile(filepath.Join(service.Config.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var old struct{ Namespace string }
	json.Unmarshal(oldJournal, &old)
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Server = nil
	if err := os.Remove(filepath.Join(service.Config.StateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	service.Restart(t)
	if status, _ := service.Lookup(t, main, "deps"); status != 204 {
		t.Fatal("recreated service adopted objects", status)
	}
	newJournal, _ := os.ReadFile(filepath.Join(service.Config.StateDir, "state.json"))
	var fresh struct{ Namespace string }
	json.Unmarshal(newJournal, &fresh)
	if old.Namespace == fresh.Namespace || !strings.HasPrefix(fresh.Namespace, "managed/") {
		t.Fatal("recreation reused namespace")
	}

}

func TestGarageTLSConfigurationRefusals(t *testing.T) {
	_, valid := teamGarage(t)
	for _, test := range []struct {
		name   string
		change func(*store.GarageConfig)
	}{
		{"remote-http", func(c *store.GarageConfig) { c.Endpoint = "http://storage.example:80"; c.CAPEM = "" }},
		{"missing-ca", func(c *store.GarageConfig) { c.CAPEM = "" }},
		{"bad-ca", func(c *store.GarageConfig) { c.CAPEM = "not a certificate" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseline := storetest.Start(t)
			c := baseline.Config
			c.Garage = valid
			test.change(&c.Garage)
			c.StateDir = filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(c.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			if s, err := store.New(c); err == nil {
				s.Close(context.Background())
				t.Fatal("unsafe TLS configuration accepted")
			}
		})
	}
}

func TestManagedTLSRejectsUntrustedServer(t *testing.T) {
	_, config := teamGarage(t)
	// Use a distinct CA rather than the httptest certificate shared by servers.
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	config.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
	token := service.Signer.Grant(t, storetest.Job{TenantID: 1, Write: "refs/heads/main"})
	status, id := service.Reserve(t, token, "deps", 7)
	if status != 201 {
		t.Fatal("reserve", status)
	}
	if status := service.Upload(t, token, id, "archive"); status != 503 {
		t.Fatal("untrusted storage accepted", status)
	}
}

// Pinned actions/cache v4's URL builder escapes encodeURIComponent's comma
// again; the HTTP query therefore contains %252C between ordered restore keys.
func TestLegacyDoubleEscapedRestoreKeyDelimiter(t *testing.T) {
	service := storetest.Start(t)
	token := service.Signer.Writer(t)
	service.Save(t, token, "deps-linux-1", "archive")
	for _, keys := range []string{"missing%2Cdeps-", "missing%252Cdeps-", "missing%252cdeps-"} {
		status, body := service.Do(t, "GET", "/_apis/artifactcache/cache?version=v1&keys="+keys, token, "", nil)
		if status != 200 {
			t.Fatalf("restore keys %s: %d %s", keys, status, body)
		}
	}
}

func TestLiteralEscapedCommaKeyStillRestores(t *testing.T) {
	service := storetest.Start(t)
	token := service.Signer.Writer(t)
	service.Save(t, token, "literal%2Ckey", "archive")
	status, raw := service.Do(t, "GET", "/_apis/artifactcache/cache?version=v1&keys=literal%252Ckey", token, "", nil)
	if status != 200 {
		t.Fatalf("literal percent key lost: %d %s", status, raw)
	}
}
