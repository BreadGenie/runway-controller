package main

import (
	"context"
	"time"

	"github.com/BreadGenie/runway-controller/cache/grant"
	"golang.org/x/sys/unix"
)

// cacheHostClockMargin widens the kernel's own error bound for scheduling and
// conversion slack between reading the kernel clock and using the interval.
const cacheHostClockMargin = 250 * time.Millisecond

// cacheHostClockMaxError keeps the interval inside grant.Clock's 5 s width.
const cacheHostClockMaxError = 2 * time.Second

// cacheHostClock observes the host kernel clock through adjtimex(2), the
// interface NTP daemons such as chrony use to discipline it. It refuses when
// the kernel reports the clock unsynchronized. The interval is the kernel time
// plus and minus the kernel's maximum error and a fixed margin. It trusts the
// host's NTP configuration; it does not authenticate the time source itself.
type cacheHostClock struct {
	adjtimex func(*unix.Timex) (int, error)
}

func newCacheHostClock() cacheHostClock { return cacheHostClock{adjtimex: unix.Adjtimex} }

func (observer cacheHostClock) observe(ctx context.Context) (grant.Clock, error) {
	if observer.adjtimex == nil {
		return grant.Clock{}, grant.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return grant.Clock{}, err
	}
	var state unix.Timex // zero modes: read only
	result, err := observer.adjtimex(&state)
	if err != nil {
		return grant.Clock{}, grant.ErrInvalid
	}
	return decodeCacheHostClock(result, state)
}

func decodeCacheHostClock(result int, state unix.Timex) (grant.Clock, error) {
	if result == unix.TIME_ERROR || result < unix.TIME_OK || result > unix.TIME_ERROR || state.Status&unix.STA_UNSYNC != 0 {
		return grant.Clock{}, grant.ErrInvalid
	}
	maxError, seconds, fraction := int64(state.Maxerror), int64(state.Time.Sec), int64(state.Time.Usec)
	if maxError < 0 || maxError > int64(cacheHostClockMaxError/time.Microsecond) || seconds <= 0 || fraction < 0 {
		return grant.Clock{}, grant.ErrInvalid
	}
	if state.Status&unix.STA_NANO == 0 {
		fraction *= int64(time.Microsecond)
	}
	if fraction >= int64(time.Second) {
		return grant.Clock{}, grant.ErrInvalid
	}
	now := time.Unix(seconds, fraction).UTC()
	half := time.Duration(maxError)*time.Microsecond + cacheHostClockMargin
	clock := grant.Clock{Earliest: now.Add(-half), Latest: now.Add(half)}
	if !clock.Valid() {
		return grant.Clock{}, grant.ErrInvalid
	}
	return clock, nil
}
