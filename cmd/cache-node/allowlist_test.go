package main

import (
	"bytes"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestAllowlistFailsClosed(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("null"), bytes.Replace(allowlistData, []byte(`"harness_only": false`), []byte(`"harness_only": null`), 1),
		bytes.Replace(allowlistData, []byte(`"repo":`), []byte(`"Repo":`), 1), []byte("[]"), []byte("{"), append(append([]byte{}, allowlistData...), []byte("{}")...),
		bytes.Replace(allowlistData, []byte("actions/cache"), []byte("actions/unknown"), 1),
		bytes.Replace(allowlistData, []byte(`"repo":`), []byte(`"extra": 1, "repo":`), 1),
		bytes.Replace(allowlistData, []byte(`"repo":`), []byte(`"repo": "actions/cache", "repo":`), 1),
	} {
		if got := loadAllowlist(data); len(got) != 0 {
			t.Fatalf("invalid allowlist routes: %s", data)
		}
	}
	got := loadAllowlist(allowlistData)
	if len(got["cache"]) != 12 || len(got["setup-node"]) != 4 {
		t.Fatalf("lost original pins: %v", got)
	}
}

func TestFastPathDoesNotParseAllowlist(t *testing.T) {
	if bundles != nil {
		t.Skip("allowlist already loaded by an earlier test in this process")
	}
	environment([]string{"node", "/missing/unrelated.js"}, nil, func() bool { return false }, func(string) {})
	if bundles != nil {
		t.Fatal("the fast path parsed the allowlist")
	}
}

func TestGuestAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowlist.json")
	if got := loadGuestAllowlist(path); !reflect.DeepEqual(got, loadAllowlist(allowlistData)) {
		t.Fatal("absent file must use embedded allowlist")
	}
	// A valid file with a distinct digest replaces, rather than extends, embedded pins.
	data := bytes.ReplaceAll(allowlistData, []byte("d7b9163d46eb2a31a2ba58c40fa8a8c342d19b9211676ca6a32809d298352475"), []byte(strings.Repeat("a", 64)))
	// Exercise selection independently of the root-only filesystem trust fixtures.
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadGuestAllowlistWith(path, os.ReadFile); !reflect.DeepEqual(got, loadAllowlist(data)) {
		t.Fatal("valid external data did not replace embedded pins")
	}
	if got := loadGuestAllowlist(path); os.Geteuid() != 0 && len(got) != 0 {
		t.Fatal("non-root-owned allowlist routed")
	}
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if len(loadGuestAllowlistWith(path, os.ReadFile)) != 0 {
		t.Fatal("invalid external data fell back to embedded pins")
	}
	for _, tc := range []struct {
		name  string
		data  []byte
		mode  os.FileMode
		uid   int
		valid bool
	}{
		{"valid", data, 0644, 0, true},
		{"invalid", []byte("{}"), 0644, 0, false},
		{"group writable", data, 0664, 0, false},
		{"world writable", data, 0646, 0, false},
		{"not root", data, 0644, 1001, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Geteuid() != 0 {
				t.Skip("root ownership fixture requires root")
			}
			os.Remove(path)
			if err := os.WriteFile(path, tc.data, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(path, tc.uid, 0); err != nil {
				t.Fatal(err)
			}
			got := loadGuestAllowlist(path)
			if tc.valid {
				if !reflect.DeepEqual(got, loadAllowlist(data)) {
					t.Fatal("file not preferred")
				}
			} else if len(got) != 0 {
				t.Fatal("unsafe/invalid file routed")
			}
		})
	}
	os.Remove(path)
	if err := os.Symlink(path+".absent", path); err != nil {
		t.Fatal(err)
	}
	if len(loadGuestAllowlist(path)) != 0 {
		t.Fatal("dangling symlink used embedded pins")
	}
	os.Remove(path)
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if len(loadGuestAllowlist(path)) != 0 {
		t.Fatal("FIFO routed")
	}
}

// Synthetic ownership lets the trust-rule matrix run on unprivileged CI too.
type cacheFileInfo struct {
	os.FileInfo
	uid  uint32
	mode os.FileMode
}

func (i cacheFileInfo) Sys() any          { return &syscall.Stat_t{Uid: i.uid} }
func (i cacheFileInfo) Mode() os.FileMode { return i.mode }

func TestGuestAllowlistTrustRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowlist.json")
	if err := os.WriteFile(path, allowlistData, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		uid   uint32
		mode  os.FileMode
		valid bool
	}{
		{"root regular", 0, 0644, true},
		{"runner owned", 1001, 0644, false},
		{"group writable", 0, 0664, false},
		{"world writable", 0, 0646, false},
		{"directory", 0, os.ModeDir | 0755, false},
		{"symlink", 0, os.ModeSymlink | 0644, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := loadGuestAllowlistWith(path, func(string) ([]byte, error) {
				if !trustedCacheFileInfo(cacheFileInfo{info, tc.uid, tc.mode}) {
					return nil, fmt.Errorf("unsafe cache file")
				}
				return allowlistData, nil
			})
			if (len(got) != 0) != tc.valid {
				t.Fatal("unexpected routing for trust rule")
			}
		})
	}
}
