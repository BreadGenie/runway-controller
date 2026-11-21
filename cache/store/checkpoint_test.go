package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BreadGenie/runway-controller/cache/store"
	"github.com/BreadGenie/runway-controller/cache/store/storetest"
)

func teamGrant(t *testing.T, service *storetest.Service, tenant int64) string {
	t.Helper()
	return service.Signer.Grant(t, storetest.Job{TenantID: tenant, Write: "refs/heads/main"})
}

func replaceStore(t *testing.T, service *storetest.Service) {
	t.Helper()
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Server = nil
	service.Config.StateDir = filepath.Join(t.TempDir(), "state")
	service.Config.Disk.Directory = filepath.Join(t.TempDir(), "spools")
	service.Restart(t)
}

func waitForCheckpoint(t *testing.T, g *storetest.Garage, tenant int64) (string, []byte) {
	t.Helper()
	key := fmt.Sprintf("/cache/managed/index/%d.json", tenant)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		g.Mu.Lock()
		data := g.Objects[key]
		g.Mu.Unlock()
		if data != nil {
			return key, data
		}
	}
	t.Fatal("checkpoint was not written")
	return "", nil
}

func TestCheckpointRestoresTeamsIndependently(t *testing.T) {
	a, ac := teamGarage(t)
	b, bc := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: ac, 2: bc} })
	teamA, teamB := teamGrant(t, service, 1), teamGrant(t, service, 2)
	aID := service.Save(t, teamA, "deps", "first archive")
	_, oldURL := service.Lookup(t, teamA, "deps")
	location, err := url.Parse(oldURL)
	if err != nil || location.Query().Get("ticket") == "" {
		t.Fatal("missing ticket")
	}
	ticket := location.Query().Get("ticket")
	_, data := waitForCheckpoint(t, a, 1)
	for _, secret := range []string{ac.AccessKey, ac.SecretKey, teamA, ticket, fmt.Sprintf("%x", sha256.Sum256([]byte(ticket)))} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("checkpoint contains a credential or ticket")
		}
	}
	if bytes.Contains(data, []byte(`"Tickets"`)) {
		t.Fatal("checkpoint has a ticket field")
	}
	replaceStore(t, service)
	bID := service.Save(t, teamB, "deps", "second archive")
	if aID != bID {
		t.Fatal("fixture should create overlapping IDs")
	}
	waitForCheckpoint(t, b, 2)
	replaceStore(t, service)
	if status, _ := service.Do(t, "GET", oldURL, teamA, "", nil); status != 404 {
		t.Fatal("old ticket redeemed", status)
	}
	b.Mu.Lock()
	gets := b.CheckpointGets
	b.Mu.Unlock()
	if status, data := service.Restore(t, teamA, "deps"); status != 200 || data != "first archive" {
		t.Fatal("Team A restore", status, data)
	}
	b.Mu.Lock()
	got := b.CheckpointGets
	b.Mu.Unlock()
	if got != gets {
		t.Fatal("Team A recovery contacted Team B checkpoint")
	}
	if status, data := service.Restore(t, teamB, "deps"); status != 200 || data != "second archive" {
		t.Fatal("Team B restore", status, data)
	}
	newID := service.Save(t, teamA, "new", "third archive")
	if newID <= aID {
		t.Fatal("new ID reused a checkpoint ID")
	}
	service.Restart(t)
	for _, check := range []struct{ token, key, archive string }{
		{teamA, "deps", "first archive"}, {teamB, "deps", "second archive"}, {teamA, "new", "third archive"},
	} {
		if status, data := service.Restore(t, check.token, check.key); status != 200 || data != check.archive {
			t.Fatal("reboot lost a restored entry", status, data)
		}
	}
}

func TestCheckpointRefusesInvalidEntries(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "bucket", "namespace", "origin", "version"} {
		t.Run(damage, func(t *testing.T) {
			g, config := teamGarage(t)
			service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
			token := teamGrant(t, service, 1)
			service.Save(t, token, "deps", "archive")
			key, _ := waitForCheckpoint(t, g, 1)
			replaceStore(t, service)
			g.Mu.Lock()
			var checkpoint map[string]json.RawMessage
			if err := json.Unmarshal(g.Objects[key], &checkpoint); err != nil {
				g.Mu.Unlock()
				t.Fatal(err)
			}
			switch damage {
			case "missing", "corrupt":
				for object := range g.Objects {
					if strings.HasSuffix(object, "/1/1") {
						if damage == "missing" {
							delete(g.Objects, object)
						} else {
							g.Objects[object] = []byte("changed")
						}
					}
				}
			case "bucket":
				checkpoint["Storage"] = json.RawMessage(`{"Bucket":"other"}`)
			case "namespace":
				checkpoint["Namespace"] = json.RawMessage(`"managed/00000000000000000000000000000000/"`)
			case "origin":
				checkpoint["PublicOrigin"] = json.RawMessage(`"https://other.example"`)
			case "version":
				checkpoint["Version"] = json.RawMessage(`99`)
			}
			g.Objects[key], _ = json.Marshal(checkpoint)
			g.Mu.Unlock()
			if status, _ := service.Lookup(t, token, "deps"); status != 204 {
				t.Fatal("invalid checkpoint entry hit", status)
			}
		})
	}
}

func TestCheckpointSingleflightAndRetry(t *testing.T) {
	g, config := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
	token := teamGrant(t, service, 1)
	service.Save(t, token, "deps", "archive")
	waitForCheckpoint(t, g, 1)
	replaceStore(t, service)
	g.Mu.Lock()
	g.FailCheckpointGet = true
	before := g.CheckpointGets
	g.Mu.Unlock()
	for range 2 {
		if status, _ := service.Lookup(t, token, "deps"); status != 204 {
			t.Fatal("outage should be a miss", status)
		}
	}

	gate := make(chan struct{})
	g.Mu.Lock()
	g.FailCheckpointGet = false
	g.Gate = gate
	g.GateKey = "1.json"
	failedGets := g.CheckpointGets - before
	g.Mu.Unlock()
	if failedGets != 1 {
		close(gate)
		t.Fatal("backoff retried immediately", failedGets)
	}
	time.Sleep(1100 * time.Millisecond)
	var requests sync.WaitGroup
	for range 8 {
		requests.Go(func() {
			if status, _ := service.Lookup(t, token, "deps"); status != 200 {
				t.Errorf("retry lookup: %d", status)
			}
		})
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		g.Mu.Lock()
		started := g.CheckpointGets > before+1
		g.Mu.Unlock()
		if started {
			break
		}
	}
	close(gate)
	requests.Wait()
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if got := g.CheckpointGets - before; got != 2 {
		t.Fatalf("checkpoint GET count = %d, want 2 across outage and retry", got)
	}
}

func TestCheckpointWriteFailureDoesNotFailPublish(t *testing.T) {
	g, config := teamGarage(t)
	g.Mu.Lock()
	g.FailCheckpointPut = true
	g.Mu.Unlock()
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
	token := teamGrant(t, service, 1)
	service.Save(t, token, "deps", "archive")
	if status, data := service.Restore(t, token, "deps"); status != 200 || data != "archive" {
		t.Fatal("checkpoint outage affected archive", status, data)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		g.Mu.Lock()
		failed := g.CheckpointPuts > 0
		g.Mu.Unlock()
		if failed {
			return
		}
	}
	t.Fatal("no checkpoint PUT attempted")
}

func TestLocalJournalDoesNotLoadCheckpoint(t *testing.T) {
	g, config := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
	token := teamGrant(t, service, 1)
	service.Save(t, token, "deps", "archive")
	waitForCheckpoint(t, g, 1)
	_, location := service.Lookup(t, token, "deps")
	service.Restart(t)
	g.Mu.Lock()
	before := g.CheckpointGets
	g.Mu.Unlock()
	if status, _ := service.Do(t, "GET", location, token, "", nil); status != 200 {
		t.Fatal("reboot lost local ticket", status)
	}
	if status, _ := service.Lookup(t, token, "deps"); status != 200 {
		t.Fatal("reboot lost indexed entry", status)
	}
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if g.CheckpointGets != before {
		t.Fatal("reboot loaded checkpoint instead of journal")
	}
}

func TestCheckpointExcludesEvictedEntryBeforeDeletion(t *testing.T) {
	g, config := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) {
		c.Buckets = buckets{1: config}
		c.QuotaBytes = 7
		c.MaxArchiveBytes = 7
	})
	token := teamGrant(t, service, 1)
	service.Save(t, token, "old", "archive")
	key, _ := waitForCheckpoint(t, g, 1)
	service.Save(t, token, "new", "another")
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if bytes.Contains(g.Objects[key], []byte(`"Key":"old"`)) || !bytes.Contains(g.Objects[key], []byte(`"Key":"new"`)) {
		t.Fatal("checkpoint retained the evicted entry or lost the new entry")
	}
}

func checkpointGets(g *storetest.Garage) int {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	return g.CheckpointGets
}

func TestCheckpointResolutionSurvivesRestart(t *testing.T) {
	for _, older := range []bool{false, true} {
		t.Run(fmt.Sprintf("older-journal=%t", older), func(t *testing.T) {
			a, ac := teamGarage(t)
			b, bc := teamGarage(t)
			service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: ac, 2: bc} })
			teamA, teamB := teamGrant(t, service, 1), teamGrant(t, service, 2)
			service.Save(t, teamA, "deps", "first")
			service.Save(t, teamB, "deps", "second")
			waitForCheckpoint(t, a, 1)
			waitForCheckpoint(t, b, 2)
			replaceStore(t, service)
			if older {
				editJournal(t, service, func(d map[string]json.RawMessage) {
					delete(d, "ResolvedTeams")
					delete(d, "CheckpointTeams")
				})
			} else {
				service.Restart(t)
			}
			aBefore, bBefore := checkpointGets(a), checkpointGets(b)
			if status, data := service.Restore(t, teamA, "deps"); status != 200 || data != "first" {
				t.Fatal("Team A after pre-traffic restart", status, data)
			}
			if checkpointGets(a) != aBefore+1 || checkpointGets(b) != bBefore {
				t.Fatal("expected one Team A checkpoint GET")
			}
			service.Restart(t)
			if status, data := service.Restore(t, teamB, "deps"); status != 200 || data != "second" {
				t.Fatal("Team B after Team A's restart", status, data)
			}
			if status, _ := service.Lookup(t, teamA, "deps"); status != 200 {
				t.Fatal("resolved Team A missed", status)
			}
			if checkpointGets(a) != aBefore+1 || checkpointGets(b) != bBefore+1 {
				t.Fatal("only unresolved Team B should have loaded a checkpoint")
			}
		})
	}
}

func TestMissingCheckpointResolutionSurvivesRestart(t *testing.T) {
	g, config := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
	token := teamGrant(t, service, 1)
	for range 2 {
		if status, _ := service.Lookup(t, token, "deps"); status != 204 {
			t.Fatal("missing checkpoint should be a miss", status)
		}
		service.Restart(t)
	}
	if got := checkpointGets(g); got != 1 {
		t.Fatalf("checkpoint GETs = %d, want 1", got)
	}
}

func editJournal(t *testing.T, service *storetest.Service, edit func(map[string]json.RawMessage)) {
	t.Helper()
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.Server = nil
	path := filepath.Join(service.Config.StateDir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var journal map[string]json.RawMessage
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	edit(journal)
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	service.Restart(t)
}

func TestOlderJournalKeepsLocalEntryWhenMergingCheckpoint(t *testing.T) {
	g, config := teamGarage(t)
	service := storetest.Start(t, func(c *store.Config) { c.Buckets = buckets{1: config} })
	token := teamGrant(t, service, 1)
	service.Save(t, token, "duplicate", "remote")
	service.Save(t, token, "remote-only", "restored")
	key, _ := waitForCheckpoint(t, g, 1)
	replaceStore(t, service)
	g.Mu.Lock()
	original := g.Objects[key]
	delete(g.Objects, key)
	g.Mu.Unlock()
	service.Save(t, token, "duplicate", "local")
	if err := service.Server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.Mu.Lock()
	g.Objects[key] = original
	g.Mu.Unlock()

	editJournal(t, service, func(d map[string]json.RawMessage) {
		delete(d, "ResolvedTeams")
		delete(d, "CheckpointTeams")
	})
	for attempt := range 3 {
		for key, want := range map[string]string{"duplicate": "local", "remote-only": "restored"} {
			if status, data := service.Restore(t, token, key); status != 200 || data != want {
				t.Fatalf("%s: status %d, data %q, want %q", key, status, data, want)
			}
		}
		switch attempt {
		case 0:
			service.Restart(t)
		case 1:
			replaceStore(t, service)
		}
	}
}
