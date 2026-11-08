package main

import (
	"bytes"
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
