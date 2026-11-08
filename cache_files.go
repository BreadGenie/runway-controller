package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

var errCacheInvalid = errors.New("invalid cache input")

var cacheGitHubPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

func canonicalCacheUUID(value string) bool {
	identity, err := uuid.Parse(value)
	return err == nil && identity != uuid.Nil && identity.String() == value
}

func validCacheGitHubPart(value string) bool {
	return value != "." && value != ".." && cacheGitHubPartPattern.MatchString(value)
}

// readCacheObject accepts one JSON object of at most 4 KiB holding exactly
// the given fields, each non-null.
func readCacheObject(encoded []byte, fields []string) error {
	if len(encoded) > 4096 || !utf8.Valid(encoded) {
		return errCacheInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if first, err := decoder.Token(); err != nil || first != json.Delim('{') {
		return errCacheInvalid
	}
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		seen[field] = false
	}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return errCacheInvalid
		}
		if _, allowed := seen[key]; !allowed {
			return errCacheInvalid
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || bytes.Equal(value, []byte("null")) {
			return errCacheInvalid
		}
	}
	if _, err := decoder.Token(); err != nil {
		return errCacheInvalid
	}
	for _, present := range seen {
		if !present {
			return errCacheInvalid
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errCacheInvalid
	}
	return nil
}

func cacheGitHubClient(scopeURL, path string, options ...scaleset.HTTPOption) (*scaleset.Client, error) {
	encoded, err := readProtectedCacheFile(path, true)
	if err != nil || len(encoded) > 4096 {
		return nil, errors.New("protected GitHub credential unavailable")
	}
	token := strings.TrimSpace(string(encoded))
	if token == "" || strings.ContainsAny(token, " \r\n\t") {
		return nil, errors.New("invalid protected GitHub credential")
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: scopeURL, PersonalAccessToken: token}, options...)
}

// readProtectedCacheFile reads a root-owned, single-link regular file that is
// not group or world writable (nor readable at all when private).
func readProtectedCacheFile(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errCacheInvalid
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errCacheInvalid
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 {
		return nil, errCacheInvalid
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != 0 || metadata.Nlink != 1 {
		return nil, errCacheInvalid
	}
	return io.ReadAll(io.LimitReader(file, 4097))
}

// cacheSocketDirectoryGroup permits only the jailed peer's group to traverse.
func cacheSocketDirectoryGroup(dir string) (uint32, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0750 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return 0, errCacheInvalid
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != 0 {
		return 0, errCacheInvalid
	}
	return metadata.Gid, nil
}
