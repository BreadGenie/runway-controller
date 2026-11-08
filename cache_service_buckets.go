package main

import (
	"context"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/BreadGenie/runway/cache/grant"
	"github.com/BreadGenie/runway/cache/store"
)

// This durable registry is authoritative for this service prefix, including
// retired teams. No tenant is discovered from guest input or an unsigned grant.
type cacheManagedConfig struct {
	CentralURL       string           `json:"central_url"`
	CentralTokenFile string           `json:"central_token_file"`
	StorageCAFile    string           `json:"storage_ca_file"`
	Prefix           string           `json:"prefix"`
	Teams            map[int64]string `json:"teams"`
}

type cacheBucketEntry struct {
	config store.GarageConfig
	until  time.Time
}

type cacheBucketProvider struct {
	central     cacheCentralHTTPS
	teams       map[int64]string
	ca          string
	mu          sync.Mutex
	entries     map[int64]cacheBucketEntry
	generations map[int64]uint64
}

func (p *cacheBucketProvider) Bucket(ctx context.Context, tenant int64) (store.GarageConfig, error) {
	p.mu.Lock()
	team, ok := p.teams[tenant]
	if !ok {
		p.mu.Unlock()
		return store.GarageConfig{}, grant.ErrInvalid
	}
	if entry, ok := p.entries[tenant]; ok && time.Now().Before(entry.until) {
		p.mu.Unlock()
		return entry.config, nil
	}
	generation := p.generations[tenant]
	p.mu.Unlock()
	config, err := p.central.bucket(ctx, team, tenant, p.ca)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil || p.generations[tenant] != generation {
		delete(p.entries, tenant)
		return store.GarageConfig{}, grant.ErrInvalid
	}
	if p.entries == nil {
		p.entries = map[int64]cacheBucketEntry{}
	}
	p.entries[tenant] = cacheBucketEntry{config, time.Now().Add(30 * time.Second)}
	return config, nil
}

// Invalidate makes the next operation re-read Central after a storage auth error.
func (p *cacheBucketProvider) Invalidate(tenant int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, tenant)
	if p.generations == nil {
		p.generations = map[int64]uint64{}
	}
	p.generations[tenant]++
}

func (p *cacheBucketProvider) Tenants(ctx context.Context) ([]int64, error) {
	ids := make([]int64, 0, len(p.teams))
	for id := range p.teams {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// Force a fresh Central resolution for the entire registry before reset.
	// No deletion can happen if even the final tenant fails enumeration.
	for _, id := range ids {
		p.Invalidate(id)
		if _, err := p.Bucket(ctx, id); err != nil {
			return nil, grant.ErrInvalid
		}
	}
	return ids, nil
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
