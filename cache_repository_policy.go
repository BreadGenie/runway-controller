package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BreadGenie/runway-controller/cache/grant"
)

var errCacheRepository = errors.New("cache repository policy unavailable")

// newCacheRepositoryClient reuses the existing protected PAT custody rules.
// App installation credentials need their existing owner's token provider; this
// constructor never falls back to gh, environment bearer values or guest tokens.
func newCacheRepositoryClient(tokenFile string) (*http.Client, error) {
	encoded, err := readProtectedCacheFile(tokenFile, true)
	if err != nil {
		return nil, errCacheRepository
	}
	token := strings.TrimSpace(string(encoded))
	if token == "" || strings.ContainsAny(token, " \r\n\t") {
		return nil, errCacheRepository
	}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 32768, DisableKeepAlives: true,
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: cacheRepositoryCredential{token: token, transport: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type cacheRepositoryCredential struct {
	token     string
	transport http.RoundTripper
}

func (credential cacheRepositoryCredential) RoundTrip(request *http.Request) (*http.Response, error) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/repos/"), "/")
	validPath := len(parts) == 2 && request.URL.RawQuery == ""
	if len(parts) == 5 && parts[2] == "actions" && (parts[3] == "runs" || parts[3] == "jobs") {
		validPath = cacheCanonicalPositive(parts[4]) && request.URL.RawQuery == ""
	}
	// One attempt's job page: the only route that carries a (fixed-shape) query.
	if len(parts) == 8 && parts[2] == "actions" && parts[3] == "runs" && parts[5] == "attempts" && parts[7] == "jobs" {
		validPath = cacheCanonicalPositive(parts[4]) && cacheCanonicalPositive(parts[6]) && cacheJobPageQuery.MatchString(request.URL.RawQuery)
	}
	if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "api.github.com" || request.URL.User != nil || request.URL.Fragment != "" || !strings.HasPrefix(request.URL.Path, "/repos/") || !validPath || !validCacheGitHubPart(parts[0]) || !validCacheGitHubPart(parts[1]) || (request.Host != "" && request.Host != "api.github.com") {
		return nil, errCacheRepository
	}
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+credential.token)
	copy.Header.Del("Proxy-Authorization")
	copy.Header.Del("Cookie")
	return credential.transport.RoundTrip(copy)
}

var cacheJobPageQuery = regexp.MustCompile(`^per_page=100&page=[1-9][0-9]?$`)

func cacheCanonicalPositive(value string) bool {
	number, err := strconv.ParseInt(value, 10, 64)
	return err == nil && number > 0 && strconv.FormatInt(number, 10) == value
}

// readCacheRepositoryPolicy reads the repository's stable ID and default branch
// with the host credential. It is metadata, not permission to issue a grant.
func readCacheRepositoryPolicy(ctx context.Context, authenticated *http.Client, repository string) (grant.RepositoryPolicy, error) {
	parts := strings.Split(repository, "/")
	if authenticated == nil || len(parts) != 2 || !validCacheGitHubPart(parts[0]) || !validCacheGitHubPart(parts[1]) || len(repository) > 512 {
		return grant.RepositoryPolicy{}, errCacheRepository
	}
	data, err := readCacheGitHubMetadata(ctx, authenticated, "/repos/"+repository)
	if err != nil {
		return grant.RepositoryPolicy{}, err
	}
	return decodeCacheRepositoryPolicy(data, repository)
}

func readCacheGitHubMetadata(ctx context.Context, authenticated *http.Client, path string) ([]byte, error) {
	return readCacheGitHubBounded(ctx, authenticated, path, 65536)
}

// readCacheGitHubBounded performs one GET with a five-second ceiling (a shorter
// parent deadline wins), no redirects and a response-size limit.
func readCacheGitHubBounded(ctx context.Context, authenticated *http.Client, path string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := *authenticated
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, errCacheRepository
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "runway-cache")
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.Join(errCacheRepository, ctx.Err())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errCacheRepository
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errCacheRepository
	}
	return data, nil
}

func decodeCacheRepositoryPolicy(data []byte, repository string) (grant.RepositoryPolicy, error) {
	var wire struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.ID <= 0 || wire.FullName != repository || wire.DefaultBranch == "" || len(wire.DefaultBranch) > 1024 || strings.ContainsAny(wire.DefaultBranch, "\x00\r\n\t ") {
		return grant.RepositoryPolicy{}, errCacheRepository
	}
	return grant.RepositoryPolicy{RepositoryID: wire.ID, Repository: wire.FullName, DefaultBranch: wire.DefaultBranch}, nil
}
