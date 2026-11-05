package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"syscall"
)

const spoolBlockBytes int64 = 4096

// spoolPool owns the private staging directory under an exclusive flock. Its
// contents never survive a restart: openSpoolPool wipes them, so staging needs
// no journal. physical bounds the preallocated bytes of all live spools.
type spoolPool struct {
	mu                sync.Mutex
	directory, lock   *os.File
	physical, charged int64
	allocate          func(fd int, size int64) error
}

// spool is one staged archive: an upload being assembled or a download being verified.
type spool struct {
	pool         *spoolPool
	file         *os.File
	name         string
	size, charge int64
	mu           sync.Mutex // guards parts and sealed
	parts        []interval
	sealed       bool
}

type interval struct{ start, end int64 }

func openSpoolPool(path string, physical int64) (*spoolPool, error) {
	directory, err := openPrivateStateDir(path)
	if err != nil {
		return nil, err
	}
	pool := &spoolPool{directory: directory, physical: physical, allocate: func(fd int, size int64) error { return syscall.Fallocate(fd, 0, 0, size) }}
	fd, err := syscall.Openat(int(directory.Fd()), ".spool-lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		directory.Close()
		return nil, err
	}
	pool.lock = os.NewFile(uintptr(fd), ".spool-lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		err = pool.wipe()
	}
	if err != nil {
		pool.lock.Close()
		directory.Close()
		return nil, err
	}
	return pool, nil
}

// wipe removes every staging file left by an earlier process.
func (pool *spoolPool) wipe() error {
	names, err := pool.directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == ".spool-lock" {
			continue
		}
		if err := syscall.Unlinkat(int(pool.directory.Fd()), name); err != nil {
			return err
		}
	}
	return nil
}

// create preallocates a private file for size bytes, or refuses when the
// physical budget is exhausted.
func (pool *spoolPool) create(size int64) (*spool, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	charge := (size + spoolBlockBytes - 1) / spoolBlockBytes * spoolBlockBytes
	if pool.directory == nil || size <= 0 || charge > pool.physical-pool.charged {
		return nil, errors.New("staging disk admission refused")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	name := ".archive-" + hex.EncodeToString(random)
	fd, err := syscall.Openat(int(pool.directory.Fd()), name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	// Preallocate real blocks, not a sparse file, so ENOSPC surfaces here.
	if err = pool.allocate(fd, charge); err == nil {
		err = file.Truncate(size)
	}
	if err != nil {
		file.Close()
		syscall.Unlinkat(int(pool.directory.Fd()), name)
		return nil, err
	}
	pool.charged += charge
	return &spool{pool: pool, file: file, name: name, size: size, charge: charge}, nil
}

// remove closes and unlinks a spool. A failed unlink keeps the charge (the
// file is wiped at the next start) but never fails the service.
func (pool *spoolPool) remove(staged *spool) {
	if staged == nil {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	staged.file.Close()
	if pool.directory == nil {
		return
	}
	if err := syscall.Unlinkat(int(pool.directory.Fd()), staged.name); err == nil || errors.Is(err, syscall.ENOENT) {
		pool.charged -= staged.charge
	}
}

func (pool *spoolPool) close() error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.directory == nil {
		return nil
	}
	err := errors.Join(pool.lock.Close(), pool.directory.Close())
	pool.directory = nil
	return err
}

// writeChunk writes one uploaded range. An identical repeated range is
// accepted; any other overlap is refused.
func (staged *spool) writeChunk(ctx context.Context, offset int64, data []byte) error {
	staged.mu.Lock()
	defer staged.mu.Unlock()
	if staged.sealed || offset < 0 || len(data) == 0 || offset > staged.size-int64(len(data)) {
		return errors.New("invalid chunk")
	}
	part := interval{offset, offset + int64(len(data)) - 1}
	for _, old := range staged.parts {
		if old == part {
			existing := make([]byte, len(data))
			if _, err := staged.file.ReadAt(existing, offset); err != nil || string(existing) != string(data) {
				return errors.New("conflicting duplicate chunk")
			}
			return nil
		}
		if part.start <= old.end && part.end >= old.start {
			return errors.New("overlapping chunk")
		}
	}
	if len(staged.parts) >= 4096 {
		return errors.New("too many chunks")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := staged.file.WriteAt(data, offset); err != nil {
		return err
	}
	staged.parts = append(staged.parts, part)
	return nil
}

// seal requires complete coverage and returns the SHA256 of the staged bytes.
func (staged *spool) seal(ctx context.Context) ([32]byte, error) {
	staged.mu.Lock()
	defer staged.mu.Unlock()
	sort.Slice(staged.parts, func(i, j int) bool { return staged.parts[i].start < staged.parts[j].start })
	var offset int64
	for _, part := range staged.parts {
		if part.start != offset {
			return [32]byte{}, errors.New("incomplete upload")
		}
		offset = part.end + 1
	}
	if offset != staged.size {
		return [32]byte{}, errors.New("incomplete upload")
	}
	staged.sealed = true
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, io.NewSectionReader(staged.file, 0, staged.size)}); err != nil {
		staged.sealed = false
		return [32]byte{}, err
	}
	return [32]byte(hash.Sum(nil)), nil
}

// contextReader stops a long copy when its context ends.
type contextReader struct {
	ctx context.Context
	io.Reader
}

func (reader contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.Reader.Read(data)
}
