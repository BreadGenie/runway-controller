package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorageBudgetScalesWithBytesAndCaps(t *testing.T) {
	budget := DefaultStorageBudget
	if err := budget.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := budget.operation(0); got != 12*time.Second {
		t.Fatalf("empty operation budget %s", got)
	}
	// The 584 MB BLR finalize took about 9.6 s; the fixed 12 s budget left a
	// 2.4 s margin. The scaled budget adds 584388223 / 16 MiB/s (about 34.8 s).
	measured := budget.operation(584388223)
	if measured < 46*time.Second || measured > 47*time.Second {
		t.Fatalf("584 MB budget %s", measured)
	}
	if got := budget.operation(1 << 40); got != 300*time.Second {
		t.Fatalf("operation cap %s", got)
	}
	// About 1.5 GB over four objects took 5.4 s to recover on BLR.
	recovery := budget.recovery(4, 1500000000)
	if recovery < 100*time.Second || recovery > 106*time.Second {
		t.Fatalf("recovery budget %s", recovery)
	}
	if got := budget.recovery(10000, 1<<40); got != 900*time.Second {
		t.Fatalf("recovery cap %s", got)
	}
	for name, bad := range map[string]StorageBudget{
		"zero":               {},
		"base too large":     {BaseSeconds: 121, MinBytesPerSecond: 1 << 20, MaxSeconds: 200, RecoveryMaxSeconds: 200},
		"slow rate":          {BaseSeconds: 1, MinBytesPerSecond: 1<<20 - 1, MaxSeconds: 10, RecoveryMaxSeconds: 10},
		"fast rate":          {BaseSeconds: 1, MinBytesPerSecond: 10<<30 + 1, MaxSeconds: 10, RecoveryMaxSeconds: 10},
		"max below base":     {BaseSeconds: 10, MinBytesPerSecond: 1 << 20, MaxSeconds: 9, RecoveryMaxSeconds: 10},
		"recovery below max": {BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: 10, RecoveryMaxSeconds: 9},
		"unbounded":          {BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: 3601, RecoveryMaxSeconds: 3601},
		"unbounded recovery": {BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: 10, RecoveryMaxSeconds: 3601},
		"negative base":      {BaseSeconds: -1, MinBytesPerSecond: 1 << 20, MaxSeconds: 10, RecoveryMaxSeconds: 10},
		"negative rate":      {BaseSeconds: 1, MinBytesPerSecond: -1, MaxSeconds: 10, RecoveryMaxSeconds: 10},
		"negative max":       {BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: -1, RecoveryMaxSeconds: 10},
		"negative recovery":  {BaseSeconds: 1, MinBytesPerSecond: 1 << 20, MaxSeconds: 10, RecoveryMaxSeconds: -1},
	} {
		if bad.Validate() == nil {
			t.Fatalf("%s budget accepted", name)
		}
	}
}

func TestStateDirectoryCustodyRejectsLinksWithoutSideEffects(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "new-state")} {
		if f, err := openPrivateStateDir(path); err == nil {
			f.Close()
			t.Fatalf("symlink accepted: %s", path)
		}
	}
	files, err := os.ReadDir(outside)
	if err != nil || len(files) != 0 {
		t.Fatal("created state through symlink")
	}
	if f, err := openPrivateStateDir(outside); err == nil {
		f.Close()
		t.Fatal("non-private directory accepted")
	}
	after, err := os.Stat(outside)
	if err != nil || after.Mode() != before.Mode() {
		t.Fatal("existing directory chmodded")
	}
}
