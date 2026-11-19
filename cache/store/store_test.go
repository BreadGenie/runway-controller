package store_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BreadGenie/runway-controller/cache/store"
	"github.com/BreadGenie/runway-controller/cache/store/storetest"
)

func TestSaveRestoreRefsAndIsolation(t *testing.T) {
	service := storetest.Start(t)
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	if status, _ := service.Lookup(t, writer, "deps"); status != 204 {
		t.Fatal("empty cache hit", status)
	}
	service.Save(t, writer, "deps-linux-1", "main")
	if status, data := service.Restore(t, reader, "deps-linux-1"); status != 200 || data != "main" {
		t.Fatal("exact restore", status, data)
	}
	// Restore keys: a prefix matches the newest entry; an exact key wins.
	service.Save(t, writer, "deps-linux-2", "newer")
	if _, data := service.Restore(t, reader, "deps-linux"); data != "newer" {
		t.Fatal("prefix did not select the newest entry", data)
	}
	if _, data := service.Restore(t, reader, "deps-linux-1,deps-linux"); data != "main" {
		t.Fatal("exact key lost to a prefix", data)
	}
	// A feature branch reads its own ref first, then the default branch.
	feature := service.Signer.Grant(t, storetest.Job{Read: []string{"refs/heads/feature", "refs/heads/main"}, Write: "refs/heads/feature"})
	if _, data := service.Restore(t, feature, "deps-linux-1"); data != "main" {
		t.Fatal("default-branch fallback", data)
	}
	service.Save(t, feature, "deps-linux-1", "feature")
	if _, data := service.Restore(t, feature, "deps-linux-1"); data != "feature" {
		t.Fatal("own ref not preferred", data)
	}
	if _, data := service.Restore(t, reader, "deps-linux-1"); data != "main" {
		t.Fatal("feature write visible to main", data)
	}
	// Versions and repositories are separate namespaces.
	if status, _ := service.Do(t, "GET", "/_apis/artifactcache/cache?version=v2&keys=deps-linux-1", reader, "", nil); status != 204 {
		t.Fatal("version namespace crossed", status)
	}
	other := service.Signer.Grant(t, storetest.Job{RepositoryID: 2, Write: "refs/heads/main"})
	if status, _ := service.Lookup(t, other, "deps"); status != 204 {
		t.Fatal("repository isolation", status)
	}
	if status, _ := service.Reserve(t, reader, "read-only", 4); status != 403 {
		t.Fatal("read-only grant reserved", status)
	}
	if hits, misses := service.LookupCounts(); hits != 6 || misses != 3 {
		t.Fatalf("lookup counters: got %d hits, %d misses", hits, misses)
	}
}

func TestReservationUploadAndFinalizeContracts(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Writer(t)
	status, id := service.Reserve(t, writer, "key", 8)
	if status != 201 {
		t.Fatal(status)
	}
	if status, _ := service.Reserve(t, writer, "key", 8); status != 409 {
		t.Fatal("second writer reserved the same key", status)
	}
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", id)
	sibling := service.Signer.Grant(t, storetest.Job{JobID: "11111111-2222-4333-8444-555555555555", Write: "refs/heads/main"})
	for _, step := range []struct {
		name, token, contentRange, body string
		want                            int
	}{
		{"other job", sibling, "bytes 0-3/*", "abcd", 403},
		{"past end", writer, "bytes 6-9/*", "abcd", 400},
		{"short body", writer, "bytes 0-3/*", "abc", 400},
		{"malformed range", writer, "bytes=0-3", "abcd", 400},
		{"first half", writer, "bytes 0-3/*", "abcd", 204},
		{"identical retry", writer, "bytes 0-3/*", "abcd", 204},
		{"changed retry", writer, "bytes 0-3/*", "abce", 409},
		{"overlap", writer, "bytes 2-5/*", "cdef", 409},
	} {
		if status, _ := service.Do(t, "PATCH", path, step.token, step.contentRange, []byte(step.body)); status != step.want {
			t.Errorf("%s: %d, want %d", step.name, status, step.want)
		}
	}
	if status, _ := service.Do(t, "POST", path, writer, "", []byte(`{"size":8}`)); status != 409 {
		t.Fatal("incomplete upload finalized", status)
	}
	if status, _ := service.Do(t, "POST", path, writer, "", []byte(`{"size":7}`)); status != 409 {
		t.Fatal("size mismatch finalized", status)
	}
	service.Do(t, "PATCH", path, writer, "bytes 4-7/*", []byte("efgh"))
	for range 2 { // a retried finalize after success is idempotent
		if status, _ := service.Do(t, "POST", path, writer, "", []byte(`{"size":8}`)); status != 204 {
			t.Fatal("finalize", status)
		}
	}
	if status, _ := service.Do(t, "PATCH", path, writer, "bytes 0-3/*", []byte("abcd")); status != 409 {
		t.Fatal("published entry accepted a chunk", status)
	}
	if _, data := service.Restore(t, writer, "key"); data != "abcdefgh" {
		t.Fatal("restored bytes", data)
	}
}

func TestConcurrentReservationFirstWriterWins(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Writer(t)
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := service.Reserve(t, writer, "race", 4)
			statuses <- status
		}()
	}
	wg.Wait()
	close(statuses)
	created := 0
	for status := range statuses {
		if status == 201 {
			created++
		} else if status != 409 {
			t.Fatal("unexpected status", status)
		}
	}
	if created != 1 {
		t.Fatal("reservations created:", created)
	}
}

func TestQuotaEvictsLeastRecentlyUsedButNotRead(t *testing.T) {
	service := storetest.Start(t, func(c *store.Config) { c.QuotaBytes, c.TotalBytes = 2048, 2048 })
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	kib := strings.Repeat("x", 1024)
	service.Save(t, writer, "a", kib)
	service.Save(t, writer, "b", kib)
	// A live read ticket protects "a"; "b" is the only eviction candidate.
	if status, _ := service.Lookup(t, reader, "a"); status != 200 {
		t.Fatal(status)
	}
	service.Save(t, writer, "c", kib)
	if status, _ := service.Lookup(t, reader, "b"); status != 204 {
		t.Fatal("least recently used entry survived", status)
	}
	if status, data := service.Restore(t, reader, "a"); status != 200 || data != kib {
		t.Fatal("entry with a live ticket was evicted", status)
	}
	if status, _ := service.Reserve(t, writer, "too-big", 1025); status != 400 {
		t.Fatal("archive cap ignored", status)
	}
}

// Bug: a published entry kept its upload spool charged against the physical
// budget, so a few saves exhausted staging and every request returned 503.
func TestPublicationReleasesUploadStaging(t *testing.T) {
	service := storetest.Start(t, func(c *store.Config) {
		c.Disk.PhysicalBytes, c.QuotaBytes, c.TotalBytes = 8192, 4<<20, 4<<20
	})
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	for i := range 13 {
		service.Save(t, writer, fmt.Sprint("archive-", i), strings.Repeat("y", 1024))
	}
	if charged := service.Charged(); charged != 0 {
		t.Fatal("published uploads still charged:", charged)
	}
	if status, data := service.Restore(t, reader, "archive-0"); status != 200 || len(data) != 1024 {
		t.Fatal("download after many saves", status)
	}
}

// Bug: the index lock was held across Garage I/O and streaming, so one slow
// download serialized every other request.
func TestSlowDownloadDoesNotBlockOtherRequests(t *testing.T) {
	service := storetest.Start(t)
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	slowID := service.Save(t, writer, "slow", "slow-bytes")
	service.Save(t, writer, "fast", "fast-bytes")
	_, slow := service.Lookup(t, reader, "slow")
	gate := make(chan struct{})
	service.Garage.Mu.Lock()
	service.Garage.Gate, service.Garage.GateKey = gate, fmt.Sprint(slowID)
	service.Garage.Mu.Unlock()
	done := make(chan string)
	go func() {
		status, data := service.Do(t, "GET", slow, reader, "", nil)
		done <- fmt.Sprint(status, " ", string(data))
	}()
	waitFor(t, func() bool {
		service.Garage.Mu.Lock()
		defer service.Garage.Mu.Unlock()
		return service.Garage.Requests["GET"] >= 1
	})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if status, data := service.Restore(t, reader, "fast"); status != 200 || data != "fast-bytes" {
			t.Error("concurrent download", status, data)
		}
		if status, _ := service.Lookup(t, reader, "slow"); status != 200 {
			t.Error("concurrent lookup", status)
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled download blocked other requests")
	}
	// The slow read passed its ticket check before fetching. Idle expiry and
	// a sweep while it streams must not remove the pinned entry.
	service.Clock.Advance(8 * 24 * time.Hour)
	later := service.Signer.Grant(t, storetest.Job{Now: time.Now().Add(8 * 24 * time.Hour)})
	if status, _ := service.Lookup(t, later, "fast"); status != 204 {
		t.Fatal("idle entry survived", status)
	}
	service.SweepNow()
	close(gate)
	if got := <-done; got != "200 slow-bytes" {
		t.Fatal("pinned download", got)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Bug: a reservation lived a fixed 60 s, including hashing and the PUT.
func TestReservationLifetimeScalesWithStorageBudget(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Grant(t, storetest.Job{Write: "refs/heads/main", Lifetime: time.Hour})
	status, id := service.Reserve(t, writer, "slow-upload", 1024)
	if status != 201 {
		t.Fatal(status)
	}
	service.Clock.Advance(80 * time.Second) // past the old fixed minute
	if status := service.Upload(t, writer, id, strings.Repeat("z", 1024)); status != 204 {
		t.Fatal("reservation expired before its scaled window", status)
	}
	status, id = service.Reserve(t, writer, "abandoned", 1024)
	if status != 201 {
		t.Fatal(status)
	}
	service.Clock.Advance(90 * time.Second)
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", id)
	if status, _ := service.Do(t, "PATCH", path, writer, "bytes 0-0/*", []byte("z")); status != 410 {
		t.Fatal("abandoned reservation survived", status)
	}
}

func TestHiddenEntryCleanupBacksOffDuringOutage(t *testing.T) {
	service := storetest.Start(t, func(c *store.Config) { c.QuotaBytes, c.TotalBytes = 1024, 1024 })
	writer := service.Signer.Writer(t)
	service.Save(t, writer, "old", strings.Repeat("o", 1024))
	service.Garage.Mu.Lock()
	service.Garage.Fail = true
	service.Garage.Mu.Unlock()
	// Eviction hides "old"; its deletion fails, so the reservation is refused.
	if status, _ := service.Reserve(t, writer, "new", 1024); status != 409 {
		t.Fatal("reserved without freeing space", status)
	}
	for range 20 {
		if status, _ := service.Lookup(t, writer, "old"); status != 204 {
			t.Fatal("lookup during outage", status)
		}
	}
	service.Garage.Mu.Lock()
	attempts := service.Garage.Requests["DELETE"]
	service.Garage.Fail = false
	service.Garage.Mu.Unlock()
	if attempts < 1 || attempts > 3 {
		t.Fatal("DELETE attempts during backoff:", attempts)
	}
	service.SweepNow()
	if status, _ := service.Reserve(t, writer, "new", 1024); status != 201 {
		t.Fatal("space not recovered after outage", status)
	}
}

func TestGrantExpiryDuringStorageNeverPublishes(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Grant(t, storetest.Job{Write: "refs/heads/main", Lifetime: 30 * time.Second})
	service.Garage.Mu.Lock()
	service.Garage.OnPut = func() { service.Clock.Advance(time.Minute) }
	service.Garage.Mu.Unlock()
	status, id := service.Reserve(t, writer, "expires", 4)
	if status != 201 {
		t.Fatal(status)
	}
	if status := service.Upload(t, writer, id, "data"); status != 401 {
		t.Fatal("expired write published", status)
	}
	reader := service.Signer.Grant(t, storetest.Job{Now: time.Now().Add(time.Minute)})
	if status, _ := service.Lookup(t, reader, "expires"); status != 204 {
		t.Fatal("expired publication visible", status)
	}
	service.SweepNow()
	service.Garage.Mu.Lock()
	defer service.Garage.Mu.Unlock()
	if len(service.Garage.Objects) != 0 {
		t.Fatal("unpublished object not deleted")
	}
}

func TestStorageDeadlineScalesButStallTimesOut(t *testing.T) {
	budget := store.StorageBudget{BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: 3, RecoveryMaxSeconds: 3}
	service := storetest.Start(t, func(c *store.Config) {
		c.StorageBudget, c.MaxArchiveBytes, c.QuotaBytes, c.TotalBytes, c.Disk.PhysicalBytes = &budget, 2<<20, 8<<20, 8<<20, 8<<20
	})
	writer := service.Signer.Writer(t)
	data := strings.Repeat("s", 2<<20) // 1 s base + 2 s transfer budget
	service.Garage.Mu.Lock()
	service.Garage.PutDelay = 1600 * time.Millisecond // past a fixed 1 s budget
	service.Garage.Mu.Unlock()
	service.Save(t, writer, "large", data)
	service.Garage.Mu.Lock()
	service.Garage.PutDelay = time.Minute
	service.Garage.Mu.Unlock()
	_, id := service.Reserve(t, writer, "stalled", len(data))
	started := time.Now()
	if status := service.Upload(t, writer, id, data); status != 503 || time.Since(started) < 2500*time.Millisecond || time.Since(started) > 6*time.Second {
		t.Fatal("stalled finalize", status, time.Since(started))
	}
}

func TestReadTicketsAndClock(t *testing.T) {
	service := storetest.Start(t)
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	firstID := service.Save(t, writer, "first", "one")
	secondID := service.Save(t, writer, "second", "two")
	_, location := service.Lookup(t, reader, "second")
	if status, _ := service.Do(t, "GET", strings.Replace(location, fmt.Sprintf("/download/%d?", secondID), fmt.Sprintf("/download/%d?", firstID), 1), reader, "", nil); status != 404 {
		t.Fatal("ticket read another entry", status)
	}
	if status, _ := service.Do(t, "GET", location+"0", reader, "", nil); status != 404 {
		t.Fatal("altered ticket accepted", status)
	}
	if status, _ := service.Do(t, "GET", location, "", "", nil); status != 404 {
		t.Fatal("bearerless ticket redeem accepted", status)
	}
	service.Clock.Mu.Lock()
	service.Clock.Err = fmt.Errorf("unsynchronized")
	service.Clock.Mu.Unlock()
	if status, _ := service.Do(t, "GET", location, reader, "", nil); status != 503 {
		t.Fatal("download bypassed clock refusal", status)
	}
	service.Clock.Mu.Lock()
	service.Clock.Err = nil
	service.Clock.Mu.Unlock()
	service.Clock.Advance(61 * time.Second)
	if status, _ := service.Do(t, "GET", location, reader, "", nil); status != 404 {
		t.Fatal("ticket outlived a minute", status)
	}
	// A ticket never outlives its grant.
	short := service.Signer.Grant(t, storetest.Job{Lifetime: 10 * time.Second, Now: time.Now().Add(61 * time.Second)})
	_, location = service.Lookup(t, short, "second")
	service.Clock.Advance(11 * time.Second)
	if status, _ := service.Do(t, "GET", location, short, "", nil); status != 404 {
		t.Fatal("ticket outlived its grant", status)
	}
}

// Tickets are short-lived, so journals written before tenant binding carry no
// tenant and are dropped on recovery: an old-format ticket never redeems
// again, while entries keep serving under fresh tickets.
func TestOldFormatTicketRefusedAfterUpgrade(t *testing.T) {
	service := storetest.Start(t)
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	service.Save(t, writer, "legacy", "legacy-bytes")
	_, location := service.Lookup(t, reader, "legacy")
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Server = nil
	path := filepath.Join(service.Config.StateDir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	tickets, _ := state["Tickets"].(map[string]any)
	if len(tickets) == 0 {
		t.Fatal("journal ticket fixture")
	}
	for _, ticket := range tickets {
		delete(ticket.(map[string]any), "Tenant")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	service.Restart(t)
	if status, _ := service.Do(t, "GET", location, reader, "", nil); status != 404 {
		t.Fatal("old-format ticket redeemed", status)
	}
	if status, data := service.Restore(t, reader, "legacy"); status != 200 || data != "legacy-bytes" {
		t.Fatal("fresh ticket after upgrade", status, data)
	}
}

func TestRefusesUnauthenticatedMalformedAndForeignRequests(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Writer(t)
	other := storetest.NewSigner(t, service.Config.PublicOrigin).Writer(t)
	expired := service.Signer.Grant(t, storetest.Job{Write: "refs/heads/main", Lifetime: time.Second, Now: time.Now().Add(-time.Minute)})
	for name, token := range map[string]string{"none": "", "garbage": "x", "altered": writer + "x", "other key": other, "expired": expired} {
		if status, _ := service.Lookup(t, token, "k"); status != 401 {
			t.Errorf("%s token: %d", name, status)
		}
	}
	longest := strings.Repeat("é", 512)
	for name, path := range map[string]string{
		"too long key": "/_apis/artifactcache/cache?version=v1&keys=" + longest + "x",
		"no version":   "/_apis/artifactcache/cache?keys=k",
		"many keys":    "/_apis/artifactcache/cache?version=v1&keys=" + strings.Repeat("k,", 10) + "k",
	} {
		if status, _ := service.Do(t, "GET", path, writer, "", nil); status != 400 {
			t.Errorf("%s: %d", name, status)
		}
	}
	if status, _ := service.Lookup(t, writer, longest); status != 204 {
		t.Fatal("pinned client's longest key refused", status)
	}
	for name, body := range map[string]string{"unknown field": `{"key":"k","version":"v1","cacheSize":1,"admin":true}`, "trailing": `{"key":"k","version":"v1","cacheSize":1}{}`, "zero size": `{"key":"k","version":"v1","cacheSize":0}`} {
		if status, _ := service.Do(t, "POST", "/_apis/artifactcache/caches", writer, "", []byte(body)); status != 400 {
			t.Errorf("%s: %d", name, status)
		}
	}
	if status, _ := service.Do(t, "POST", "/_prototype/clear", writer, "", nil); status != 404 {
		t.Fatal("administration route exists", status)
	}
	// Only TLS requests for the configured origin are served; forwarded headers select nothing.
	request := httptest.NewRequest("GET", service.Config.PublicOrigin+"/_apis/artifactcache/cache?version=v1&keys=k", nil)
	request.Header.Set("Authorization", "Bearer "+writer)
	for name, change := range map[string]func(*http.Request){
		"plaintext":    func(r *http.Request) { r.TLS = nil },
		"foreign host": func(r *http.Request) { r.Host = "cache.example" },
		"forwarded": func(r *http.Request) {
			r.Host = "cache.example"
			r.Header.Set("X-Forwarded-Host", service.HTTP.Listener.Addr().String())
		},
	} {
		bad := request.Clone(t.Context())
		bad.TLS = &tls.ConnectionState{}
		change(bad)
		recorder := httptest.NewRecorder()
		service.ServeHTTP(recorder, bad)
		if recorder.Code != 400 {
			t.Errorf("%s: %d", name, recorder.Code)
		}
	}
}

func TestRestartRecoversVerifiesAndHidesLostObjects(t *testing.T) {
	service := storetest.Start(t)
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	service.Save(t, writer, "kept", "kept-bytes")
	lostID := service.Save(t, writer, "lost", "lost-bytes")
	service.Save(t, writer, "corrupt", "good-bytes")
	_, location := service.Lookup(t, reader, "kept")
	service.Garage.Mu.Lock()
	for key := range service.Garage.Objects {
		if strings.HasSuffix(key, fmt.Sprint("/", lostID)) {
			delete(service.Garage.Objects, key)
		} else if strings.HasSuffix(key, fmt.Sprint("/", lostID+1)) {
			service.Garage.Objects[key] = []byte("bad-bytes!")
		}
	}
	service.Garage.Mu.Unlock()
	service.Restart(t)
	if status, data := service.Do(t, "GET", location, reader, "", nil); status != 200 || string(data) != "kept-bytes" {
		t.Fatal("persisted ticket or entry lost", status, string(data))
	}
	for _, key := range []string{"lost", "corrupt"} {
		if status, _ := service.Lookup(t, reader, key); status != 204 {
			t.Fatal("lost or corrupt object served", key, status)
		}
	}
	// Corruption after startup is caught at download, before any byte is served.
	service.Garage.Set([]byte("BAD-BYTES!"))
	if status, data := service.Restore(t, reader, "kept"); status != 503 || data == "BAD-BYTES!" {
		t.Fatal("same-length corrupt archive served", status)
	}
	// A Garage outage at startup refuses to start and keeps the journal.
	service.Save(t, writer, "fresh", "fresh")
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Garage.Mu.Lock()
	service.Garage.Fail = true
	service.Garage.Mu.Unlock()
	if other, err := store.New(service.Config); err == nil {
		other.Close(context.Background())
		t.Fatal("started without verifying indexed objects")
	}
	service.Garage.Mu.Lock()
	service.Garage.Fail = false
	service.Garage.Mu.Unlock()
	service.Restart(t)
	if status, data := service.Restore(t, reader, "fresh"); status != 200 || data != "fresh" {
		t.Fatal("journal lost across a refused start", status)
	}
}

func TestDurableOwnershipAndMalformedState(t *testing.T) {
	service := storetest.Start(t)
	if other, err := store.New(service.Config); err == nil {
		other.Close(context.Background())
		t.Fatal("second owner accepted")
	}
	moved := service.Config
	moved.PublicOrigin = "https://cache.example"
	moved.Verifier = storetest.NewSigner(t, moved.PublicOrigin).Verifier
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if other, err := store.New(moved); err == nil {
		other.Close(context.Background())
		t.Fatal("journal rebound to another origin")
	}
	moved = service.Config
	moved.Garage.Bucket = "other"
	if other, err := store.New(moved); err == nil {
		other.Close(context.Background())
		t.Fatal("journal rebound to another bucket")
	}
	for _, state := range []string{`{"Version":1}`, `{`, `[]`} {
		if err := os.WriteFile(filepath.Join(service.Config.StateDir, "state.json"), []byte(state), 0600); err != nil {
			t.Fatal(err)
		}
		if other, err := store.New(service.Config); err == nil {
			other.Close(context.Background())
			t.Fatal("malformed state accepted", state)
		}
	}
}

// The journal on blr-1 predates this package: it carries spool, generation and
// PUT-intent fields. It must still load and serve its entries.
func TestLegacyJournalLoads(t *testing.T) {
	service := storetest.Start(t)
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	service.Save(t, writer, "legacy", "legacy-bytes")
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(service.Config.StateDir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(data), `"Version":1`, `"Spools":[{"Name":".archive-00000000000000000000000000000000","Size":12,"Charge":4096,"Device":1,"Inode":2}],"Version":1,"Generations":{}`, 1)
	legacy = strings.Replace(legacy, `"Published":true`, `"Generation":0,"Published":true,"PutConsumed":true,"SpoolName":".archive-00000000000000000000000000000000","PhysicalCharge":4096`, 1)
	if legacy == string(data) || os.WriteFile(path, []byte(legacy), 0600) != nil {
		t.Fatal("legacy fixture")
	}
	if err := os.WriteFile(filepath.Join(service.Config.Disk.Directory, ".archive-00000000000000000000000000000000"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	service.Restart(t)
	if status, data := service.Restore(t, reader, "legacy"); status != 200 || data != "legacy-bytes" {
		t.Fatal("legacy entry", status, data)
	}
	if service.Charged() != 0 {
		t.Fatal("stale staging survived startup")
	}
}

func TestStagingAdmissionAndUnlinkFailure(t *testing.T) {
	service := storetest.Start(t, func(c *store.Config) { c.Disk.PhysicalBytes = 4096 }) // one 1 KiB spool
	writer, reader := service.Signer.Writer(t), service.Signer.Reader(t)
	id := service.Save(t, writer, "a", strings.Repeat("a", 1024))
	status, pending := service.Reserve(t, writer, "b", 1024)
	if status != 201 {
		t.Fatal(status)
	}
	if status, _ := service.Restore(t, reader, "a"); status != 503 {
		t.Fatal("download admitted beyond the physical budget", status)
	}
	if status, _ := service.Reserve(t, writer, "c", 1); status != 503 {
		t.Fatal("reservation admitted beyond the physical budget", status)
	}
	service.Upload(t, writer, pending, strings.Repeat("b", 1024))
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	// A download whose staging file cannot be unlinked still succeeds, and
	// the service keeps serving.
	gate := make(chan struct{})
	service.Garage.Mu.Lock()
	service.Garage.Gate, service.Garage.GateKey = gate, fmt.Sprint(id)
	service.Garage.Mu.Unlock()
	_, location := service.Lookup(t, reader, "a")
	done := make(chan int)
	go func() { status, _ := service.Do(t, "GET", location, reader, "", nil); done <- status }()
	waitFor(t, func() bool { return service.Charged() == 4096 })
	if err := os.Chmod(service.Config.Disk.Directory, 0500); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if status := <-done; status != 200 {
		t.Fatal("download", status)
	}
	if err := os.Chmod(service.Config.Disk.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if status, _ := service.Lookup(t, reader, "b"); status != 200 {
		t.Fatal("service fenced by a failed unlink", status)
	}
}

func TestConfigurationBounds(t *testing.T) {
	service := storetest.Start(t)
	for name, change := range map[string]func(*store.Config){
		"plaintext origin":   func(c *store.Config) { c.PublicOrigin = "http://" + strings.TrimPrefix(c.PublicOrigin, "https://") },
		"no verifier":        func(c *store.Config) { c.Verifier = nil },
		"no clock":           func(c *store.Config) { c.Clock = nil },
		"no state":           func(c *store.Config) { c.StateDir = "" },
		"no staging":         func(c *store.Config) { c.Disk.Directory = "" },
		"small staging":      func(c *store.Config) { c.Disk.PhysicalBytes = 1023 },
		"zero quota":         func(c *store.Config) { c.QuotaBytes = 0 },
		"archive over quota": func(c *store.Config) { c.MaxArchiveBytes = c.QuotaBytes + 1 },
		"huge archive":       func(c *store.Config) { c.MaxArchiveBytes, c.QuotaBytes, c.TotalBytes = 1<<30+1, 2<<30, 2<<30 },
		"total under quota":  func(c *store.Config) { c.TotalBytes = c.QuotaBytes - 1 },
		"huge total":         func(c *store.Config) { c.TotalBytes = 4<<30 + 1 },
		"remote garage":      func(c *store.Config) { c.Garage.Endpoint = "http://192.0.2.1:3900" },
		"https garage":       func(c *store.Config) { c.Garage.Endpoint = strings.Replace(c.Garage.Endpoint, "http:", "https:", 1) },
		"garage path":        func(c *store.Config) { c.Garage.Endpoint += "/bucket" },
		"no garage key":      func(c *store.Config) { c.Garage.SecretKey = "" },
		"bucket path":        func(c *store.Config) { c.Garage.Bucket = "a/b" },
		"bad budget":         func(c *store.Config) { c.StorageBudget = &store.StorageBudget{BaseSeconds: 0} },
	} {
		bad := service.Config
		change(&bad)
		if other, err := store.New(bad); err == nil {
			other.Close(context.Background())
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRecoveryRefusesStalledGarageWithinBudget(t *testing.T) {
	budget := store.StorageBudget{BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: 1, RecoveryMaxSeconds: 2}
	service := storetest.Start(t, func(c *store.Config) { c.StorageBudget = &budget })
	writer := service.Signer.Writer(t)
	for _, key := range []string{"a", "b", "c"} {
		service.Save(t, writer, key, "bytes")
	}
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Garage.Mu.Lock()
	service.Garage.Gate = make(chan struct{}) // every GET stalls
	service.Garage.Mu.Unlock()
	started := time.Now()
	if other, err := store.New(service.Config); err == nil {
		other.Close(context.Background())
		t.Fatal("recovery succeeded against a stalled Garage")
	}
	// Recovery shares one deadline (here capped at 2 s; each request at 1 s).
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatal("recovery deadline", elapsed)
	}
}

func TestJournalFailureFencesBeforeStorage(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Writer(t)
	_, id := service.Reserve(t, writer, "fenced", 4)
	path := fmt.Sprintf("/_apis/artifactcache/caches/%d", id)
	service.Do(t, "PATCH", path, writer, "bytes 0-3/*", []byte("data"))
	// A directory cannot be replaced by the journal's rename, even as root.
	state := filepath.Join(service.Config.StateDir, "state.json")
	if err := os.Remove(state); err != nil || os.Mkdir(state, 0700) != nil {
		t.Fatal(err)
	}
	if status, _ := service.Do(t, "POST", path, writer, "", []byte(`{"size":4}`)); status != 503 {
		t.Fatal("finalize without durable intent", status)
	}
	if status, _ := service.Lookup(t, writer, "fenced"); status != 503 {
		t.Fatal("service not fenced after a journal failure", status)
	}
	service.Garage.Mu.Lock()
	defer service.Garage.Mu.Unlock()
	if service.Garage.Puts != 0 {
		t.Fatal("object stored without journaled intent")
	}
	if err := service.Server.Close(context.Background()); err == nil {
		t.Fatal("close hid the journal failure")
	}
}

func TestStateCustodyRefusesLinksAndSurvivesRenames(t *testing.T) {
	service := storetest.Start(t)
	writer := service.Signer.Writer(t)
	// The service keeps its directory by descriptor: renaming the path and
	// planting a link cannot redirect journal writes.
	moved := service.Config.StateDir + ".moved"
	outside := t.TempDir()
	if os.Rename(service.Config.StateDir, moved) != nil || os.Symlink(outside, service.Config.StateDir) != nil {
		t.Fatal("fixture")
	}
	service.Save(t, writer, "custody", "data")
	if files, _ := os.ReadDir(outside); len(files) != 0 {
		t.Fatal("journal writes redirected")
	}
	if data, err := os.ReadFile(filepath.Join(moved, "state.json")); err != nil || !strings.Contains(string(data), "custody") {
		t.Fatal("journal left its directory", err)
	}
	for _, name := range []string{"lock", "state.json"} {
		dir := filepath.Join(t.TempDir(), "state")
		target := filepath.Join(t.TempDir(), "target")
		if os.Mkdir(dir, 0700) != nil || os.WriteFile(target, []byte("unchanged"), 0600) != nil || os.Symlink(target, filepath.Join(dir, name)) != nil {
			t.Fatal("fixture")
		}
		config := service.Config
		config.StateDir = dir
		if other, err := store.New(config); err == nil {
			other.Close(context.Background())
			t.Fatal("symlinked state file accepted", name)
		}
		if data, _ := os.ReadFile(target); string(data) != "unchanged" {
			t.Fatal("symlink target modified")
		}
	}
}

func TestCrashBeforePublicationLeavesDeletableIntent(t *testing.T) {
	service := storetest.Start(t, func(c *store.Config) { c.QuotaBytes, c.TotalBytes = 1024, 1024 })
	writer := service.Signer.Writer(t)
	state := filepath.Join(service.Config.StateDir, "state.json")
	var snapshot []byte
	service.Garage.Mu.Lock()
	service.Garage.OnPut = func() { snapshot, _ = os.ReadFile(state) }
	service.Garage.Mu.Unlock()
	service.Save(t, writer, "orphan", strings.Repeat("o", 1024))
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot), `"Published":false`) || os.WriteFile(state, snapshot, 0600) != nil {
		t.Fatal("journal did not record intent before the PUT")
	}
	service.Garage.Mu.Lock()
	service.Garage.OnPut = nil
	service.Garage.Mu.Unlock()
	service.Restart(t)
	if status, _ := service.Lookup(t, writer, "orphan"); status != 204 {
		t.Fatal("unpublished intent became visible", status)
	}
	service.SweepNow()
	service.Garage.Mu.Lock()
	objects := len(service.Garage.Objects)
	service.Garage.Mu.Unlock()
	if objects != 0 || service.Entries() != 0 {
		t.Fatal("orphan not deleted", objects, service.Entries())
	}
	if status, _ := service.Reserve(t, writer, "next", 1024); status != 201 {
		t.Fatal("orphan quota not recovered", status)
	}
}
