package store

import (
	"context"
	"time"
)

// Charged is the physical staging charge of all live spools.
func (s *Server) Charged() int64 {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return s.pool.charged
}

// Entries counts journaled entries, hidden ones included.
func (s *Server) Entries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// SweepNow clears any outage backoff and runs one sweep.
func (s *Server) SweepNow() {
	s.sweep.run_.Lock()
	s.sweep.retryAt = time.Time{}
	s.sweep.run_.Unlock()
	s.sweep.run(context.Background())
}

func (s *Server) UploadWindow(size int64) time.Duration {
	return s.uploadDeadline(&entry{size: size}).Sub(time.Time{})
}
