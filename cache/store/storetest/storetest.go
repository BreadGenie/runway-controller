// Package storetest runs a real cache store for tests: an in-memory Garage
// (S3) endpoint, generated grant keys and a TLS listener on the store's origin.
package storetest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/store"
)

// Garage is an in-memory S3 endpoint. Hold Mu while changing its fields.
type Garage struct {
	*httptest.Server
	Mu       sync.Mutex
	Objects  map[string][]byte
	Puts     int
	Requests map[string]int // by method, including failed ones
	Fail     bool           // answer every request with 503
	PutDelay time.Duration  // wait before reading a PUT body
	OnPut    func()         // called after a PUT is stored
	Gate     chan struct{}  // when set, GETs of GateKey (or all, if empty) stall after the first byte
	GateKey  string
}

func NewGarage(t testing.TB) *Garage {
	t.Helper()
	g := &Garage{Objects: map[string][]byte{}, Requests: map[string]int{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Server.Close)
	return g
}

func (g *Garage) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	g.Mu.Lock()
	g.Requests[r.Method]++
	if g.Fail {
		g.Mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodPut:
		delay, hook := g.PutDelay, g.OnPut
		g.Mu.Unlock()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		select { // after the body, so a client timeout is seen as a disconnect
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		g.Mu.Lock()
		g.Objects[r.URL.Path] = data
		g.Puts++
		g.Mu.Unlock()
		if hook != nil {
			hook()
		}
		w.Header().Set("ETag", `"fixture"`)
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			type item struct{ Key string }
			result := struct {
				XMLName     xml.Name
				Contents    []item
				IsTruncated bool
			}{XMLName: xml.Name{Local: "ListBucketResult"}}
			for key := range g.Objects {
				if strings.HasPrefix(key, r.URL.Path) {
					object := strings.TrimPrefix(strings.TrimPrefix(key, r.URL.Path), "/")
					if strings.HasPrefix(object, r.URL.Query().Get("prefix")) {
						result.Contents = append(result.Contents, item{object})
					}
				}
			}
			g.Mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(result)
			return
		}
		data, ok := g.Objects[r.URL.Path]
		gate := g.Gate
		if g.GateKey != "" && !strings.HasSuffix(r.URL.Path, "/"+g.GateKey) {
			gate = nil
		}
		g.Mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if gate != nil && len(data) > 0 {
			_, _ = w.Write(data[:1])
			w.(http.Flusher).Flush()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			data = data[1:]
		}
		_, _ = w.Write(data)
	case http.MethodDelete:
		delete(g.Objects, r.URL.Path)
		g.Mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		g.Mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// Set replaces every stored object's bytes (corruption fixtures).
func (g *Garage) Set(data []byte) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	for key := range g.Objects {
		g.Objects[key] = data
	}
}

func (g *Garage) Config() store.GarageConfig {
	return store.GarageConfig{Endpoint: g.URL, Region: "garage", Bucket: "cache", AccessKey: "test-access", SecretKey: "test-secret"}
}

// Signer issues grants for one region and origin.
type Signer struct {
	Key      ed25519.PrivateKey
	Verifier *grant.Verifier
	Origin   string
	Region   string
}

func NewSigner(t testing.TB, origin string) *Signer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := grant.NewVerifier(map[string]ed25519.PublicKey{grant.KeyID(public): public}, origin, "region")
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{Key: private, Verifier: verifier, Origin: origin, Region: "region"}
}

// Job names a grant. Empty fields take defaults; Write "" makes it read-only.
type Job struct {
	TenantID     int64
	JobID        string
	RepositoryID int64
	Read         []string
	Write        string
	Lifetime     time.Duration
	Now          time.Time
}

const DefaultJob = "afc2e663-0d50-409b-bf27-2254fcf2fe97"

func (signer *Signer) Grant(t testing.TB, job Job) string {
	t.Helper()
	if job.JobID == "" {
		job.JobID = DefaultJob
	}
	if job.RepositoryID == 0 {
		job.RepositoryID = 1
	}
	if job.Read == nil {
		job.Read = []string{"refs/heads/main"}
	}
	if job.Lifetime == 0 {
		job.Lifetime = 10 * time.Minute
	}
	if job.Now.IsZero() {
		job.Now = time.Now()
	}
	clock := grant.Clock{Earliest: job.Now, Latest: job.Now}
	token, err := grant.Issue(signer.Key, signer.Origin, grant.Authorization{
		TenantID: job.TenantID, RegionID: signer.Region, JobID: job.JobID, RunID: 12, RunAttempt: 1, RepositoryID: job.RepositoryID,
		ReadRefs: job.Read, WriteRef: job.Write, Deadline: job.Now.Add(job.Lifetime),
	}, clock)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Writer and Reader are the usual main-branch grants of one repository.
func (signer *Signer) Writer(t testing.TB) string {
	return signer.Grant(t, Job{Write: "refs/heads/main"})
}

func (signer *Signer) Reader(t testing.TB) string {
	return signer.Grant(t, Job{JobID: "b2b00953-9291-4411-8536-348002a08a87"})
}

// Clock is a settable test clock; zero Offset is wall time.
type Clock struct {
	Mu     sync.Mutex
	Offset time.Duration
	Err    error
}

func (c *Clock) Observe(context.Context) (grant.Clock, error) {
	c.Mu.Lock()
	defer c.Mu.Unlock()
	if c.Err != nil {
		return grant.Clock{}, c.Err
	}
	now := time.Now().Add(c.Offset)
	return grant.Clock{Earliest: now, Latest: now}, nil
}

func (c *Clock) Advance(d time.Duration) {
	c.Mu.Lock()
	c.Offset += d
	c.Mu.Unlock()
}

// Service is a running store behind TLS on its configured origin.
type Service struct {
	*store.Server
	Config store.Config
	HTTP   *httptest.Server
	Client *http.Client
	Garage *Garage
	Signer *Signer
	Clock  *Clock
}

// Start runs a store with 1 KiB archives, a 4 KiB budget and the given options.
func Start(t testing.TB, options ...func(*store.Config)) *Service {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://" + listener.Addr().String()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	service := &Service{Garage: NewGarage(t), Signer: NewSigner(t, origin), Clock: &Clock{}}
	service.Config = store.Config{
		PublicOrigin: origin, Verifier: service.Signer.Verifier, Clock: service.Clock.Observe, StateDir: state,
		Disk:       store.DiskConfig{Directory: filepath.Join(t.TempDir(), "spools"), PhysicalBytes: 1 << 20},
		Garage:     service.Garage.Config(),
		QuotaBytes: 4096, TotalBytes: 4096, MaxArchiveBytes: 1024,
	}
	for _, option := range options {
		option(&service.Config)
	}
	service.HTTP = httptest.NewUnstartedServer(nil)
	service.HTTP.Config.ErrorLog = log.New(io.Discard, "", 0)
	service.HTTP.Listener.Close()
	service.HTTP.Listener = listener
	service.HTTP.StartTLS()
	roots := x509.NewCertPool()
	roots.AddCert(service.HTTP.Certificate())
	service.Client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	t.Cleanup(service.HTTP.Close)
	service.Restart(t)
	return service
}

// Restart closes the store (if running) and opens a new one on the same state.
func (service *Service) Restart(t testing.TB) {
	t.Helper()
	if service.Server != nil {
		if err := service.Server.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	server, err := store.New(service.Config)
	if err != nil {
		t.Fatal(err)
	}
	service.Server = server
	service.HTTP.Config.Handler = server
	t.Cleanup(func() { server.Close(context.Background()) })
}

// Do sends one request to the origin; token "" sends no authorization.
func (service *Service) Do(t testing.TB, method, path, token, contentRange string, body []byte) (int, []byte) {
	t.Helper()
	if !strings.HasPrefix(path, "https://") {
		path = service.Config.PublicOrigin + path
	}
	request, err := http.NewRequest(method, path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if contentRange != "" {
		request.Header.Set("Content-Range", contentRange)
	}
	response, err := service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

// Reserve returns the status and cache ID.
func (service *Service) Reserve(t testing.TB, token, key string, size int) (int, int64) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"key": key, "version": "v1", "cacheSize": size})
	status, data := service.Do(t, "POST", "/_apis/artifactcache/caches", token, "", body)
	var reply struct {
		CacheID int64 `json:"cacheId"`
	}
	if status == http.StatusCreated && json.Unmarshal(data, &reply) != nil {
		t.Fatal("malformed reservation")
	}
	return status, reply.CacheID
}

// Upload writes data as one chunk and returns the finalize status.
func (service *Service) Upload(t testing.TB, token string, id int64, data string) int {
	t.Helper()
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", id)
	if status, _ := service.Do(t, "PATCH", path, token, fmt.Sprintf("bytes 0-%d/*", len(data)-1), []byte(data)); status != http.StatusNoContent {
		t.Fatalf("chunk upload: %d", status)
	}
	status, _ := service.Do(t, "POST", path, token, "", []byte(fmt.Sprintf(`{"size":%d}`, len(data))))
	return status
}

// Save reserves, uploads and publishes key, failing the test otherwise.
func (service *Service) Save(t testing.TB, token, key, data string) int64 {
	t.Helper()
	status, id := service.Reserve(t, token, key, len(data))
	if status != http.StatusCreated {
		t.Fatalf("reserve %s: %d", key, status)
	}
	if status := service.Upload(t, token, id, data); status != http.StatusNoContent {
		t.Fatalf("finalize %s: %d", key, status)
	}
	return id
}

// Lookup returns the status and archive location for comma-separated keys.
func (service *Service) Lookup(t testing.TB, token, keys string) (int, string) {
	t.Helper()
	status, data := service.Do(t, "GET", "/_apis/artifactcache/cache?version=v1&keys="+keys, token, "", nil)
	var reply struct {
		Location string `json:"archiveLocation"`
	}
	if status == http.StatusOK && json.Unmarshal(data, &reply) != nil {
		t.Fatal("malformed lookup")
	}
	return status, reply.Location
}

// Restore looks key up and downloads it.
func (service *Service) Restore(t testing.TB, token, key string) (int, string) {
	t.Helper()
	status, location := service.Lookup(t, token, key)
	if status != http.StatusOK {
		return status, ""
	}
	status, data := service.Do(t, "GET", location, "", "", nil)
	return status, string(data)
}
