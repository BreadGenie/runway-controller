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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BreadGenie/runway-controller/cache/store"
	"github.com/BreadGenie/runway-controller/cache/store/storetest"
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
	if status, _ := service.Lookup(t, main, "deps"); status != 200 {
		t.Fatal("recreated service missed checkpointed objects", status)
	}
	newJournal, _ := os.ReadFile(filepath.Join(service.Config.StateDir, "state.json"))
	var fresh struct{ Namespace string }
	json.Unmarshal(newJournal, &fresh)
	if old.Namespace == fresh.Namespace || !strings.HasPrefix(fresh.Namespace, "managed/") {
		t.Fatal("recreation reused namespace")
	}

}

// Read tickets are bound to the grant's signed tenant at mint. Redemption
// checks the ticket against the redeeming grant's tenant and against the
// entry's routed tenant, so a cross-tenant replay fails closed even if the
// translator's session binding regresses.
func TestManagedReadTicketsBindTenant(t *testing.T) {
	_, ac := teamGarage(t)
	_, bc := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: ac, 2: bc} })
	teamA := service.Signer.Grant(t, storetest.Job{TenantID: 1, Write: "refs/heads/main"})
	teamB := service.Signer.Grant(t, storetest.Job{TenantID: 2, Write: "refs/heads/main"})
	aID := service.Save(t, teamA, "deps-a", "team-a-bytes")
	bID := service.Save(t, teamB, "deps-b", "team-b-bytes")
	if status, data := service.Restore(t, teamA, "deps-a"); status != 200 || data != "team-a-bytes" {
		t.Fatal("same-tenant redeem", status, data)
	}
	_, locationA := service.Lookup(t, teamA, "deps-a")
	// A ticket replayed under another tenant's session is refused.
	if status, _ := service.Do(t, "GET", locationA, teamB, "", nil); status != 404 {
		t.Fatal("cross-tenant session redeem", status)
	}
	// A ticket used for another tenant's object key is refused.
	swapped := strings.Replace(locationA, "/download/"+strconv.FormatInt(aID, 10)+"?", "/download/"+strconv.FormatInt(bID, 10)+"?", 1)
	if swapped == locationA {
		t.Fatal("entry swap fixture")
	}
	if status, _ := service.Do(t, "GET", swapped, teamA, "", nil); status != 404 {
		t.Fatal("cross-tenant object redeem", status)
	}
	if status, _ := service.Do(t, "GET", swapped, teamB, "", nil); status != 404 {
		t.Fatal("cross-tenant object redeem under its own session", status)
	}
	// Another repository of the same tenant cannot redeem the ticket either.
	sibling := service.Signer.Grant(t, storetest.Job{TenantID: 1, RepositoryID: 2, Write: "refs/heads/main"})
	if status, _ := service.Do(t, "GET", locationA, sibling, "", nil); status != 404 {
		t.Fatal("same-tenant other-repository redeem", status)
	}
	// No bearer, a forged bearer, or a standalone grant redeems nothing.
	for name, token := range map[string]string{"none": "", "forged": teamA + "x", "standalone": service.Signer.Reader(t)} {
		if status, _ := service.Do(t, "GET", locationA, token, "", nil); status != 404 {
			t.Errorf("%s redeem: %d", name, status)
		}
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
	// Exercise the archive PUT independently of fresh-VM checkpoint recovery.
	service.Restart(t)
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
