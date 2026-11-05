package grant

import (
	"testing"
	"time"
)

func TestIssueBindsJobAndCapsLifetime(t *testing.T) {
	key, claims, clock, verifier := fixture(t)
	a := Authorization{
		RegionID: claims.RegionID, JobID: claims.JobID, RunID: claims.RunID, RunAttempt: claims.RunAttempt, RepositoryID: claims.RepositoryID,
		ReadRefs: claims.ReadRefs, WriteRef: claims.WriteRef, Deadline: clock.Earliest.Add(5 * time.Minute),
	}
	token, err := Issue(key, claims.Audience, a, clock)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifier.Verify(token, clock)
	if err != nil || got.ExpiresAt != a.Deadline.Unix() || got.JobID != a.JobID || got.RunID != a.RunID || got.RequestID != 0 {
		t.Fatal("job or deadline binding changed", err)
	}
	for _, change := range []func(*Authorization){
		func(a *Authorization) { a.RegionID = "" },
		func(a *Authorization) { a.JobID = "not-a-uuid" },
		func(a *Authorization) { a.RunID = 0 },
		func(a *Authorization) { a.RunAttempt = 0 },
		func(a *Authorization) { a.RepositoryID = 0 },
		func(a *Authorization) { a.ReadRefs, a.WriteRef = nil, "" },
		func(a *Authorization) { a.Deadline = clock.Latest },
	} {
		bad := a
		change(&bad)
		if _, err := Issue(key, claims.Audience, bad, clock); err == nil {
			t.Fatal("invalid authorization received a grant")
		}
	}
	a.Deadline = clock.Earliest.Add(time.Hour)
	token, err = Issue(key, claims.Audience, a, clock)
	if err != nil {
		t.Fatal(err)
	}
	got, err = verifier.Verify(token, clock)
	if err != nil || got.ExpiresAt != clock.Earliest.Add(maxLifetime).Unix() {
		t.Fatal("maximum cache lifetime not enforced", err)
	}
	stop := time.Unix(got.ExpiresAt, 0)
	if _, err := verifier.Verify(token, Clock{Earliest: stop.Add(-time.Second), Latest: stop}); err == nil {
		t.Fatal("clock interval touching the 30-minute stop accepted")
	}
	// Integer-second signing must never round a short cap upwards.
	a.Deadline = clock.Latest.Add(2500 * time.Millisecond)
	token, err = Issue(key, claims.Audience, a, clock)
	if err != nil {
		t.Fatal(err)
	}
	got, err = verifier.Verify(token, clock)
	if err != nil || got.ExpiresAt != a.Deadline.Unix() || time.Unix(got.ExpiresAt, 0).After(a.Deadline) {
		t.Fatal("short cache cap was extended", err)
	}
	a.Deadline = clock.Latest.Add(500 * time.Millisecond)
	if _, err := Issue(key, claims.Audience, a, clock); err == nil {
		t.Fatal("cap with no whole-second lifetime past uncertainty accepted")
	}
}
