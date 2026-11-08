package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseReplacePreservesPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	for _, value := range []string{"first", "second", "second"} {
		if err := releaseReplace(path, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	previous, err := os.ReadFile(path + ".previous")
	if err != nil || string(previous) != "first" {
		t.Fatalf("previous = %q, %v", previous, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private config: %v %v", info, err)
	}
}

func TestReleaseTLSIdentity(t *testing.T) {
	dir := t.TempDir()
	local := func(path string) string { return filepath.Join(dir, path) }
	c := cacheServiceConfig{CertificateFile: "certificate.pem", PrivateKeyFile: "private-key.pem", CacheDataCAFile: "ca.pem"}
	if err := prepareCacheServiceTLS(c, local, "runway-cache-svc.example.test"); err != nil {
		t.Fatal(err)
	}
	certificate, _ := os.ReadFile(local(c.CertificateFile))
	ca, _ := os.ReadFile(local(c.CacheDataCAFile))
	if string(certificate) != string(ca) {
		t.Fatal("public CA differs from service certificate")
	}
	if err := os.WriteFile(local(c.CacheDataCAFile), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(local(c.PrivateKeyFile))
	if err := prepareCacheServiceTLS(c, local, "runway-cache-svc.example.test"); err != nil {
		t.Fatal(err)
	}
	ca, _ = os.ReadFile(local(c.CacheDataCAFile))
	if string(certificate) != string(ca) {
		t.Fatal("public CA not repaired on initialization")
	}
	after, _ := os.ReadFile(local(c.PrivateKeyFile))
	if string(before) != string(after) {
		t.Fatal("TLS key replaced")
	}
	if err := prepareCacheServiceTLS(c, local, "different.example.test"); err == nil {
		t.Fatal("accepted incompatible identity")
	}
	if err := os.Remove(local(c.CertificateFile)); err != nil {
		t.Fatal(err)
	}
	if err := prepareCacheServiceTLS(c, local, "runway-cache-svc.example.test"); err == nil {
		t.Fatal("accepted partial identity")
	}
}
