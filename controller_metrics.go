package main

import (
	"io"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

type controllerMetrics struct {
	mu                                      sync.Mutex
	now                                     func() time.Time
	pool, region                            string
	fence                                   int64
	renewed, unservedSince, noProgressSince time.Time
	assigned, runningJobs                   int
	runners, cleaning                       int
	cleanup                                 map[string]time.Time
	oldestCleanup                           time.Time
	guest                                   guestCacheMetrics
	lastGuest                               guestCacheMetrics
	lastLease                               string
	extractionKnown                         bool
}

func newControllerMetrics(snapshot pilotSnapshot, now func() time.Time) *controllerMetrics {
	m := &controllerMetrics{now: now, pool: snapshot.Pool.ID, region: snapshot.Pool.Region, fence: snapshot.Fence, cleanup: make(map[string]time.Time)}
	m.renew(now())
	m.observe(snapshot)
	return m
}

func (m *controllerMetrics) renewalStart() time.Time {
	if m == nil {
		return time.Time{}
	}
	return m.now()
}

func (m *controllerMetrics) renew(started time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if started.After(m.renewed) {
		m.renewed = started
	}
}

func (m *controllerMetrics) demand(stats *scaleset.RunnerScaleSetStatistic, available int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.observeProgress()
	if stats == nil {
		if available > 0 && m.unservedSince.IsZero() {
			m.unservedSince = m.now()
		}
		return
	}
	running := max(0, stats.TotalRunningJobs)
	m.assigned = max(0, stats.TotalAssignedJobs)
	switch {
	case m.assigned <= running && available == 0:
		m.unservedSince = time.Time{}
	case m.unservedSince.IsZero():
		m.unservedSince = m.now()
	}
	m.runningJobs = running
}

// With concurrency one, a running job is expected to occupy the only slot.
func (m *controllerMetrics) observeProgress() {
	if m.unservedSince.IsZero() || m.runningJobs > 0 {
		m.noProgressSince = time.Time{}
	} else if m.noProgressSince.IsZero() {
		m.noProgressSince = m.now()
	}
}

func (m *controllerMetrics) clearDemand() {
	m.demand(&scaleset.RunnerScaleSetStatistic{}, 0)
}

func (m *controllerMetrics) observe(snapshot pilotSnapshot) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runners, m.cleaning = 0, 0
	m.oldestCleanup = time.Time{}
	current := make(map[string]time.Time)
	m.extractionKnown = false
	for _, lease := range snapshot.Leases {
		if lease.Phase == "running" {
			m.runners++
		}
		if lease.ID == m.lastLease && (lease.Phase == "running" || lease.Phase == "jit_ready") {
			m.extractionKnown = m.lastGuest.ExtractionBytes != nil
		}
		if lease.Phase != "deleting" && !(lease.Phase == "deleted" && lease.JITState != "removed") {
			continue
		}
		start := m.cleanup[lease.ID]
		if start.IsZero() {
			start = m.now()
			// An older lease can be recovered after its deadline. Track time
			// past that deadline even if this process did not see retirement.
			if !lease.Deadline.IsZero() && lease.Deadline.Before(start) {
				start = lease.Deadline
			}
		}
		current[lease.ID] = start
		m.cleaning++
		if m.oldestCleanup.IsZero() || start.Before(m.oldestCleanup) {
			m.oldestCleanup = start
		}
	}
	m.cleanup = current
	if !snapshot.Pool.Enabled {
		m.assigned, m.runningJobs = 0, 0
		m.unservedSince = time.Time{}
		m.observeProgress()
	}
}

type guestCacheMetrics struct {
	Routed          uint64  `json:"routed"`
	Fallback        uint64  `json:"fallback"`
	RelayErrors     uint64  `json:"relay_errors"`
	ExtractionBytes *uint64 `json:"extraction_bytes"`
}

func (m *controllerMetrics) observeGuest(lease pilotLease, stats guestCacheMetrics) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastLease != lease.ID {
		m.lastLease = lease.ID
		m.lastGuest = guestCacheMetrics{}
	}
	// The guest counters are bounded files in its disposable /run filesystem.
	// Keep a high-water mark so repeat or regressed observations cannot add events.
	m.guest.Routed += max(stats.Routed, m.lastGuest.Routed) - m.lastGuest.Routed
	m.guest.Fallback += max(stats.Fallback, m.lastGuest.Fallback) - m.lastGuest.Fallback
	m.guest.RelayErrors += max(stats.RelayErrors, m.lastGuest.RelayErrors) - m.lastGuest.RelayErrors
	m.lastGuest.Routed = max(stats.Routed, m.lastGuest.Routed)
	m.lastGuest.Fallback = max(stats.Fallback, m.lastGuest.Fallback)
	m.lastGuest.RelayErrors = max(stats.RelayErrors, m.lastGuest.RelayErrors)
	m.lastGuest.ExtractionBytes = stats.ExtractionBytes
	m.extractionKnown = stats.ExtractionBytes != nil && (lease.Phase == "running" || lease.Phase == "jit_ready")
}

func (m *controllerMetrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	labels := "{pool=" + metricLabel(m.pool) + ",region=" + metricLabel(m.region) + "}"
	now := m.now()
	age := func(start time.Time) float64 {
		if start.IsZero() {
			return 0
		}
		return max(0, now.Sub(start).Seconds())
	}
	metric(w, "controller_assigned_jobs", "gauge", labels, m.assigned)
	metric(w, "controller_waiting_jobs", "gauge", labels, max(m.assigned-m.runningJobs, boolInt(!m.unservedSince.IsZero())))
	metric(w, "controller_running_runners", "gauge", labels, m.runners)
	metric(w, "controller_unserved_seconds", "gauge", labels, age(m.unservedSince))
	metric(w, "controller_no_progress_seconds", "gauge", labels, age(m.noProgressSince))
	metric(w, "controller_ownership_fence", "gauge", labels, m.fence)
	metric(w, "controller_ownership_renewal_age_seconds", "gauge", labels, age(m.renewed))
	metric(w, "controller_ownership_renewal_timestamp_seconds", "gauge", labels, m.renewed.Unix())
	metric(w, "controller_cleanup_leases", "gauge", labels, m.cleaning)
	metric(w, "controller_cleanup_oldest_seconds", "gauge", labels, age(m.oldestCleanup))
	metric(w, "cache_routed_total", "counter", labels, m.guest.Routed)
	metric(w, "cache_fallback_total", "counter", labels, m.guest.Fallback)
	metric(w, "cache_relay_errors_total", "counter", labels, m.guest.RelayErrors)
	if m.extractionKnown {
		metric(w, "cache_extraction_available_bytes", "gauge", labels, *m.lastGuest.ExtractionBytes)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
