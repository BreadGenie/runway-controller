package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/BreadGenie/runway/cache/grant"
)

type cachePolicyTestTransport struct {
	testing     *testing.T
	destination *url.URL
	transport   http.RoundTripper
}

func (transport cachePolicyTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" || (request.URL.Path != "/repos/example/repo" && request.URL.Path != "/repos/example/repo/actions/runs/12") {
		transport.testing.Error("repository request escaped fixed GitHub endpoint")
	}
	copy := request.Clone(request.Context())
	copy.URL.Scheme, copy.URL.Host = transport.destination.Scheme, transport.destination.Host
	copy.Host = transport.destination.Host
	return transport.transport.RoundTrip(copy)
}

func TestCacheRepositoryPolicyReadsBoundedVerifiedHTTP(t *testing.T) {
	valid := `{"id":9007199254741001,"full_name":"example/repo","default_branch":"main","description":"ignored"}`
	for _, test := range []struct {
		name, body string
		status     int
		success    bool
	}{
		{"valid", valid, 200, true},
		{"wrong repository", strings.Replace(valid, "example/repo", "other/repo", 1), 200, false},
		{"zero id", `{"id":0,"full_name":"example/repo","default_branch":"main"}`, 200, false},
		{"fraction id", `{"id":1.5,"full_name":"example/repo","default_branch":"main"}`, 200, false},
		{"missing", `{}`, 200, false},
		{"malformed", `{`, 200, false},
		{"oversized", strings.Repeat(" ", 65537) + valid, 200, false},
		{"redirect", valid, 302, false},
		{"denied", valid, 403, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
					t.Error("API version missing")
				}
				w.Header().Set("Location", "https://not-github.invalid/")
				w.WriteHeader(test.status)
				w.Write([]byte(test.body))
			}))
			defer server.Close()
			destination, _ := url.Parse(server.URL)
			client := server.Client()
			client.Transport = cachePolicyTestTransport{t, destination, client.Transport}
			policy, err := readCacheRepositoryPolicy(t.Context(), client, "example/repo")
			if calls != 1 {
				t.Fatal("metadata read retried or followed redirect")
			}
			if (err == nil) != test.success {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if test.success && (policy.RepositoryID != 9007199254741001 || policy.DefaultBranch != "main") {
				t.Fatal("metadata precision changed")
			}
			if !test.success && policy != (grant.RepositoryPolicy{}) {
				t.Fatal("failed read returned policy")
			}
		})
	}
}

func TestCacheRepositoryPolicyCancellationAndInvalidSelector(t *testing.T) {
	for _, repository := range []string{"example/../repo", "../repo", "example/repo?token=bad", "example/repo\n", ""} {
		if _, err := readCacheRepositoryPolicy(t.Context(), http.DefaultClient, repository); err == nil {
			t.Fatal("invalid selector accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readCacheRepositoryPolicy(ctx, http.DefaultClient, "example/repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestCacheRepositoryCredentialNeverLeavesFixedMetadataRoute(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		if request.Header.Get("Authorization") != "Bearer fixture-only" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
			t.Error("credential boundary failed")
		}
		w.Write([]byte(`{"id":1,"full_name":"example/repo","default_branch":"main"}`))
	}))
	defer server.Close()
	destination, _ := url.Parse(server.URL)
	client := server.Client()
	client.Transport = cacheRepositoryCredential{token: "fixture-only", transport: cachePolicyTestTransport{t, destination, client.Transport}}
	if _, err := readCacheRepositoryPolicy(t.Context(), client, "example/repo"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://api.github.com/repos/example/repo", "https://other.invalid/repos/example/repo", "https://api.github.com/user", "https://api.github.com/repos/example/repo?secret=x", "https://user@api.github.com/repos/example/repo"} {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, nil)
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
			t.Fatal("credential accepted foreign route")
		}
	}
	if calls != 1 {
		t.Fatal("rejected route made an HTTP request")
	}
}
