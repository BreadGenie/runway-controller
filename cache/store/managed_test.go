package store_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
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

func (b buckets) Tenants(context.Context) ([]int64, error) {
	ids := []int64{}
	for id := range b {
		ids = append(ids, id)
	}
	return ids, nil
}
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

func TestManagedBucketsNamespaceAndColdReset(t *testing.T) {
	a, ac := teamGarage(t)
	b, bc := teamGarage(t)
	const prefix = "managed/region-cache/"
	for _, g := range []*storetest.Garage{a, b} {
		g.Objects["/cache/"+prefix+"old/1"] = []byte("orphan")
		g.Objects["/cache/foreign/1"] = []byte("foreign")
		g.Objects["/cache/managed/other/1"] = []byte("other service")
	}
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: ac, 2: bc}; c.ManagedPrefix = prefix })
	for _, g := range []*storetest.Garage{a, b} {
		g.Mu.Lock()
		if len(g.Objects) != 2 {
			t.Error("cold reset did not preserve exactly foreign objects", g.Objects)
		}
		g.Mu.Unlock()
	}
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
	service.Restart(t)
	if _, data := service.Restore(t, main, "deps-linux-1"); data != "team-a" {
		t.Fatal("journal recovery", data)
	}
	if _, data := service.Restore(t, other, "deps-linux-1"); data != "team-b" {
		t.Fatal("bucket routing", data)
	}
	for id, g := range map[string]*storetest.Garage{"1": a, "2": b} {
		g.Mu.Lock()
		for key := range g.Objects {
			if strings.HasPrefix(key, "/cache/"+prefix) && !strings.HasPrefix(key, "/cache/"+prefix+id+"/") {
				t.Error("wrong tenant bucket", key)
			}
		}
		g.Mu.Unlock()
	}
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
	for _, g := range []*storetest.Garage{a, b} {
		g.Mu.Lock()
		if len(g.Objects) != 2 {
			t.Error("reset left objects", g.Objects)
		}
		g.Mu.Unlock()
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

func TestManagedResetFailureLeavesNoJournal(t *testing.T) {
	g, c := teamGarage(t)
	baseline := storetest.Start(t)
	config := baseline.Config
	config.Buckets = buckets{1: c}
	config.ManagedPrefix = "managed/test/"
	config.Disk.Directory = filepath.Join(t.TempDir(), "spools")
	config.StateDir = filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(config.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	g.Mu.Lock()
	g.Fail = true
	g.Mu.Unlock()
	if s, err := store.New(config); err == nil {
		s.Close(context.Background())
		t.Fatal("reset failure served")
	}
	if _, err := os.Stat(filepath.Join(config.StateDir, "state.json")); !os.IsNotExist(err) {
		t.Fatal("failed reset wrote journal", err)
	}
	g.Mu.Lock()
	g.Fail = false
	g.Mu.Unlock()
	s, err := store.New(config)
	if err != nil {
		t.Fatal("reset did not retry", err)
	}
	s.Close(context.Background())
}

func TestManagedTLSRejectsUntrustedServer(t *testing.T) {
	_, c := teamGarage(t)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	c.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	baseline := storetest.Start(t)
	config := baseline.Config
	config.Buckets = buckets{1: c}
	config.ManagedPrefix = "managed/test/"
	config.Disk.Directory = filepath.Join(t.TempDir(), "spools")
	config.StateDir = filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(config.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := store.New(config); err == nil {
		s.Close(context.Background())
		t.Fatal("untrusted TLS server accepted")
	}
}
