// Package transport translates one guest's v1 cache API and archive reads to
// the regional cache-data service. The caller authenticates the guest before
// selecting a Handler; this package neither authenticates nor issues grants.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Grant is the signed cache bearer for one job and its expiry.
type Grant struct {
	Bearer  string
	Expires time.Time
}

// Limits are mandatory finite per-request bounds. ChunkBytes applies to PATCH;
// RequestBytes applies to reserve/finalize. ResponseBytes bounds buffered control
// responses. ArchiveBytes separately caps streamed downloads.
type Limits struct {
	RequestBytes, ChunkBytes, ResponseBytes, HeaderBytes int64
	ArchiveBytes                                         int64
	URLBytes, Concurrent                                 int
	Timeout                                              time.Duration
}

// Config names the cache-data origin and the CA that signs its certificate.
type Config struct {
	// BindTickets confines managed archive reads to this authenticated session.
	BindTickets bool
	Grant       Grant
	Origin      string
	Roots       *x509.CertPool // copied
	Limits      Limits
}

// Handler is scoped to one job's grant. Lookups are rewritten to the fixed
// guest relay. Close cancels active operations and refuses new ones; parent
// cancellation and grant expiry have the same effect. Close is idempotent.
type Handler struct {
	grant     Grant
	origin    url.URL
	limits    Limits
	ctx       context.Context
	cancel    context.CancelFunc
	transport *http.Transport
	slots     chan struct{}
	ticketsMu sync.Mutex
	tickets   map[string]time.Time
}

func New(parent context.Context, c Config) (*Handler, error) {
	u, err := url.Parse(c.Origin)
	l := c.Limits
	if parent == nil || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || c.Roots == nil || c.Grant.Bearer == "" || strings.ContainsAny(c.Grant.Bearer, " \t\r\n") || !c.Grant.Expires.After(time.Now()) || l.RequestBytes <= 0 || l.ChunkBytes <= 0 || l.ResponseBytes <= 0 || l.HeaderBytes <= 0 || l.URLBytes <= 0 || l.Concurrent <= 0 || l.Timeout <= 0 || l.RequestBytes >= 1<<62 || l.ChunkBytes >= 1<<62 || l.ResponseBytes >= 1<<62 || l.ArchiveBytes <= 0 || l.ArchiveBytes >= 1<<62 {
		return nil, errors.New("invalid cache transport configuration")
	}
	ctx, cancel := context.WithDeadline(parent, c.Grant.Expires)
	h := &Handler{grant: c.Grant, origin: *u, limits: l, ctx: ctx, cancel: cancel, slots: make(chan struct{}, l.Concurrent)}
	if c.BindTickets {
		h.tickets = map[string]time.Time{}
	}
	h.transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: l.Timeout}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.Roots.Clone()}, TLSHandshakeTimeout: l.Timeout, ResponseHeaderTimeout: l.Timeout, MaxResponseHeaderBytes: l.HeaderBytes, MaxConnsPerHost: l.Concurrent, MaxIdleConnsPerHost: l.Concurrent, IdleConnTimeout: l.Timeout, DisableCompression: true}
	return h, nil
}
func (h *Handler) Close() {
	h.cancel()
	h.transport.CloseIdleConnections()
}

func headerSize(h http.Header) int64 {
	var n int64
	for k, vs := range h {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v)) + 4
		}
	}
	return n
}

func allowed(r *http.Request) bool {
	u := r.URL
	if u == nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(u.Path, "%\\") {
		return false
	}
	const base = "/_apis/artifactcache/"
	if r.Method == http.MethodGet && u.Path == base+"cache" {
		q, e := url.ParseQuery(u.RawQuery)
		if e != nil {
			return false
		}
		for k, v := range q {
			if (k != "keys" && k != "version") || len(v) != 1 {
				return false
			}
		}
		return len(q["keys"]) == 1 && len(q["version"]) == 1
	}
	if u.RawQuery != "" || u.ForceQuery {
		return false
	}
	if r.Method == http.MethodPost && u.Path == base+"caches" {
		return true
	}
	id := strings.TrimPrefix(u.Path, base+"caches/")
	if id == u.Path || id == "" || id[0] == '0' {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, e := strconv.ParseInt(id, 10, 64)
	return e == nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch)
}

func fail(w http.ResponseWriter, code int) { http.Error(w, http.StatusText(code), code) }
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	download := (r.Method == http.MethodGet || r.Method == http.MethodHead) && downloadRoute(r.URL)
	if r.URL == nil || len(r.RequestURI) > h.limits.URLBytes || len(r.URL.String()) > h.limits.URLBytes || (!download && !allowed(r)) {
		fail(w, 400)
		return
	}
	if download && h.tickets != nil {
		h.ticketsMu.Lock()
		until, ok := h.tickets[r.URL.String()]
		h.ticketsMu.Unlock()
		if !ok || !time.Now().Before(until) {
			fail(w, http.StatusForbidden)
			return
		}
	}
	if download && (len(r.Header.Values("Range")) != 0 || len(r.Header.Values("If-Range")) != 0) {
		fail(w, 400)
		return
	}
	if headerSize(r.Header) > h.limits.HeaderBytes || int64(len(r.Host)) > h.limits.HeaderBytes-headerSize(r.Header) {
		fail(w, 431)
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, h.limits.Timeout)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	if h.ctx.Err() != nil || r.Context().Err() != nil {
		fail(w, 403)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		fail(w, 429)
		return
	}
	max := h.limits.RequestBytes
	if r.Method == http.MethodPatch {
		max = h.limits.ChunkBytes
	}
	if r.Method == http.MethodGet || download {
		max = 0
	}
	if r.ContentLength > max {
		fail(w, 413)
		return
	}
	// Bound guest body reads on a real HTTP connection as well as upstream IO.
	// A native caller using a custom ResponseWriter must provide equivalent
	// cancellation/deadlines for its transport (see README).
	rc := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	if h.grant.Expires.Before(deadline) {
		deadline = h.grant.Expires
	}
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	readStopped := make(chan struct{})
	stopRead := context.AfterFunc(ctx, func() {
		_ = rc.SetReadDeadline(time.Now())
		if download {
			_ = rc.SetWriteDeadline(time.Now())
		}
		close(readStopped)
	})
	defer func() {
		if !stopRead() {
			<-readStopped
		}
	}()
	// Buffer at most the configured chunk/control bound, detecting unknown-length
	// overflow before any upstream mutation. Never buffer an archive without limit.
	var body []byte
	var err error
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, max+1))
	}
	if err != nil {
		w.Header().Set("Connection", "close")
		fail(w, 400)
		return
	}
	_ = rc.SetReadDeadline(time.Time{})
	if int64(len(body)) > max {
		fail(w, 413)
		return
	}
	u := h.origin
	u.Path = r.URL.Path
	u.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		fail(w, 400)
		return
	}
	// Construct from scratch: no guest credentials, routing or forwarding headers.
	if !download {
		req.Header.Set("Authorization", "Bearer "+h.grant.Bearer)
		for _, k := range []string{"Accept", "Content-Type", "Content-Range"} {
			for _, v := range r.Header.Values(k) {
				req.Header.Add(k, v)
			}
		}
	}
	if ctx.Err() != nil || !time.Now().Before(h.grant.Expires) {
		fail(w, 403)
		return
	}
	// RoundTrip never follows redirects, and has no environment proxy.
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		fail(w, 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// Preserve client-visible denial, conflict and quota semantics without
		// exposing the upstream diagnostic body or credential-bearing headers.
		fail(w, resp.StatusCode)
		return
	}
	if resp.StatusCode >= 300 {
		fail(w, 502)
		return
	}
	if download {
		h.streamArchive(w, r, resp, ctx)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, h.limits.ResponseBytes+1))
	if err != nil || int64(len(data)) > h.limits.ResponseBytes || ctx.Err() != nil {
		fail(w, 502)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/_apis/artifactcache/cache" && resp.StatusCode != http.StatusNoContent {
		data, err = h.rewriteLookup(data)
		if resp.StatusCode != http.StatusOK || err != nil || int64(len(data)) > h.limits.ResponseBytes {
			fail(w, 502)
			return
		}
	}
	// Only protocol media type is returned; cookies/location/credentials and
	// arbitrary upstream diagnostics are never copied as response headers.
	if v := resp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

const archiveRelayOrigin = "http://127.0.0.1:18080"

func downloadRoute(u *url.URL) bool {
	if u == nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawPath != "" || u.ForceQuery {
		return false
	}
	if !strings.HasPrefix(u.Path, "/download/") {
		return false
	}
	id := strings.TrimPrefix(u.Path, "/download/")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id || len(u.RawQuery) != len("ticket=")+64 || !strings.HasPrefix(u.RawQuery, "ticket=") {
		return false
	}
	for _, c := range strings.TrimPrefix(u.RawQuery, "ticket=") {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (h *Handler) rewriteLookup(data []byte) ([]byte, error) {
	// Decode field-by-field so duplicate keys cannot hide an unvalidated URL.
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid cache lookup")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid cache lookup")
		}
		name, ok := key.(string)
		if !ok || fields[name] != nil {
			return nil, errors.New("invalid cache lookup field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("invalid cache lookup suffix")
	}
	var location string
	if err := json.Unmarshal(fields["archiveLocation"], &location); err != nil || len(location) > h.limits.URLBytes {
		return nil, errors.New("invalid archive location")
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme != h.origin.Scheme || u.Host != h.origin.Host || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("invalid archive origin")
	}
	u.Scheme, u.Host = "", ""
	if !downloadRoute(u) || location != h.origin.Scheme+"://"+h.origin.Host+u.String() {
		return nil, errors.New("invalid archive route")
	}
	local := archiveRelayOrigin + u.String()
	if len(local) > h.limits.URLBytes {
		return nil, errors.New("archive relay URL exceeds limit")
	}
	if h.tickets != nil {
		h.ticketsMu.Lock()
		now := time.Now()
		for ticket, until := range h.tickets {
			if !now.Before(until) {
				delete(h.tickets, ticket)
			}
		}
		if len(h.tickets) >= 1024 {
			h.ticketsMu.Unlock()
			return nil, errors.New("archive ticket limit")
		}
		h.tickets[u.String()] = minTime(now.Add(time.Minute), h.grant.Expires)
		h.ticketsMu.Unlock()
	}
	fields["archiveLocation"], err = json.Marshal(local)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (h *Handler) streamArchive(w http.ResponseWriter, r *http.Request, resp *http.Response, ctx context.Context) {
	if resp.StatusCode != http.StatusOK || resp.ContentLength > h.limits.ArchiveBytes || resp.Header.Get("Content-Encoding") != "" || ctx.Err() != nil {
		fail(w, 502)
		return
	}
	for _, key := range []string{"Content-Type", "Content-Length"} {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	buffer := make([]byte, 64<<10)
	// Hide ReaderFrom so CopyBuffer always uses this bounded application buffer.
	written, err := io.CopyBuffer(struct{ io.Writer }{w}, io.LimitReader(resp.Body, h.limits.ArchiveBytes), buffer)
	if err == nil {
		var extra [1]byte
		n, readErr := resp.Body.Read(extra[:])
		if n != 0 || readErr != io.EOF {
			err = errors.New("archive did not finish within limit")
		}
	}
	if err != nil || ctx.Err() != nil || (resp.ContentLength >= 0 && written != resp.ContentLength) {
		// After committing headers, only an aborted stream can report failure.
		// A local HTTP error here would become part of the downloaded archive.
		panic(http.ErrAbortHandler)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
