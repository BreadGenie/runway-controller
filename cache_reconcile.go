package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	cacheReconcilePoll = 15 * time.Second
	cacheCreateWindow  = 2 * time.Minute
)

type cacheReconcileConfig struct {
	StateDir string                 `json:"state_dir"`
	Metrics  string                 `json:"metrics_listen"`
	Regions  []cacheReconcileRegion `json:"regions"`
}

type cacheReconcileRegion struct {
	cacheVMShape
	Region          string        `json:"region"`
	RunwayURL       string        `json:"runway_url"`
	RunwayKeyFile   string        `json:"runway_api_key_file"`
	AtlasURL        string        `json:"atlas_url"`
	AtlasTokenFile  string        `json:"atlas_token_file"`
	Firewall        cacheFirewall `json:"firewall"`
	SiteCA          string        `json:"runway_site_ca_base64"`
	StorageCA       string        `json:"runway_storage_ca_base64"`
	EnrolmentSecond int           `json:"enrolment_timeout_seconds"`
}

var cacheRunbookFirewall = cacheFirewall{
	Enabled: true,
	Inbound: []cacheFirewallRule{
		{Protocol: "tcp", Ports: "8443", CIDRs: []string{"fdaa::/16"}},
		{Protocol: "icmp", CIDRs: []string{"fdaa::/16"}},
	},
	Outbound: []cacheFirewallRule{
		{Protocol: "any", CIDRs: []string{"0.0.0.0/0", "::/0"}},
	},
}

func loadCacheReconcileConfig(path string) (cacheReconcileConfig, error) {
	const limit = 1 << 20
	raw, err := readProtectedCacheFileLimit(path, true, limit)
	if err != nil || len(raw) > limit {
		return cacheReconcileConfig{}, errors.New("invalid cache-reconcile configuration")
	}
	return decodeCacheReconcileConfig(raw)
}

func decodeCacheReconcileConfig(raw []byte) (cacheReconcileConfig, error) {
	var c cacheReconcileConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || len(c.Regions) == 0 {
		return c, errors.New("invalid cache-reconcile configuration")
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/runway/cache-reconcile"
	}
	if !filepath.IsAbs(c.StateDir) {
		return c, errors.New("cache-reconcile requires an absolute state_dir")
	}
	seen := make(map[string]bool)
	for i := range c.Regions {
		r := &c.Regions[i]
		if !validCacheGitHubPart(r.Region) || seen[r.Region] || !cacheHTTPSOrigin(r.RunwayURL) ||
			strings.ContainsAny(r.RunwayURL, "'\r\n") || !cacheHTTPSOrigin(r.AtlasURL) ||
			!filepath.IsAbs(r.RunwayKeyFile) || !filepath.IsAbs(r.AtlasTokenFile) ||
			r.ImageID == "" || r.CPU <= 0 || r.Memory <= 0 || r.Disk < 8192 ||
			!reflect.DeepEqual(r.Firewall, cacheRunbookFirewall) ||
			r.EnrolmentSecond < 0 || r.EnrolmentSecond > 900 {
			return c, fmt.Errorf("invalid cache-reconcile region at index %d", i)
		}
		seen[r.Region] = true
		r.RunwayURL = strings.TrimSuffix(r.RunwayURL, "/")
		r.AtlasURL = strings.TrimSuffix(r.AtlasURL, "/")
		if r.EnrolmentSecond == 0 {
			r.EnrolmentSecond = 600
		}
	}
	return c, nil
}

type cacheReconcileJournal struct {
	AtlasURL     string        `json:"atlas_url"`
	RunwayURL    string        `json:"runway_url"`
	Attempt      *cacheAttempt `json:"attempt,omitempty"`
	Attempts     uint64        `json:"attempts"`
	Replacements uint64        `json:"replacements"`
}

type cacheAttempt struct {
	Intent cacheVMIntent `json:"intent"`
	At     time.Time     `json:"at"`
	Phase  string        `json:"phase"`
	VM     string        `json:"vm,omitempty"`
}

type cacheReconciler struct {
	config        cacheReconcileRegion
	atlas, runway cacheProvisionAPI
	journal       cacheReconcileJournal
	path          string
	now           func() time.Time
}

func (r *cacheReconciler) load() error {
	r.journal = cacheReconcileJournal{AtlasURL: r.config.AtlasURL, RunwayURL: r.config.RunwayURL}
	if _, err := os.Lstat(r.path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	raw, err := readProtectedCacheFile(r.path, true)
	if err != nil {
		return errors.New("cache-reconcile journal unreadable")
	}
	return r.decodeJournal(raw)
}

func (r *cacheReconciler) decodeJournal(raw []byte) error {
	if len(raw) > 4096 || json.Unmarshal(raw, &r.journal) != nil ||
		r.journal.AtlasURL != r.config.AtlasURL || r.journal.RunwayURL != r.config.RunwayURL {
		return errors.New("cache-reconcile journal unreadable or endpoint changed")
	}
	if a := r.journal.Attempt; a != nil {
		if a.At.IsZero() || a.Intent.Tags["role"] != "runway-cache" || a.Intent.Tags["region"] != r.config.Region ||
			!canonicalCacheUUID(a.Intent.Tags["attempt"]) {
			return errors.New("invalid cache-reconcile journal attempt")
		}
		switch a.Phase {
		case "preparing", "submitted", "failed":
			if a.VM == "" {
				return nil
			}
		case "current", "revoking", "deleting", "confirming":
			if validCacheGitHubPart(a.VM) {
				return nil
			}
		}
		return errors.New("invalid cache-reconcile journal phase")
	}
	return nil
}

func (r *cacheReconciler) save(a *cacheAttempt) error {
	next := r.journal
	next.Attempt = a
	if a != nil && a.Phase == "submitted" {
		next.Attempts++
	}
	if a != nil && a.Phase == "revoking" {
		next.Replacements++
	}
	raw, err := json.Marshal(next)
	if err != nil || writeCacheCredential(r.path, string(raw)) != nil {
		// A failed directory sync can leave the renamed file visible. Keep the
		// last confirmed in-memory state and retry its transition before any write.
		return errors.New("journal_write")
	}
	r.journal = next
	return nil
}

func (r *cacheReconciler) step(ctx context.Context) error {
	list, err := r.atlas.list(ctx, r.config.Region, "")
	if err != nil {
		return err
	}
	if len(list) > 1 {
		return errors.New("duplicate_vms")
	}
	a := r.journal.Attempt
	if a != nil {
		switch a.Phase {
		case "submitted":
			return r.recoverCreate(ctx, *a, list)
		case "current":
			return r.checkCurrent(ctx, *a, list)
		case "revoking", "deleting", "confirming":
			if len(list) == 1 && list[0].ID != a.VM {
				return errors.New("unmanaged_vm")
			}
			return r.replace(ctx, *a)
		}
	}

	if len(list) != 0 {
		return errors.New("unmanaged_vm")
	}
	status, err := r.runway.cacheStatus(ctx, r.config.Region)
	if err != nil {
		return err
	}
	if *status.Enrolled || status.Mesh != "" || status.CA != "" {
		return errors.New("unmanaged_enrolment")
	}
	if a == nil || a.Phase == "failed" || a.Intent.cacheVMShape != r.config.cacheVMShape {
		a = &cacheAttempt{
			Intent: cacheVMIntent{
				cacheVMShape: r.config.cacheVMShape, Firewall: r.config.Firewall, IsPrivileged: true,
				Tags: map[string]string{"role": "runway-cache", "region": r.config.Region, "attempt": uuid.NewString()},
			},
			At: r.now(), Phase: "preparing",
		}
		if err := r.save(a); err != nil {
			return err
		}
	}
	return r.create(ctx, *a)
}

func (r *cacheReconciler) create(ctx context.Context, a cacheAttempt) error {
	token, err := r.runway.issueEnrolment(ctx, r.config.Region)
	if err != nil {
		return err
	}
	a.At, a.Phase = r.now(), "submitted"
	if err := r.save(&a); err != nil {
		return err
	}
	payload := struct {
		cacheVMIntent
		UserData string `json:"user_data"`
	}{a.Intent, cacheVMUserData(r.config, token)}
	var vm cacheAtlasVM
	if err := r.atlas.request(ctx, "POST", cacheAtlasVMs, payload, &vm); err != nil {
		return err
	}
	return r.adopt(a, vm)
}

func (r *cacheReconciler) adopt(a cacheAttempt, vm cacheAtlasVM) error {
	if !vm.tagged(r.config.Region) || vm.Tags["attempt"] != a.Intent.Tags["attempt"] {
		return errors.New("atlas_scope")
	}
	a.VM, a.Phase = vm.ID, "current"
	if vm.CreatedAt > a.At.Unix() && vm.CreatedAt <= r.now().Unix() {
		a.At = time.Unix(vm.CreatedAt, 0)
	}
	return r.save(&a)
}

func (r *cacheReconciler) recoverCreate(ctx context.Context, a cacheAttempt, regionVMs []cacheAtlasVM) error {
	found, err := r.atlas.list(ctx, r.config.Region, a.Intent.Tags["attempt"])
	if err != nil {
		return err
	}
	if len(found) > 1 || (len(found) == 1 && len(regionVMs) == 1 && found[0].ID != regionVMs[0].ID) {
		return errors.New("duplicate_vms")
	}
	if len(found) == 1 {
		return r.adopt(a, found[0])
	}
	if len(regionVMs) != 0 {
		return errors.New("unmanaged_vm")
	}
	if r.now().Sub(a.At) >= cacheCreateWindow {
		a.Phase = "failed"
		return r.save(&a)
	}
	return nil
}

func (r *cacheReconciler) owns(a cacheAttempt, vm cacheAtlasVM) bool {
	return vm.ID == a.VM && vm.tagged(r.config.Region) && vm.Tags["attempt"] == a.Intent.Tags["attempt"]
}

func (r *cacheReconciler) checkCurrent(ctx context.Context, a cacheAttempt, list []cacheAtlasVM) error {
	if len(list) == 1 && !r.owns(a, list[0]) {
		return errors.New("unmanaged_vm")
	}
	vm, err := r.atlas.detail(ctx, a.VM)
	if errors.Is(err, errCacheVMAbsent) {
		return r.beginReplacement(ctx, a)
	}
	if err != nil {
		return err
	}
	if !r.owns(a, vm.cacheAtlasVM) {
		return errors.New("atlas_scope")
	}
	if len(list) == 0 {
		return errors.New("atlas_scope")
	}
	shape := list[0].cacheVMShape
	if shape.ImageID == "" || shape.CPU <= 0 || shape.Memory <= 0 || shape.Disk <= 0 {
		return errors.New("atlas_response")
	}
	if shape != r.config.cacheVMShape || !reflect.DeepEqual(a.Intent.Firewall, r.config.Firewall) {
		return r.beginReplacement(ctx, a)
	}
	if vm.State == "" {
		return errors.New("atlas_response")
	}
	expired := r.now().Sub(a.At) >= time.Duration(r.config.EnrolmentSecond)*time.Second
	switch vm.State {
	case "running":
		status, err := r.runway.cacheStatus(ctx, r.config.Region)
		if err != nil {
			return err
		}
		mesh, meshErr := netip.ParseAddr(vm.Network.Mesh)
		enrolled, enrolledErr := netip.ParseAddr(status.Mesh)
		if meshErr == nil && enrolledErr == nil && mesh == enrolled && *status.Enrolled &&
			status.CA != "" && !*status.TokenPending && status.TokenExpires == nil {
			return nil
		}
	case "pending", "unknown", "starting", "migrating":
		// Atlas can report a transition before the host starts the guest.
	default:
		return r.beginReplacement(ctx, a)
	}
	if expired {
		return r.beginReplacement(ctx, a)
	}
	return nil
}

func (r *cacheReconciler) beginReplacement(ctx context.Context, a cacheAttempt) error {
	a.Phase = "revoking"
	if err := r.save(&a); err != nil {
		return err
	}
	return r.replace(ctx, a)
}

func (r *cacheReconciler) replace(ctx context.Context, a cacheAttempt) error {
	vm, err := r.atlas.detail(ctx, a.VM)
	absent := errors.Is(err, errCacheVMAbsent)
	if !absent && err != nil {
		return err
	}
	if !absent && !r.owns(a, vm.cacheAtlasVM) {
		return errors.New("atlas_scope")
	}
	if a.Phase == "revoking" {
		if err := r.runway.request(ctx, "POST", "/api/method/runway.api.cache_revoke", map[string]string{"region": r.config.Region}, nil); err != nil {
			return err
		}
		a.Phase = "deleting"
		if err := r.save(&a); err != nil {
			return err
		}
	}
	if absent {
		return r.save(nil)
	}
	if a.Phase == "confirming" || vm.State == "terminating" {
		return nil
	}
	err = r.atlas.request(ctx, "DELETE", cacheAtlasVMs+"/"+a.VM, nil, nil)
	if err != nil && !errors.Is(err, errCacheVMAbsent) {
		return err
	}
	a.Phase = "confirming"
	return r.save(&a)
}

type cacheReconcileObservation struct {
	region, vm, lastError  string
	attempts, replacements uint64
}

type cacheReconcileMetrics struct {
	mu   sync.Mutex
	rows []cacheReconcileObservation
}

func (m *cacheReconcileMetrics) write(w io.Writer) {
	m.mu.Lock()
	rows := append([]cacheReconcileObservation(nil), m.rows...)
	m.mu.Unlock()
	fmt.Fprintln(w, "# TYPE runway_cache_reconcile_current_vm gauge")
	fmt.Fprintln(w, "# TYPE runway_cache_reconcile_attempts_total counter")
	fmt.Fprintln(w, "# TYPE runway_cache_reconcile_replacements_total counter")
	fmt.Fprintln(w, "# TYPE runway_cache_reconcile_last_error gauge")
	for _, row := range rows {
		region := metricLabel(row.region)
		if row.vm != "" {
			fmt.Fprintf(w, "runway_cache_reconcile_current_vm{region=%s,vm=%s} 1\n", region, metricLabel(row.vm))
		}
		fmt.Fprintf(w, "runway_cache_reconcile_attempts_total{region=%s} %d\n", region, row.attempts)
		fmt.Fprintf(w, "runway_cache_reconcile_replacements_total{region=%s} %d\n", region, row.replacements)
		fmt.Fprintf(w, "runway_cache_reconcile_last_error{region=%s,class=%s} 1\n", region, metricLabel(row.lastError))
	}
}

func cacheReconcileDelay(previous time.Duration, err error) time.Duration {
	if err == nil {
		return cacheReconcilePoll
	}
	return min(2*time.Minute, max(cacheReconcilePoll, previous)*2)
}

func (r *cacheReconciler) run(ctx context.Context, report func(cacheReconcileObservation)) {
	delay, lastError := cacheReconcilePoll, ""
	for ctx.Err() == nil {
		err := r.step(ctx)
		if ctx.Err() != nil {
			return
		}
		row := cacheReconcileObservation{region: r.config.Region, lastError: "none", attempts: r.journal.Attempts, replacements: r.journal.Replacements}
		if a := r.journal.Attempt; a != nil {
			row.vm = a.VM
		}
		if err != nil {
			row.lastError = err.Error()
			if row.lastError != lastError {
				log.Printf("cache-reconcile region=%s error=%s", row.region, row.lastError)
			}
		}
		lastError = row.lastError
		report(row)
		delay = cacheReconcileDelay(delay, err)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func cacheProvisionCredential(path string) func() ([]byte, error) {
	return func() ([]byte, error) { return readProtectedCacheFile(path, true) }
}

func runCacheReconcile(path string) error {
	c, err := loadCacheReconcileConfig(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil || checkCachePrivateDirectory(c.StateDir) != nil {
		return errors.New("cache-reconcile state directory must be root-owned and private")
	}
	// A single process owns the regional journals, including during restarts.
	lock, err := os.OpenFile(filepath.Join(c.StateDir, ".lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cache-reconcile lock unavailable")
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return errors.New("cache-reconcile state directory already in use")
	}
	atlasClient, _ := cacheProvisionClient("")
	reconcilers := make([]cacheReconciler, 0, len(c.Regions))
	for _, region := range c.Regions {
		siteClient, err := cacheProvisionClient(region.SiteCA)
		if _, storageErr := cacheProvisionClient(region.StorageCA); err != nil || storageErr != nil {
			return fmt.Errorf("invalid cache-reconcile CA for region %s", region.Region)
		}
		r := cacheReconciler{
			config: region, now: time.Now, path: filepath.Join(c.StateDir, region.Region+".json"),
			atlas:  cacheProvisionAPI{origin: region.AtlasURL, credential: cacheProvisionCredential(region.AtlasTokenFile), client: atlasClient, atlas: true},
			runway: cacheProvisionAPI{origin: region.RunwayURL, credential: cacheProvisionCredential(region.RunwayKeyFile), client: siteClient},
		}
		if err := r.load(); err != nil {
			return fmt.Errorf("region %s: %w", region.Region, err)
		}
		reconcilers = append(reconcilers, r)
	}
	metrics := cacheReconcileMetrics{rows: make([]cacheReconcileObservation, len(reconcilers))}
	for i, r := range reconcilers {
		metrics.rows[i] = cacheReconcileObservation{region: r.config.Region, lastError: "none"}
	}
	closeMetrics, err := startMetrics(c.Metrics, metrics.write)
	if err != nil {
		return err
	}
	defer closeMetrics()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var workers sync.WaitGroup
	for i := range reconcilers {
		workers.Go(func() {
			reconcilers[i].run(ctx, func(row cacheReconcileObservation) {
				metrics.mu.Lock()
				metrics.rows[i] = row
				metrics.mu.Unlock()
			})
		})
	}
	workers.Wait()
	return nil
}
