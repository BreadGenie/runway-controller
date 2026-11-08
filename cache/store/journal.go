package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// objectPrefix is the persisted Garage key prefix of every namespace; the live
// journal on blr-1 names its objects with it.
const objectPrefix = "prototype/"

// The journal holds identities, digests and object intent, never credentials
// or download bearer tokens; only hashes of read tickets are stored. Journals
// written before 2026-11-05 also carry spool and generation fields, which are
// ignored.
type diskEntry struct {
	ID, Size                                       int64
	Repository, Ref, Key, Version, Subject, Object string
	Created, Accessed                              time.Time
	Published, Hidden                              bool
	Digest                                         [32]byte
}

type diskTicket struct {
	EntryID int64
	Expires time.Time
}

type diskStorage struct{ Endpoint, Region, Bucket string }

type diskState struct {
	Version      int
	Storage      diskStorage
	PublicOrigin string
	Namespace    string
	NextID       int64
	Entries      []diskEntry
	Tickets      map[string]diskTicket
}

func ticketKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) storage() diskStorage {
	if s.config.Buckets != nil {
		return diskStorage{Endpoint: "managed", Bucket: s.config.ManagedPrefix}
	}
	return diskStorage{Endpoint: s.config.Garage.Endpoint, Region: s.config.Garage.Region, Bucket: s.config.Garage.Bucket}
}

// openState takes exclusive ownership of the state directory and recovers it.
func (s *Server) openState() error {
	dir, err := openPrivateStateDir(s.config.StateDir)
	if err != nil {
		return err
	}
	lock, err := openStateFile(dir, "lock", syscall.O_CREAT|syscall.O_RDWR)
	if err == nil && syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		err = errors.New("state directory already owned")
	}
	if err != nil {
		dir.Close()
		return err
	}
	s.stateDir, s.stateLock = dir, lock
	if err = s.recover(); err != nil {
		lock.Close()
		dir.Close()
	}
	return err
}

// recover loads the journal and verifies every published object within one
// shared budget. A lost or different object becomes a hidden miss; any other
// storage failure refuses startup and leaves the journal for a later attempt.
func (s *Server) recover() error {
	f, err := openStateFile(s.stateDir, "state.json", syscall.O_RDONLY)
	if os.IsNotExist(err) {
		if s.config.Buckets != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.budget.RecoveryMaxSeconds)*time.Second)
			defer cancel()
			if err := s.store.reset(ctx); err != nil {
				return err
			}
			s.namespace = s.config.ManagedPrefix
			return s.persist()
		}
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		s.namespace = objectPrefix + hex.EncodeToString(random) + "/"
		return s.persist()
	}
	if err != nil {
		return err
	}
	var d diskState
	err = json.NewDecoder(io.LimitReader(f, 2<<20)).Decode(&d)
	f.Close()
	hexPart := strings.TrimSuffix(strings.TrimPrefix(d.Namespace, objectPrefix), "/")
	_, hexErr := hex.DecodeString(hexPart)
	validNamespace := hexErr == nil && len(hexPart) == 32 && d.Namespace == objectPrefix+hexPart+"/"
	if s.config.Buckets != nil {
		validNamespace = d.Namespace == s.config.ManagedPrefix
	}
	if err != nil || !validNamespace ||
		d.Version != 1 || d.Storage != s.storage() || d.PublicOrigin != s.config.PublicOrigin || len(d.Entries) > maxEntries || len(d.Tickets) > maxTickets || d.NextID < 0 {
		return errors.New("invalid cache state")
	}
	s.namespace, s.nextID = d.Namespace, d.NextID
	objects, bytes := 0, int64(0)
	for _, v := range d.Entries {
		if s.objectKey(v.Repository, v.ID) == "" || v.ID <= 0 || v.ID > d.NextID || s.entries[v.ID] != nil || v.Size <= 0 || v.Size > s.config.MaxArchiveBytes || v.Repository == "" || v.Ref == "" || v.Subject == "" || !validKey(v.Key) || v.Version == "" || v.Created.IsZero() || v.Accessed.IsZero() || v.Object != "" && v.Object != s.objectKey(v.Repository, v.ID) || v.Published && v.Object == "" {
			return errors.New("invalid cache entry")
		}
		// No upload resumes across a restart: unpublished entries stay hidden.
		e := &entry{id: v.ID, size: v.Size, repository: v.Repository, ref: v.Ref, key: v.Key, version: v.Version, subject: v.Subject, objectKey: v.Object, created: v.Created, accessed: v.Accessed, published: v.Published, hidden: v.Hidden || !v.Published, digest: v.Digest}
		s.entries[e.id] = e
		if !e.hidden {
			objects, bytes = objects+1, bytes+e.size
		}
	}
	if _, total := s.usage(""); total > s.config.TotalBytes {
		return errors.New("recovered cache exceeds configured budget")
	}
	now := s.now()
	for k, v := range d.Tickets {
		if _, err := hex.DecodeString(k); err != nil || len(k) != 64 || s.entries[v.EntryID] == nil || v.Expires.After(now.Add(ticketLifetime)) {
			return errors.New("invalid read ticket")
		}
		if now.Before(v.Expires) {
			s.tickets[k] = readTicket{v.EntryID, v.Expires}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.budget.recovery(objects, bytes))
	defer cancel()
	for _, e := range s.entries {
		if e.hidden {
			continue
		}
		staged, err := s.pool.create(e.size)
		if err != nil {
			return err
		}
		err = s.store.get(ctx, e.objectKey, staged, e.digest)
		s.pool.remove(staged)
		if err != nil && ctx.Err() == nil && lostArchive(err) {
			e.hidden = true // Garage is not the source of truth: this is a miss
		} else if err != nil {
			return errors.New("cache recovery storage unavailable")
		}
	}
	return s.persist()
}

// persist atomically replaces the journal. Any failure fences the service
// until restart: after an uncertain rename no later storage effect is safe.
// Caller holds mu (or owns s exclusively).
func (s *Server) persist() error {
	if s.stateErr != nil {
		return s.stateErr
	}
	d := diskState{Version: 1, Storage: s.storage(), PublicOrigin: s.config.PublicOrigin, Namespace: s.namespace, NextID: s.nextID, Tickets: make(map[string]diskTicket)}
	for _, e := range s.entries {
		d.Entries = append(d.Entries, diskEntry{ID: e.id, Size: e.size, Repository: e.repository, Ref: e.ref, Key: e.key, Version: e.version, Subject: e.subject, Object: e.objectKey, Created: e.created, Accessed: e.accessed, Published: e.published, Hidden: e.hidden, Digest: e.digest})
	}
	for k, v := range s.tickets {
		d.Tickets[k] = diskTicket{v.entryID, v.expires}
	}
	s.stateErr = s.writeState(d)
	return s.stateErr
}

func (s *Server) writeState(d diskState) error {
	encoded, err := json.Marshal(d)
	if err != nil || len(encoded) >= 2<<20 {
		return errors.New("cache state exceeds journal bound")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	name := ".state-" + hex.EncodeToString(random)
	f, err := openStateFile(s.stateDir, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL)
	if err != nil {
		return err
	}
	defer syscall.Unlinkat(int(s.stateDir.Fd()), name)
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = syscall.Renameat(int(s.stateDir.Fd()), name, int(s.stateDir.Fd()), "state.json"); err != nil {
		return err
	}
	return s.stateDir.Sync()
}
