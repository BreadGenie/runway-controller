package store

import (
	"errors"
	"time"
)

// StorageBudget scales Garage deadlines with the bytes moved. One operation may
// take Base plus size/MinBytesPerSecond, never more than Max. Startup recovery
// shares one deadline for every indexed object: Base plus the recovered bytes
// at the same minimum rate plus one second per object, never more than
// RecoveryMax. Parent deadlines still take precedence.
type StorageBudget struct {
	BaseSeconds        int64 `json:"base_seconds"`
	MinBytesPerSecond  int64 `json:"min_bytes_per_second"`
	MaxSeconds         int64 `json:"max_seconds"`
	RecoveryMaxSeconds int64 `json:"recovery_max_seconds"`
}

// DefaultStorageBudget adds time at 16 MiB/s, a quarter of the slowest
// finalize measured on BLR (584 MB in 9.6 s), over a 12-second floor.
var DefaultStorageBudget = StorageBudget{BaseSeconds: 12, MinBytesPerSecond: 16 << 20, MaxSeconds: 300, RecoveryMaxSeconds: 900}

// Validate refuses budgets that are unbounded, inverted or too small to be useful.
func (budget StorageBudget) Validate() error {
	if budget.BaseSeconds < 1 || budget.BaseSeconds > 120 ||
		budget.MinBytesPerSecond < 1<<20 || budget.MinBytesPerSecond > 10<<30 ||
		budget.MaxSeconds < budget.BaseSeconds || budget.MaxSeconds > 3600 ||
		budget.RecoveryMaxSeconds < budget.MaxSeconds || budget.RecoveryMaxSeconds > 3600 {
		return errors.New("invalid storage budget")
	}
	return nil
}

func (budget StorageBudget) transfer(bytes int64) time.Duration {
	bytes = max(bytes, 0)
	whole, rest := bytes/budget.MinBytesPerSecond, bytes%budget.MinBytesPerSecond
	return time.Duration(whole)*time.Second + time.Duration(rest)*time.Second/time.Duration(budget.MinBytesPerSecond)
}

// operation is the deadline for moving one object of the given size.
func (budget StorageBudget) operation(bytes int64) time.Duration {
	limit := time.Duration(budget.MaxSeconds) * time.Second
	if bytes/budget.MinBytesPerSecond >= budget.MaxSeconds {
		return limit
	}
	return min(time.Duration(budget.BaseSeconds)*time.Second+budget.transfer(bytes), limit)
}

// recovery is the one shared deadline for verifying every indexed object.
func (budget StorageBudget) recovery(objects int, bytes int64) time.Duration {
	limit := time.Duration(budget.RecoveryMaxSeconds) * time.Second
	if objects < 0 || int64(objects) >= budget.RecoveryMaxSeconds || bytes/budget.MinBytesPerSecond >= budget.RecoveryMaxSeconds {
		return limit
	}
	return min(time.Duration(budget.BaseSeconds)*time.Second+budget.transfer(bytes)+time.Duration(objects)*time.Second, limit)
}
