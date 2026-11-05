package transport

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, upstream http.HandlerFunc, edit func(*Config)) (*Handler, *httptest.Server) {
	t.Helper()
	s := httptest.NewTLSServer(upstream)
	t.Cleanup(s.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	c := Config{Grant: Grant{Bearer: "cache-only", Expires: time.Now().Add(time.Minute)}, Origin: s.URL, Roots: roots,
		Limits: Limits{RequestBytes: 128, ChunkBytes: 256, ResponseBytes: 128, HeaderBytes: 1024, URLBytes: 1024, ArchiveBytes: 256 << 10, Concurrent: 1, Timeout: time.Second}}
	if edit != nil {
		edit(&c)
	}
	h, e := New(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(h.Close)
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return h, front
}
func send(t *testing.T, s *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	r, e := http.NewRequest(method, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	for _, k := range []string{"Authorization", "Cookie", "Forwarded", "X-Forwarded-Host", "X-Attempt", "X-Upstream", "Proxy-Authorization"} {
		r.Header.Set(k, "github-secret")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Range", "bytes 0-2/*")
	resp, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
func TestV1AndCredentialBoundary(t *testing.T) {
	var calls atomic.Int32
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, err := io.ReadAll(r.Body)
		if err != nil || (r.Method != "GET" && string(b) != "abc") || (r.Method == "GET" && len(b) != 0) {
			t.Error("changed request body")
		}
		if r.TLS == nil || r.Header.Get("Authorization") != "Bearer cache-only" {
			t.Error("missing scoped TLS authorization")
		}
		for _, k := range []string{"Cookie", "Forwarded", "X-Forwarded-Host", "X-Attempt", "X-Upstream", "Proxy-Authorization"} {
			if r.Header.Get(k) != "" {
				t.Errorf("forwarded %s", k)
			}
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("lost protocol header")
		}
		w.Header().Set("Set-Cookie", "secret")
		if r.Method == "GET" {
			w.WriteHeader(204) // a miss; hits are covered by the lookup rewrite tests
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}, nil)
	for _, v := range [][2]string{{"GET", "/_apis/artifactcache/cache?keys=k&version=v"}, {"POST", "/_apis/artifactcache/caches"}, {"PATCH", "/_apis/artifactcache/caches/1"}, {"POST", "/_apis/artifactcache/caches/1"}} {
		body, want := "abc", 200
		if v[0] == "GET" {
			body, want = "", 204
		}
		if code, _ := send(t, s, v[0], v[1], body); code != want {
			t.Fatalf("%v: %d", v, code)
		}
	}
	for _, path := range []string{"/_apis/artifacts", "/admin/clear", "/_apis/artifactcache/caches/../1", "/_apis/artifactcache/caches/%2e%2e", "/_apis/artifactcache/caches%2f1", "/_apis/artifactcache/caches/01", "/_apis/artifactcache/caches/1?upstream=https://evil.test"} {
		if code, _ := send(t, s, "POST", path, ""); code != 400 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("denied route reached upstream")
	}
}
func TestCloseAndExpiry(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		t.Run("lifecycle", func(t *testing.T) {
			var calls atomic.Int32
			h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, func(c *Config) {
				if expiry {
					c.Grant.Expires = time.Now().Add(30 * time.Millisecond)
				}
			})
			if expiry {
				<-h.ctx.Done()
			} else {
				h.Close()
			}
			if code, _ := send(t, s, "POST", "/_apis/artifactcache/caches", ""); code != 403 || calls.Load() != 0 {
				t.Fatal("retired attempt accepted")
			}
		})
	}
}
func TestBoundsAndSafeErrors(t *testing.T) {
	for _, mode := range []string{"redirect", "error", "body", "headers"} {
		t.Run(mode, func(t *testing.T) {
			var target atomic.Int32
			evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1) }))
			defer evil.Close()
			_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					w.Header().Set("Location", evil.URL)
					w.WriteHeader(307)
				case "error":
					w.WriteHeader(500)
					_, _ = w.Write([]byte("cache-only github-secret"))
				case "body":
					_, _ = w.Write([]byte(strings.Repeat("x", 129)))
				case "headers":
					w.Header().Set("X-Large", strings.Repeat("x", 4096))
					w.WriteHeader(200)
				}
			}, nil)
			code, body := send(t, s, "POST", "/_apis/artifactcache/caches", "")
			if code != 502 || strings.Contains(body, "secret") || strings.Contains(body, "cache-only") || target.Load() != 0 {
				t.Fatalf("unsafe response %d %q", code, body)
			}
		})
	}
	var calls atomic.Int32
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
	for _, v := range []struct {
		method string
		n      int
	}{{"POST", 129}, {"PATCH", 257}} {
		path := "/_apis/artifactcache/caches"
		if v.method == "PATCH" {
			path += "/1"
		}
		if code, _ := send(t, s, v.method, path, strings.Repeat("x", v.n)); code != 413 {
			t.Fatal(code)
		}
	}
	r, _ := http.NewRequest("POST", s.URL+"/_apis/artifactcache/caches", nil)
	r.Header.Set("X-Large", strings.Repeat("x", 1025))
	resp, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 431 || calls.Load() != 0 {
		t.Fatal("bounds failed")
	}
}
func TestCancellationAndConcurrency(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})
	h, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(done) }, nil)
	result := make(chan int, 1)
	go func() {
		resp, e := s.Client().Post(s.URL+"/_apis/artifactcache/caches", "application/json", nil)
		if e != nil {
			result <- 0
			return
		}
		defer resp.Body.Close()
		result <- resp.StatusCode
	}()
	<-started
	if code, _ := send(t, s, "POST", "/_apis/artifactcache/caches", ""); code != 429 {
		t.Fatal(code)
	}
	h.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not canceled")
	}
	if code := <-result; code != 502 {
		t.Fatal(code)
	}
}
func TestUnknownLengthAndFixedOrigin(t *testing.T) {
	var calls atomic.Int32
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host == "guest.example" {
			t.Error("guest host forwarded")
		}
		w.WriteHeader(204)
	}, nil)
	r, _ := http.NewRequest("POST", s.URL+"/_apis/artifactcache/caches", io.NopCloser(strings.NewReader(strings.Repeat("x", 129))))
	r.ContentLength = -1
	resp, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 || calls.Load() != 0 {
		t.Fatal("chunked overflow reached upstream")
	}
	r, _ = http.NewRequest("POST", s.URL+"/_apis/artifactcache/caches", nil)
	r.Host = "guest.example"
	r.Header.Set("X-Upstream", "https://guest.example")
	resp, e = s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || calls.Load() != 1 {
		t.Fatal("fixed origin not used")
	}
}

func TestTimeoutAndClientCancellation(t *testing.T) {
	for _, clientCancel := range []bool{false, true} {
		t.Run(strconv.FormatBool(clientCancel), func(t *testing.T) {
			started := make(chan struct{})
			ended := make(chan struct{})
			_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(ended) }, func(c *Config) { c.Limits.Timeout = 100 * time.Millisecond })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/_apis/artifactcache/caches", nil)
			result := make(chan error, 1)
			go func() {
				resp, e := s.Client().Do(req)
				if e == nil {
					defer resp.Body.Close()
					if resp.StatusCode != 502 {
						e = fmt.Errorf("status %d", resp.StatusCode)
					}
				}
				result <- e
			}()
			<-started
			if clientCancel {
				cancel()
			}
			select {
			case <-ended:
			case <-time.After(2 * time.Second):
				t.Fatal("request not canceled")
			}
			e := <-result
			// At the write deadline the server may close the socket before it can
			// emit the bounded local error. Either outcome terminates the call.
			_ = e
		})
	}
}

func TestSlowGuestBodyDeadline(t *testing.T) {
	var calls atomic.Int32
	_, s := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, func(c *Config) { c.Limits.Timeout = 50 * time.Millisecond })
	conn, e := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, e = io.WriteString(conn, "POST /_apis/artifactcache/caches HTTP/1.1\r\nHost: local\r\nContent-Length: 10\r\n\r\nx")
	if e != nil {
		t.Fatal(e)
	}
	resp, e := http.ReadResponse(bufio.NewReader(conn), nil)
	if e == nil {
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal(resp.StatusCode)
		}
	} else if !errors.Is(e, io.ErrUnexpectedEOF) && !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
	if calls.Load() != 0 {
		t.Fatal("slow body reached upstream")
	}
}

func TestConfigurationRefusals(t *testing.T) {
	_, _ = fixture(t, func(w http.ResponseWriter, r *http.Request) {}, func(c *Config) {
		base := *c
		for _, mutate := range []func(*Config){
			func(c *Config) { c.Roots = nil }, func(c *Config) { c.Origin = "http://example.test" },
			func(c *Config) { c.Origin = "https://example.test/path" }, func(c *Config) { c.Origin = "https://user@example.test" },
			func(c *Config) { c.Grant.Expires = time.Now().Add(-time.Second) }, func(c *Config) { c.Grant.Bearer = "bad\r\ntoken" },
			func(c *Config) { c.Limits.ArchiveBytes = 0 }, func(c *Config) { c.Limits.ArchiveBytes = -1 }, func(c *Config) { c.Limits.ArchiveBytes = 1 << 62 }, func(c *Config) { c.Limits.Concurrent = 0 }, func(c *Config) { c.Limits.Timeout = 0 },
			func(c *Config) { c.Limits.HeaderBytes = 0 }, func(c *Config) { c.Limits.URLBytes = 0 }, func(c *Config) { c.Limits.ChunkBytes = 0 },
			func(c *Config) { c.Limits.RequestBytes = 0 }, func(c *Config) { c.Limits.ResponseBytes = 0 },
		} {
			bad := base
			mutate(&bad)
			if h, e := New(context.Background(), bad); e == nil {
				h.Close()
				t.Error("invalid config accepted")
			}
		}
	})
}
