package grant

import (
	"crypto/ed25519"
	"time"
)

// Authorization is the job the host has verified with its own sources. It is
// not a wire schema and must never be decoded from guest input. IssueSetup
// fills RepositoryID and the refs from the repository policy.
type Authorization struct {
	RegionID          string
	JobID             string
	RequestID         int64
	RepositoryID      int64
	RunID, RunAttempt int64
	ReadRefs          []string
	WriteRef          string
	Deadline          time.Time // grant expiry ceiling; never extended
}

// Issue signs a grant that expires at the earlier of Deadline and 30 minutes
// after the clock's earliest bound.
func Issue(key ed25519.PrivateKey, audience string, a Authorization, clock Clock) (string, error) {
	if !clock.Valid() || len(key) != ed25519.PrivateKeySize || !a.Deadline.After(clock.Latest) {
		return "", ErrInvalid
	}
	expires := a.Deadline
	if cap := clock.Earliest.Add(maxLifetime); expires.After(cap) {
		expires = cap
	}
	claims := Claims{
		Version: 1, KeyID: KeyID(key.Public().(ed25519.PublicKey)), Audience: audience, RegionID: a.RegionID,
		JobID: a.JobID, RequestID: a.RequestID, RunID: a.RunID, RunAttempt: a.RunAttempt, RepositoryID: a.RepositoryID,
		ReadRefs: append([]string(nil), a.ReadRefs...), WriteRef: a.WriteRef,
		IssuedAt: clock.Earliest.Unix(), ExpiresAt: expires.Unix(),
	}
	return sign(key, claims, clock)
}
