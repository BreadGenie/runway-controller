package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"regexp"
	"strings"
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
		"actions/setup-node": {"dist/setup/index.js": true, "dist/cache-save/index.js": true, "action.yml": true},
	}
	result := map[string]map[string]bool{"cache": {}, "setup-node": {}}
	seen := map[string]bool{}
	for _, e := range entries {
		key := e.Repo + "/" + e.Commit + "/" + e.Path
		if !paths[e.Repo][e.Path] || !sha.MatchString(e.Commit) || !digest.MatchString(e.SHA256) || !version.MatchString(e.Version) || e.HarnessOnly != (e.Path == "action.yml") || seen[key] {
			return empty
		}
		seen[key] = true
		if !e.HarnessOnly {
			result[strings.TrimPrefix(e.Repo, "actions/")][e.SHA256] = true
		}
	}
	return result
}
