package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fakeAdjtimex(result int, status int32, maxErrorMicros int64, at time.Time, err error) func(*unix.Timex) (int, error) {
	return func(state *unix.Timex) (int, error) {
		if state.Modes != 0 {
			return 0, errors.New("observer attempted to adjust the clock")
		}
		state.Status = status
		state.Maxerror = maxErrorMicros
		state.Time = unix.Timeval{Sec: at.Unix(), Usec: int64(at.Nanosecond() / 1000)}
		return result, err
	}
}

func TestCacheHostClockUsesKernelErrorBound(t *testing.T) {
	at := time.Date(2026, 11, 4, 17, 5, 46, 123456000, time.UTC)
	clock, err := cacheHostClock{adjtimex: fakeAdjtimex(unix.TIME_OK, 0, 77132, at, nil)}.observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	half := 77132*time.Microsecond + cacheHostClockMargin
	if !clock.Earliest.Equal(at.Add(-half)) || !clock.Latest.Equal(at.Add(half)) || !clock.Valid() {
		t.Fatalf("wrong interval %s .. %s", clock.Earliest, clock.Latest)
	}
	// Leap-second states are synchronized; only TIME_ERROR and STA_UNSYNC refuse.
	if _, err := (cacheHostClock{adjtimex: fakeAdjtimex(unix.TIME_INS, 0, 1000, at, nil)}).observe(context.Background()); err != nil {
		t.Fatal("synchronized leap state refused", err)
	}
	nano := func(state *unix.Timex) (int, error) {
		state.Status = unix.STA_NANO
		state.Time = unix.Timeval{Sec: at.Unix(), Usec: int64(at.Nanosecond())}
		return unix.TIME_OK, nil
	}
	if clock, err := (cacheHostClock{adjtimex: nano}).observe(context.Background()); err != nil || !clock.Earliest.Equal(at.Add(-cacheHostClockMargin)) {
		t.Fatal("nanosecond kernel time misread", clock, err)
	}
}

func TestCacheHostClockRefusesUnsynchronizedOrUncertainTime(t *testing.T) {
	at := time.Date(2026, 11, 4, 17, 5, 46, 0, time.UTC)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, test := range map[string]struct {
		source func(*unix.Timex) (int, error)
		ctx    context.Context
	}{
		"time error":       {fakeAdjtimex(unix.TIME_ERROR, 0, 1000, at, nil), context.Background()},
		"unsync status":    {fakeAdjtimex(unix.TIME_OK, unix.STA_UNSYNC, 1000, at, nil), context.Background()},
		"unknown state":    {fakeAdjtimex(unix.TIME_ERROR+1, 0, 1000, at, nil), context.Background()},
		"negative state":   {fakeAdjtimex(-1, 0, 1000, at, nil), context.Background()},
		"syscall failure":  {fakeAdjtimex(unix.TIME_OK, 0, 1000, at, errors.New("EPERM")), context.Background()},
		"too uncertain":    {fakeAdjtimex(unix.TIME_OK, 0, int64(cacheHostClockMaxError/time.Microsecond)+1, at, nil), context.Background()},
		"negative error":   {fakeAdjtimex(unix.TIME_OK, 0, -1, at, nil), context.Background()},
		"epoch":            {fakeAdjtimex(unix.TIME_OK, 0, 1000, time.Unix(0, 0), nil), context.Background()},
		"canceled":         {fakeAdjtimex(unix.TIME_OK, 0, 1000, at, nil), canceled},
		"missing observer": {nil, context.Background()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (cacheHostClock{adjtimex: test.source}).observe(test.ctx); err == nil {
				t.Fatal("unqualified host time accepted")
			}
		})
	}
	bad := func(state *unix.Timex) (int, error) {
		state.Time = unix.Timeval{Sec: at.Unix(), Usec: 1000000}
		return unix.TIME_OK, nil
	}
	if _, err := (cacheHostClock{adjtimex: bad}).observe(context.Background()); err == nil {
		t.Fatal("out-of-range fraction accepted")
	}
}
