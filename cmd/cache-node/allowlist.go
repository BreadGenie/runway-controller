package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"regexp"
)

//go:embed allowlist.json
var allowlistData []byte

type allowlistEntry struct {
	Repo        string `json:"repo"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	HarnessOnly bool   `json:"harness_only"`
}

// Invalid embedded data disables every route, without breaking Node execution.
func loadAllowlist(data []byte) map[string]map[string]bool {
	empty := map[string]map[string]bool{}
	// Reject duplicate object keys as well as unknown fields.
	scan := json.NewDecoder(bytes.NewReader(data))
	var walk func() bool
	walk = func() bool {
		t, err := scan.Token()
		if err != nil {
			return false
		}
		if d, ok := t.(json.Delim); ok {
			seen := map[string]bool{}
			for scan.More() {
				if d == '{' {
					k, err := scan.Token()
					key, ok := k.(string)
					if err != nil || !ok || seen[key] || !map[string]bool{"repo": true, "version": true, "commit": true, "path": true, "sha256": true, "harness_only": true}[key] {
						return false
					}
					seen[key] = true
				}
				if !walk() {
					return false
				}
			}
			if d == '{' && len(seen) != 6 {
				return false
			}
			end, err := scan.Token()
			return err == nil && ((d == '[' && end == json.Delim(']')) || (d == '{' && end == json.Delim('}')))
		}
		return t != nil
	}
	if !walk() {
		return empty
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var entries []allowlistEntry
	if decoder.Decode(&entries) != nil || len(entries) == 0 {
		return empty
	}
	if decoder.Decode(new(any)) != io.EOF {
		return empty
	}
	sha := regexp.MustCompile("^[0-9a-f]{40}$")
	digest := regexp.MustCompile("^[0-9a-f]{64}$")
	version := regexp.MustCompile("^v[1-9][0-9]*\\.[0-9]+\\.[0-9]+$")
	paths := map[string]map[string]bool{
		"actions/cache":      {"dist/restore/index.js": true, "dist/save/index.js": true, "dist/restore-only/index.js": true, "dist/save-only/index.js": true},
		"astral-sh/setup-uv": {"dist/setup/index.js": true, "dist/save-cache/index.js": true, "dist/setup/index.cjs": true, "dist/save-cache/index.cjs": true, "action.yml": true},
		"actions/setup-node": {"dist/setup/index.js": true, "dist/cache-save/index.js": true, "action.yml": true},
	}
	result := map[string]map[string]bool{"actions/cache": {}, "actions/setup-node": {}, "astral-sh/setup-uv": {}}
	seen := map[string]bool{}
	for _, e := range entries {
		key := e.Repo + "/" + e.Commit + "/" + e.Path
		if !paths[e.Repo][e.Path] || !sha.MatchString(e.Commit) || !digest.MatchString(e.SHA256) || !version.MatchString(e.Version) || e.HarnessOnly != (e.Path == "action.yml") || seen[key] {
			return empty
		}
		seen[key] = true
		if !e.HarnessOnly {
			result[e.Repo][e.SHA256] = true
		}
	}
	return result
}

const allowlistPath = "/etc/runway/cache-allowlist.json"

// A present override is authoritative, including when it is unsafe or invalid.
// Called only after candidate detection; ordinary Node invocations do no I/O.
func loadGuestAllowlist(path string) map[string]map[string]bool {
	return loadGuestAllowlistWith(path, trustedFile)
}

func loadGuestAllowlistWith(path string, read func(string) ([]byte, error)) map[string]map[string]bool {
	data, err := read(path)
	if os.IsNotExist(err) {
		// A dangling symlink is present and unsafe, not an absent override.
		if _, linkErr := os.Lstat(path); !os.IsNotExist(linkErr) {
			return map[string]map[string]bool{}
		}
		return loadAllowlist(allowlistData)
	}
	if err != nil {
		return map[string]map[string]bool{}
	}
	return loadAllowlist(data)
}
