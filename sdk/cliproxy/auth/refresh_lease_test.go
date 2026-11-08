package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// leasingStore is a store that hands out refresh leases as a test directs.
type leasingStore struct {
	queueHostStore
	acquireErr error
	acquired   atomic.Int32
	released   atomic.Int32
	ttl        atomic.Int64
}

func (s *leasingStore) AcquireRefreshLease(_ context.Context, _ string, ttl time.Duration) (func(), error) {
	s.acquired.Add(1)
	s.ttl.Store(int64(ttl))
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	return func() { s.released.Add(1) }, nil
}

// leasedGrantExecutor declares its grant is reusable but refreshed under a lease.
type leasedGrantExecutor struct{ countingRefreshExecutor }

func (*leasedGrantExecutor) RefreshPolicy() RefreshPolicy { return RefreshLease }

func refreshWithLease(t *testing.T, store *leasingStore, force bool) *leasedGrantExecutor {
	t.Helper()
	exec := &leasedGrantExecutor{countingRefreshExecutor{id: queueRefreshProvider}}
	manager, _ := newQueueManager(t, store, exec)
	registerQueueAuth(t, manager, "a", queueRefreshProvider, time.Now())
	manager.mu.RLock()
	epoch := manager.auths["a"].RegistrationEpoch
	manager.mu.RUnlock()
	ctx := context.Background()
	if force {
		ctx = withForceRefresh(ctx)
	}
	if _, err := manager.refreshAuthForRequestAtEpoch(ctx, "a", "", epoch); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return exec
}

func TestLeasedRefreshHoldsTheLeaseForTheProviderCall(t *testing.T) {
	store := &leasingStore{}
	exec := refreshWithLease(t, store, false)
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshed %d times, want 1", got)
	}
	if store.acquired.Load() != 1 || store.released.Load() != 1 {
		t.Fatalf("acquired %d, released %d; want 1 and 1", store.acquired.Load(), store.released.Load())
	}
	if got := time.Duration(store.ttl.Load()); got != refreshLeaseTTL {
		t.Fatalf("lease ttl = %v, want %v", got, refreshLeaseTTL)
	}
}

func TestLeasedRefreshLeavesTheWorkToTheHolder(t *testing.T) {
	store := &leasingStore{acquireErr: ErrRefreshLeaseHeld}
	exec := refreshWithLease(t, store, false)
	if got := exec.refreshCalls.Load(); got != 0 {
		t.Fatalf("refreshed %d times while another replica held the lease", got)
	}
}

func TestLeasedRefreshProceedsWhenARequestNeedsAFreshToken(t *testing.T) {
	store := &leasingStore{acquireErr: ErrRefreshLeaseHeld}
	exec := refreshWithLease(t, store, true)
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshed %d times, want 1: the grant is reusable and the request cannot wait", got)
	}
}

func TestLeasedRefreshProceedsWhenTheLeaseCannotBeTaken(t *testing.T) {
	store := &leasingStore{acquireErr: errors.New("store unavailable")}
	exec := refreshWithLease(t, store, false)
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshed %d times, want 1", got)
	}
}

func TestLeasePolicyTakesNoJournalClaim(t *testing.T) {
	journal, err := refreshJournalFor(sharedJournalStore{RefreshJournal: codex.NewMemoryRefreshJournal()}, &Auth{Provider: "antigravity"}, &leasedGrantExecutor{})
	if err != nil || journal != nil {
		t.Fatalf("journal = %v, err = %v; a lease replaces the journal claim", journal, err)
	}
}
