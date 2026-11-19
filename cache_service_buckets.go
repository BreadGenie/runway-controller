package main

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/BreadGenie/runway-controller/cache/grant"
	"github.com/BreadGenie/runway-controller/cache/store"
)

type cacheManagedConfig struct {
	CentralCAFile     string `json:"central_ca_file,omitempty"`
	CentralURL        string `json:"central_url"`
	CentralTokenFile  string `json:"central_token_file"`
	StorageCAFile     string `json:"storage_ca_file"`
	CleanupStaleHours int    `json:"cleanup_stale_hours,omitempty"`
}

type cacheBucketEntry struct {
	config store.GarageConfig
	until  time.Time
}

type cacheBucketProvider struct {
	central    cacheCentralHTTPS
	credential func() (string, error)
	ca         string
	mu         sync.Mutex
	entries    map[int64]cacheBucketEntry
	generation uint64
}

func (p *cacheBucketProvider) Bucket(ctx context.Context, tenant int64) (store.GarageConfig, error) {
	p.mu.Lock()
	if entry, ok := p.entries[tenant]; ok && time.Now().Before(entry.until) {
		p.mu.Unlock()
		return entry.config, nil
	}
	generation := p.generation
	p.mu.Unlock()
	central := p.central
	if p.credential != nil {
		token, err := p.credential()
		if err != nil {
			return store.GarageConfig{}, grant.ErrInvalid
		}
		central.token = token
	}
	config, err := central.bucket(ctx, "", tenant, p.ca)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil || p.generation != generation {
		delete(p.entries, tenant)
		return store.GarageConfig{}, grant.ErrInvalid
	}
	if p.entries == nil {
		p.entries = map[int64]cacheBucketEntry{}
	}
	if len(p.entries) >= 256 {
		clear(p.entries)
	}
	p.entries[tenant] = cacheBucketEntry{config, time.Now().Add(30 * time.Second)}
	return config, nil
}

// Invalidate makes the next operation re-read Central after a storage auth error.
func (p *cacheBucketProvider) Invalidate(tenant int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, tenant)
	p.generation++
}

// The service gates setup and renewals on bucket availability as well as the
// lease. Credentials stay inside the regional processes.
type cacheCentralWithBuckets struct{ cacheCentralHTTPS }

func (c cacheCentralWithBuckets) lease(ctx context.Context, address netip.Addr) (cacheServiceLease, error) {
	lease, err := c.cacheCentralHTTPS.lease(ctx, address)
	if err != nil {
		return lease, err
	}
	if _, err = c.bucket(ctx, lease.Team, int64(lease.TenantID), ""); err != nil {
		return cacheServiceLease{}, grant.ErrInvalid
	}
	return lease, nil
}
