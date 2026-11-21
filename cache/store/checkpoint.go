package store

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	checkpointPrefix  = "managed/index/"
	checkpointLimit   = 2 << 20
	checkpointTimeout = 5 * time.Second
)

// A checkpoint contains only published entries and the identities needed to
// bind their object keys to this Team's storage and verifier audience.
type teamCheckpoint struct {
	Version      int
	Tenant       int64
	Storage      diskStorage
	PublicOrigin string
	Namespace    string
	NextID       int64
	Entries      []diskEntry
}

type checkpointLoad struct {
	done    chan struct{}
	retryAt time.Time
	backoff time.Duration
}

// The discovery key sits beside the random archive namespaces so the fresh VM
// can find it using only the authenticated tenant and its own bucket. The
// reconciler revokes the old VM before creating the next, so replacement of
// this object uses last-writer-wins.
func checkpointKey(tenant int64) string {
	return checkpointPrefix + strconv.FormatInt(tenant, 10) + ".json"
}

func (s *Server) teamNamespace(tenant int64) string {
	if namespace := s.checkpointTeams[tenant].Namespace; namespace != "" {
		return namespace
	}
	return s.namespace
}

func (s *Server) hide(e *entry) {
	e.hidden = true
	if e.published {
		s.markCheckpoint(e.repository)
	}
	s.sweep.wake()
}

func (s *Server) markCheckpoint(repository string) {
	if s.checkpointDirty == nil {
		return
	}
	tenant, ok := repositoryTenant(repository)
	if ok && tenant != 0 {
		s.checkpointRevision++
		s.checkpointDirty[tenant] = s.checkpointRevision
		s.sweep.wake()
	}
}

func (s *Server) restoreTeam(ctx context.Context, tenant int64) bool {
	if s.config.Buckets == nil {
		return true
	}
	s.mu.Lock()
	if s.resolvedTeams[tenant] {
		s.mu.Unlock()
		return true
	}
	load := s.checkpointLoads[tenant]
	if load == nil {
		load = &checkpointLoad{}
		s.checkpointLoads[tenant] = load
	}
	switch {
	case load.done != nil:
		done := load.done
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			ready := s.resolvedTeams[tenant]
			s.mu.Unlock()
			return ready
		case <-ctx.Done():
			return false
		}
	case time.Now().Before(load.retryAt):
		s.mu.Unlock()
		return false
	}
	load.done = make(chan struct{})
	s.mu.Unlock()

	// One disconnected waiter cannot cancel recovery for the other requests.
	query, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointTimeout)
	checkpoint, err := s.readCheckpoint(query, tenant)
	cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && !s.closed && s.stateErr == nil {
		if checkpoint != nil {
			err = s.mergeCheckpoint(*checkpoint)
		}
		if err == nil {
			s.resolvedTeams[tenant] = true
			err = s.persist()
			if err != nil {
				delete(s.resolvedTeams, tenant)
			} else {
				delete(s.checkpointLoads, tenant)
			}
		}
	}
	if err != nil {
		log.Printf("managed cache checkpoint load failed for tenant %d", tenant)
		load.backoff = min(max(2*load.backoff, time.Second), time.Minute)
		load.retryAt = time.Now().Add(load.backoff)
	}
	close(load.done)
	load.done = nil
	return s.resolvedTeams[tenant]
}

func (s *Server) readCheckpoint(ctx context.Context, tenant int64) (*teamCheckpoint, error) {
	config, err := s.store.teamBucket(ctx, tenant)
	if err != nil {
		return nil, err
	}
	target := newGarageStore(config, s.budget)
	defer target.close()
	key := checkpointKey(tenant)
	result, err := target.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &target.bucket, Key: &key})
	if lostArchive(err) {
		return nil, nil
	}
	if err != nil {
		s.store.invalidateTeamAuth(tenant, err)
		return nil, err
	}
	defer result.Body.Close()
	data, err := io.ReadAll(io.LimitReader(result.Body, checkpointLimit))
	if err != nil {
		return nil, err
	}
	var d teamCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if len(data) >= checkpointLimit || decoder.Decode(&d) != nil || decoder.Decode(new(any)) != io.EOF ||
		(d.Version != 1 && d.Version != 2) || d.Tenant != tenant || d.Storage != storageIdentity(config) || d.PublicOrigin != s.config.PublicOrigin ||
		!validNamespace(d.Namespace, "managed/") || d.NextID < 0 || d.NextID > math.MaxInt64-maxEntries || len(d.Entries) > maxEntries {
		log.Printf("managed cache checkpoint refused for tenant %d", tenant)
		return nil, nil
	}
	seen := make(map[string]bool)
	for _, v := range d.Entries {
		entryTenant, ok := repositoryTenant(v.Repository)
		namespace, limit := d.Namespace, d.NextID
		if d.Version == 2 && v.ObjectNamespace != "" {
			namespace, limit = v.ObjectNamespace, v.ID
		}
		if !ok || entryTenant != tenant || v.ObjectID != 0 || !v.Published || v.Hidden || seen[v.Object] ||
			!validNamespace(namespace, "managed/") || d.Version == 1 && v.ObjectNamespace != "" ||
			!s.validDiskEntry(v, limit, managedObjectKey(namespace, tenant, v.ID)) {
			log.Printf("managed cache checkpoint refused for tenant %d", tenant)
			return nil, nil
		}
		seen[v.Object] = true
	}
	return &d, nil
}

func (s *Server) mergeCheckpoint(d teamCheckpoint) error {
	if s.nextID > math.MaxInt64-int64(len(d.Entries)) {
		return errors.New("cache entry IDs exhausted")
	}
	if len(d.Entries) == 0 {
		log.Printf("managed cache checkpoint restored tenant %d: 0 entries restored, 0 dropped", d.Tenant)
		return nil
	}
	type cacheKey struct{ repository, ref, key, version string }
	keys := make(map[cacheKey]bool)
	objects := make(map[string]bool)
	for _, e := range s.entries {
		keys[cacheKey{e.repository, e.ref, e.key, e.version}] = true
		objects[e.objectKey] = true
	}
	if s.checkpointTeams == nil {
		s.checkpointTeams = make(map[int64]diskTeam)
	}
	// Keep local archives under their existing keys. Restored entries may
	// retain a different namespace when merging into an older journal.
	team := diskTeam{Namespace: d.Namespace, NextObjectID: max(d.NextID, s.nextID)}
	if d.Namespace != s.namespace && s.hasTeamEntries(d.Tenant) {
		team = diskTeam{Namespace: s.namespace, NextObjectID: s.nextID}
	}
	// Restore newest entries first when the regional index is already near
	// capacity. Allocate IDs oldest first to retain prefix-match precedence.
	slices.SortFunc(d.Entries, func(a, b diskEntry) int {
		return b.Accessed.Compare(a.Accessed)
	})
	var accepted []diskEntry
	_, total := s.usage("")
	for _, v := range d.Entries {
		key := cacheKey{v.Repository, v.Ref, v.Key, v.Version}
		if keys[key] || objects[v.Object] {
			continue
		}
		repo, _ := s.usage(v.Repository)
		if len(s.entries)+len(accepted) >= maxEntries || total+v.Size > s.config.TotalBytes || repo+v.Size > s.config.QuotaBytes {
			continue
		}
		// Account for previously accepted entries from this repository.
		for _, newer := range accepted {
			if newer.Repository == v.Repository {
				repo += newer.Size
			}
		}
		if repo+v.Size <= s.config.QuotaBytes {
			accepted = append(accepted, v)
			keys[key] = true
			total += v.Size
		}
	}
	for _, v := range d.Entries {
		if namespace, _ := managedObjectScope(v.Object); namespace == team.Namespace {
			team.NextObjectID = max(team.NextObjectID, v.ID)
		}
	}
	s.checkpointTeams[d.Tenant] = team
	slices.SortFunc(accepted, func(a, b diskEntry) int { return cmp.Compare(a.ID, b.ID) })
	for _, v := range accepted {
		s.nextID++
		v.ID, v.ObjectID = s.nextID, v.ID
		e := entryFromDisk(v)
		e.needsVerification = true
		s.entries[e.id] = e
	}
	log.Printf("managed cache checkpoint restored tenant %d: %d entries restored, %d dropped", d.Tenant, len(accepted), len(d.Entries)-len(accepted))
	return nil
}

func (s *Server) hasTeamEntries(tenant int64) bool {
	for _, e := range s.entries {
		if owner, _ := repositoryTenant(e.repository); owner == tenant {
			return true
		}
	}
	return false
}

// Only the sweeper writes checkpoints. Publish marks a dirty revision after
// the archive PUT and local journal commit. A failed checkpoint is retried and
// never affects the status of the publish that scheduled it.
func (s *Server) flushCheckpoints(ctx context.Context) {
	s.mu.Lock()
	dirty := make(map[int64]uint64, len(s.checkpointDirty))
	for tenant, revision := range s.checkpointDirty {
		dirty[tenant] = revision
	}
	s.mu.Unlock()
	for tenant, revision := range dirty {
		if ctx.Err() != nil {
			return
		}
		// Older journals may have local entries but lack resolution metadata.
		// Reconcile those entries before replacing the remote checkpoint.
		if !s.restoreTeam(ctx, tenant) {
			continue
		}
		s.mu.Lock()
		if s.stateErr != nil || s.closed {
			s.mu.Unlock()
			return
		}
		d := teamCheckpoint{Version: 1, Tenant: tenant, PublicOrigin: s.config.PublicOrigin, Namespace: s.teamNamespace(tenant), NextID: s.nextID}
		if team, ok := s.checkpointTeams[tenant]; ok {
			d.NextID = team.NextObjectID
		}
		for _, e := range s.entries {
			owner, _ := repositoryTenant(e.repository)
			if owner == tenant && e.published && !e.hidden {
				v := e.diskEntry()
				if v.ObjectID != 0 {
					v.ID, v.ObjectID = v.ObjectID, 0
				}
				if namespace, _ := managedObjectScope(v.Object); namespace != d.Namespace {
					d.Version = 2
					v.ObjectNamespace = namespace
				}
				d.Entries = append(d.Entries, v)
			}
		}
		s.mu.Unlock()
		query, cancel := context.WithTimeout(ctx, checkpointTimeout)
		err := s.writeCheckpoint(query, d)
		cancel()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			log.Printf("managed cache checkpoint write failed for tenant %d", tenant)
			continue
		}
		s.mu.Lock()
		if s.checkpointDirty[tenant] == revision {
			delete(s.checkpointDirty, tenant)
		}
		s.mu.Unlock()
	}
}

func (s *Server) writeCheckpoint(ctx context.Context, d teamCheckpoint) error {
	config, err := s.store.teamBucket(ctx, d.Tenant)
	if err != nil {
		return err
	}
	d.Storage = storageIdentity(config)
	target := newGarageStore(config, s.budget)
	defer target.close()
	// Storage may have been reprovisioned since publication. Check existence in
	// the resolved bucket, and serialize this snapshot with our object deletes.
	entries := d.Entries[:0]
	for _, e := range d.Entries {
		_, err := target.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &target.bucket, Key: &e.Object})
		if lostArchive(err) {
			continue
		}
		if err != nil {
			s.store.invalidateTeamAuth(d.Tenant, err)
			return err
		}
		entries = append(entries, e)
	}
	d.Entries = entries
	data, err := json.Marshal(d)
	if err != nil || len(data) >= checkpointLimit {
		return errors.New("cache checkpoint exceeds size bound")
	}
	key := checkpointKey(d.Tenant)
	_, err = target.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &target.bucket, Key: &key, Body: bytes.NewReader(data)})
	s.store.invalidateTeamAuth(d.Tenant, err)
	return err
}

// verifyLookup checks only the selected restored archive, outside the index lock.
// Caller holds mu on entry and return.
func (s *Server) verifyLookup(ctx context.Context, e *entry) bool {
	if !e.needsVerification {
		return true
	}
	e.pins++
	s.mu.Unlock()
	query, cancel := context.WithTimeout(ctx, s.budget.operation(e.size))
	err := s.verifyArchive(query, e)
	cancel()
	s.mu.Lock()
	e.pins--
	if err == nil {
		e.needsVerification = false
		return !e.hidden && !s.closed && s.stateErr == nil
	}
	if ctx.Err() == nil && lostArchive(err) && !e.hidden {
		s.hide(e)
		log.Printf("managed cache checkpoint dropped 1 entry after integrity verification")
	}
	return false
}

// A nil result defers managed namespace collection until checkpoint recovery
// can identify all namespaces that contain live archives for this Team.
func (s *Server) protectedNamespaces(tenant int64) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resolvedTeams[tenant] {
		return nil
	}
	protected := map[string]bool{s.teamNamespace(tenant): true}
	for _, e := range s.entries {
		if namespace, owner := managedObjectScope(e.objectKey); owner == tenant {
			protected[namespace] = true
		}
	}
	return protected
}
