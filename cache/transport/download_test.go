package transport

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const ticketPath = "/download/1?ticket=" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func downloads(c *Config) {
	c.Limits.ArchiveBytes = 256 << 10
	c.Limits.ResponseBytes = 4096
}

func TestDownloadLookupValidation(t *testing.T) {
	var origin, location string
	var raw string
	_, front := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if raw != "" {
			_, _ = io.WriteString(w, raw)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"archiveLocation": location, "cacheKey": "key", "scope": "refs/heads/main"})
	}, func(c *Config) { downloads(c); origin = c.Origin })
	location = origin + ticketPath
	status, body := send(t, front, "GET", "/_apis/artifactcache/cache?keys=k&version=v", "")
	var found map[string]string
	if status != 200 || json.Unmarshal([]byte(body), &found) != nil || found["archiveLocation"] != archiveRelayOrigin+ticketPath || found["cacheKey"] != "key" || found["scope"] != "refs/heads/main" {
		t.Fatal("lookup rewrite lost protocol fields")
	}
	for _, suffix := range []string{
		"/download/01?ticket=" + strings.Repeat("a", 64), "/download/0?ticket=" + strings.Repeat("a", 64),
		"/download/9223372036854775808?ticket=" + strings.Repeat("a", 64),
		"/download/%31?ticket=" + strings.Repeat("a", 64), "/download/1/../1?ticket=" + strings.Repeat("a", 64),
		ticketPath + "&ticket=" + strings.Repeat("a", 64), ticketPath + "&extra=x", ticketPath + "&", ticketPath + "#", ticketPath + "#fragment",
		strings.Replace(ticketPath, "ticket=", "ticket=%61", 1), strings.Replace(ticketPath, "abcdef", "ABCDEF", 1),
		strings.Replace(ticketPath, "ticket=", "%74icket=", 1), ticketPath[:len(ticketPath)-1],
	} {
		location = origin + suffix
		if status, _ := send(t, front, "GET", "/_apis/artifactcache/cache?keys=k&version=v", ""); status != 502 {
			t.Errorf("accepted malformed location variant: %d", status)
		}
	}
	u, _ := url.Parse(origin)
	for _, badOrigin := range []string{"https://foreign.invalid", "http://" + u.Host, "https://user@" + u.Host, origin + ".evil.invalid", "//" + u.Host, "https:"} {
		location = badOrigin + ticketPath
		if status, _ := send(t, front, "GET", "/_apis/artifactcache/cache?keys=k&version=v", ""); status != 502 {
			t.Errorf("accepted foreign origin: %d", status)
		}
	}
	for _, invalid := range []string{`{}`, `null`, `[]`, `{"archiveLocation":null}`, `{"archiveLocation":5}`, `{} {}`, fmt.Sprintf(`{"archiveLocation":%q,"archiveLocation":%q}`, origin+ticketPath, origin+ticketPath)} {
		raw = invalid
		if status, _ := send(t, front, "GET", "/_apis/artifactcache/cache?keys=k&version=v", ""); status != 502 {
			t.Errorf("accepted malformed lookup: %d", status)
		}
	}
}

func TestDownloadAdmissionAndCredentials(t *testing.T) {
	var calls atomic.Int32
	h, front := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for _, key := range []string{"Authorization", "Cookie", "Forwarded", "X-Forwarded-Host", "Proxy-Authorization", "Content-Range", "Range", "If-Range"} {
			if r.Header.Get(key) != "" {
				t.Errorf("forwarded download header %s", key)
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Set-Cookie", "secret")
		if r.Method != "HEAD" {
			_, _ = io.WriteString(w, "abc")
		}
	}, downloads)
	for _, method := range []string{"GET", "HEAD"} {
		req, _ := http.NewRequest(method, front.URL+ticketPath, nil)
		req.Header.Set("Authorization", "Bearer github-secret")
		req.Header.Set("Cookie", "secret")
		req.Host = "guest.invalid"
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || err != nil || resp.ContentLength != 3 || resp.Header.Get("Set-Cookie") != "" || (method == "GET" && string(data) != "abc") || (method == "HEAD" && len(data) != 0) {
			t.Fatal("incorrect download response")
		}
	}
	for _, key := range []string{"Range", "If-Range"} {
		req, _ := http.NewRequest("GET", front.URL+ticketPath, nil)
		req.Header.Set(key, "bytes=0-1")
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal("range was silently forwarded")
		}
	}
	for _, request := range []struct{ method, path, body string }{
		{"POST", ticketPath, ""}, {"GET", ticketPath, "x"}, {"HEAD", ticketPath, "x"},
		{"GET", ticketPath + "&x=y", ""}, {"GET", strings.Replace(ticketPath, "/1?", "/01?", 1), ""},
	} {
		status, _ := send(t, front, request.method, request.path, request.body)
		if status != 400 && status != 413 {
			t.Fatal("invalid download admitted")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("refused request reached upstream")
	}
	h.Close()
	if status, _ := send(t, front, "GET", ticketPath, ""); status != 403 {
		t.Fatal("retired download admitted")
	}
}

func TestDownloadStreamingBounds(t *testing.T) {
	const limit = 256 << 10
	for _, mode := range []string{"known", "unknown", "known-overflow", "unknown-overflow", "truncated", "redirect", "partial", "encoded", "denied"} {
		t.Run(mode, func(t *testing.T) {
			_, front := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				switch mode {
				case "known":
					w.Header().Set("Content-Length", fmt.Sprint(limit))
				case "known-overflow":
					w.Header().Set("Content-Length", fmt.Sprint(limit+1))
					return
				case "unknown", "unknown-overflow":
					w.(http.Flusher).Flush()
				case "truncated":
					w.Header().Set("Content-Length", fmt.Sprint(limit))
					_, _ = io.WriteString(w, "short")
					return
				case "redirect":
					w.Header().Set("Location", "https://foreign.invalid/secret")
					w.WriteHeader(307)
					return
				case "partial":
					w.Header().Set("Content-Range", "bytes 0-1/3")
					w.WriteHeader(206)
					_, _ = io.WriteString(w, "ab")
					return
				case "encoded":
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = io.WriteString(w, "secret")
					return
				case "denied":
					w.Header().Set("Set-Cookie", "secret")
					w.WriteHeader(403)
					_, _ = io.WriteString(w, "secret")
					return
				}
				_, _ = io.WriteString(w, strings.Repeat("a", limit))
				if mode == "unknown-overflow" {
					_, _ = io.WriteString(w, "x")
				}
			}, downloads)
			resp, err := front.Client().Get(front.URL + ticketPath)
			if err != nil {
				if mode == "truncated" {
					return
				}
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, readErr := io.ReadAll(resp.Body)
			switch mode {
			case "known", "unknown":
				if resp.StatusCode != 200 || readErr != nil || string(data) != strings.Repeat("a", limit) {
					t.Fatal("stream changed archive")
				}
			case "unknown-overflow", "truncated":
				if readErr == nil || len(data) > limit || strings.Contains(string(data), "Bad Gateway") {
					t.Fatal("failed stream was not aborted cleanly")
				}
			default:
				want := 502
				if mode == "denied" {
					want = 403
				}
				if resp.StatusCode != want || strings.Contains(string(data), "secret") || resp.Header.Get("Location") != "" {
					t.Fatal("unsafe error response")
				}
			}
		})
	}
}

func TestDownloadCancellation(t *testing.T) {
	for _, mode := range []string{"close", "expiry", "client", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			started, ended := make(chan struct{}), make(chan struct{})
			h, front := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, strings.Repeat("a", 8192))
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(ended)
			}, func(c *Config) {
				downloads(c)
				if mode == "expiry" {
					c.Grant.Expires = time.Now().Add(200 * time.Millisecond)
				}
				if mode == "timeout" {
					c.Limits.Timeout = 200 * time.Millisecond
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", front.URL+ticketPath, nil)
			resp, err := front.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			<-started
			if status, _ := send(t, front, "GET", ticketPath, ""); status != 429 {
				t.Fatal("download bypassed concurrency bound")
			}
			switch mode {
			case "close":
				h.Close()
			case "client":
				cancel()
			}
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Fatal("canceled stream completed successfully")
			}
			select {
			case <-ended:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream download not canceled")
			}
		})
	}
}

func TestDownloadTLSRefusals(t *testing.T) {
	for _, mode := range []string{"untrusted", "wrong-host", "plaintext"} {
		t.Run(mode, func(t *testing.T) {
			_, front := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unverified download reached origin") }, func(c *Config) {
				downloads(c)
				switch mode {
				case "untrusted":
					c.Roots = x509.NewCertPool()
				case "wrong-host":
					c.Origin = strings.Replace(c.Origin, "127.0.0.1", "localhost", 1)
				case "plaintext":
					plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("plaintext HTTP accepted") }))
					t.Cleanup(plain.Close)
					c.Origin = strings.Replace(plain.URL, "http:", "https:", 1)
				}
			})
			if status, _ := send(t, front, "GET", ticketPath, ""); status != 502 {
				t.Fatal("unverified TLS accepted", status)
			}
		})
	}
}

func TestDownloadConfigurationRefusals(t *testing.T) {
	_, _ = fixture(t, func(w http.ResponseWriter, r *http.Request) {}, func(c *Config) {
		for _, limit := range []int64{-1, 0, 1 << 62} {
			bad := *c
			bad.Limits.ArchiveBytes = limit
			if h, err := New(t.Context(), bad); err == nil {
				h.Close()
				t.Error("invalid archive bound accepted")
			}
		}
	})
}

func TestDownloadSlowReaderDeadline(t *testing.T) {
	ended := make(chan struct{})
	h, front := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		block := strings.Repeat("a", 64<<10)
		for i := 0; i < 1024; i++ {
			if _, err := io.WriteString(w, block); err != nil {
				return
			}
		}
	}, func(c *Config) {
		downloads(c)
		c.Limits.ArchiveBytes = 64 << 20
		c.Limits.Timeout = 100 * time.Millisecond
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(conn, "GET "+ticketPath+" HTTP/1.1\r\nHost: local\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("slow reader kept upstream alive")
	}
	// The slot must be released even when the guest never reads the response.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for len(h.slots) != 0 {
		select {
		case <-deadline.C:
			t.Fatal("slow reader kept request slot")
		case <-tick.C:
		}
	}
}
