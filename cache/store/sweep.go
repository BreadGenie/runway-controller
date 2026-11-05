package store

import (
	"context"
	"sync"
	"time"
)

// sweeper deletes hidden entries: their Garage object, then their journal
// record. One sweep runs at a time, outside the index lock. After a storage
// failure it backs off (1 s doubling to 5 min) instead of retrying per request.
type sweeper struct {
	run_    sync.Mutex // one sweep at a time
	retryAt time.Time  // guarded by run_
	backoff time.Duration
	signal  chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	server  *Server
}

const (
	sweepMinBackoff = time.Second
	sweepMaxBackoff = 5 * time.Minute
)

func (sw *sweeper) start(s *Server) {
	ctx, cancel := context.WithCancel(context.Background())
	sw.server, sw.signal, sw.cancel, sw.done = s, make(chan struct{}, 1), cancel, make(chan struct{})
	sw.wake()
	go func() {
		defer close(sw.done)
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sw.signal:
			case <-timer.C:
			}
			sw.run(ctx)
			sw.run_.Lock()
			wait := time.Until(sw.retryAt)
			sw.run_.Unlock()
			timer.Reset(max(wait, time.Second))
		}
	}()
}

func (sw *sweeper) stop() {
	if sw.cancel != nil {
		sw.cancel()
		<-sw.done
	}
}

// wake asks the background sweeper to run soon; it never blocks.
func (sw *sweeper) wake() {
	if sw.signal == nil {
		return
	}
	select {
	case sw.signal <- struct{}{}:
	default:
	}
}

// run performs one bounded sweep unless a previous failure's backoff is pending.
func (sw *sweeper) run(ctx context.Context) {
	sw.run_.Lock()
	defer sw.run_.Unlock()
	if time.Now().Before(sw.retryAt) {
		return
	}
	s := sw.server
	s.mu.Lock()
	var victims []*entry
	for _, e := range s.entries {
		if e.hidden && e.pins == 0 {
			victims = append(victims, e)
		}
	}
	s.mu.Unlock()
	for _, e := range victims {
		if e.objectKey != "" {
			deleteCtx, cancel := context.WithTimeout(ctx, s.budget.operation(0))
			err := s.store.delete(deleteCtx, e.objectKey)
			cancel()
			if err != nil {
				sw.backoff = min(max(2*sw.backoff, sweepMinBackoff), sweepMaxBackoff)
				sw.retryAt = time.Now().Add(sw.backoff)
				return
			}
		}
		s.mu.Lock()
		if e.pins == 0 && s.entries[e.id] == e {
			s.pool.remove(e.spool)
			delete(s.entries, e.id)
			for token, ticket := range s.tickets {
				if ticket.entryID == e.id {
					delete(s.tickets, token)
				}
			}
			s.persist()
		}
		s.mu.Unlock()
	}
	sw.backoff = 0
}
