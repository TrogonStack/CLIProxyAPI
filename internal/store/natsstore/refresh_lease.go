package natsstore

import (
	"context"
	"errors"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var _ cliproxyauth.RefreshLeaser = (*Store)(nil)

// AcquireRefreshLease takes the right to refresh a credential for at most ttl.
// The lease lives in its own bucket so the refresh journal, whose claims must
// never expire, keeps a bucket without any key TTL. A lease is never renewed or
// taken over: it runs out on the server and then the next Create wins, so a
// replica that dies mid-refresh frees the credential without anyone judging
// whether it is dead.
func (s *Store) AcquireRefreshLease(ctx context.Context, authID string, ttl time.Duration) (func(), error) {
	rev, err := s.leases.CreateWithTTL(ctx, authID, []byte(s.replicaID), ttl)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, cliproxyauth.ErrRefreshLeaseHeld
		}
		return nil, err
	}
	return func() {
		// Revision-checked, so a lease that ran out and was taken by another
		// replica is not deleted from under it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.leases.Delete(ctx, authID, rev)
	}, nil
}
