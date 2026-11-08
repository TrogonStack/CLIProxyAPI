package auth

import (
	"context"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
)

// refreshLeaseTTL is how long a replica holds the right to refresh a
// RefreshLease credential. It covers a provider call, and a replica that dies
// holding it frees the credential by itself when it runs out.
const refreshLeaseTTL = 30 * time.Second

// ErrRefreshLeaseHeld is returned by a RefreshLeaser when another replica holds the lease.
var ErrRefreshLeaseHeld = errors.New("refresh lease is held")

// RefreshLeaser is implemented by a store that can hand out refresh leases
// shared by every replica using it.
type RefreshLeaser interface {
	// AcquireRefreshLease takes the lease for a credential for at most ttl.
	// It returns ErrRefreshLeaseHeld when another replica holds it. The
	// returned function gives the lease back and is safe to call after it ran out.
	AcquireRefreshLease(ctx context.Context, authID string, ttl time.Duration) (release func(), err error)
}

func noRelease() {}

// acquireRefreshLease takes the lease a RefreshLease provider needs before
// refreshing. held is true when another replica holds it and the caller should
// leave the refresh to that replica. Everything else proceeds: a provider
// without the policy, a store without leases, or a lease that cannot be taken
// for any other reason, since the grant is reusable and a second refresh is
// harmless. needed is for callers that cannot do without a fresh token: they
// refresh even while another replica holds the lease.
func acquireRefreshLease(ctx context.Context, store Store, auth *Auth, exec ProviderExecutor, needed bool) (release func(), held bool) {
	if RefreshPolicyOf(exec) != RefreshLease {
		return noRelease, false
	}
	leaser, ok := store.(RefreshLeaser)
	if !ok {
		return noRelease, false
	}
	release, err := leaser.AcquireRefreshLease(ctx, auth.ID, refreshLeaseTTL)
	switch {
	case err == nil:
		return release, false
	case errors.Is(err, ErrRefreshLeaseHeld):
		return noRelease, !needed
	default:
		log.WithFields(log.Fields{"auth_id": auth.ID, "provider": auth.Provider}).Debugf("refresh lease unavailable, refreshing without it: %v", err)
		return noRelease, false
	}
}
