package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const queueRefreshProvider = "queue-refresh"

// queueHostStore is a store with a refresh queue: it records the requests the
// manager makes and lets a test play the replica that serves them.
type queueHostStore struct {
	mu       sync.Mutex
	requests []RefreshRequest
	failWith error
	serving  chan func(context.Context, RefreshRequest) error
}

func newQueueHostStore() *queueHostStore {
	return &queueHostStore{serving: make(chan func(context.Context, RefreshRequest) error, 4)}
}

func (s *queueHostStore) List(context.Context) ([]*Auth, error)       { return nil, nil }
func (s *queueHostStore) Save(context.Context, *Auth) (string, error) { return "", nil }
func (s *queueHostStore) Delete(context.Context, string) error        { return nil }

func (s *queueHostStore) RequestRefresh(_ context.Context, req RefreshRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	s.requests = append(s.requests, req)
	return nil
}

func (s *queueHostStore) ServeRefreshRequests(ctx context.Context, handle func(context.Context, RefreshRequest) error) error {
	s.serving <- handle
	<-ctx.Done()
	return nil
}

func (s *queueHostStore) requested() []RefreshRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RefreshRequest(nil), s.requests...)
}

// reusableGrantExecutor declares its grant stays valid after use.
type reusableGrantExecutor struct{ countingRefreshExecutor }

func (*reusableGrantExecutor) RefreshPolicy() RefreshPolicy { return RefreshNone }

func registerQueueAuth(t *testing.T, manager *Manager, id, provider string, now time.Time) {
	t.Helper()
	auth := &Auth{
		ID:              id,
		Provider:        provider,
		Status:          StatusActive,
		LastRefreshedAt: now.Add(-time.Hour),
		Metadata: map[string]any{
			"access_token":             "expired-access",
			"refresh_token":            "refresh-" + id,
			"expires_at":               now.Add(-time.Hour).Format(time.RFC3339),
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
}

// dueQueueJob runs one scheduler job for the credential the way a worker does.
func dueQueueJob(t *testing.T, manager *Manager, loop *authAutoRefreshLoop, id string, now time.Time) *authRefreshJob {
	t.Helper()
	manager.mu.RLock()
	epoch := manager.auths[id].RegistrationEpoch
	manager.mu.RUnlock()
	job := manager.markRefreshPending(loop, id, epoch, now)
	if job == nil {
		t.Fatal("credential is not due")
	}
	return job
}

func newQueueManager(t *testing.T, store Store, exec ProviderExecutor) (*Manager, *authAutoRefreshLoop) {
	t.Helper()
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	loop := newAuthAutoRefreshLoop(manager, refreshCheckInterval, 1)
	manager.refreshLoop = loop
	return manager, loop
}

func TestSchedulerQueuesARefreshInsteadOfRefreshingInline(t *testing.T) {
	store := newQueueHostStore()
	exec := &countingRefreshExecutor{id: queueRefreshProvider}
	manager, loop := newQueueManager(t, store, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)
	manager.mu.Lock()
	manager.refreshDispatcher = store
	manager.mu.Unlock()

	loop.rebuild(now)
	loop.handleDue(context.Background(), now)
	select {
	case job := <-loop.jobs:
		if !manager.beginRefreshJob(context.Background(), job) {
			t.Fatal("job did not begin")
		}
		retryAt, settled := manager.dispatchRefresh(context.Background(), job)
		if !settled || !retryAt.Equal(job.pendingUntil) {
			t.Fatalf("dispatch = %v, %v; want settled until %v", retryAt, settled, job.pendingUntil)
		}
	default:
		t.Fatal("no refresh job was scheduled")
	}
	reqs := store.requested()
	if len(reqs) != 1 || reqs[0].AuthID != "a" || reqs[0].Provider != queueRefreshProvider || reqs[0].CredentialVersion == 0 {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := exec.refreshCalls.Load(); got != 0 {
		t.Fatalf("executor refreshed %d times inline, want 0", got)
	}
}

func TestWorkerQueuesWithoutRefreshingAndWaitsBeforeAsking(t *testing.T) {
	store := newQueueHostStore()
	exec := &countingRefreshExecutor{id: queueRefreshProvider}
	manager, loop := newQueueManager(t, store, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)
	manager.mu.Lock()
	manager.refreshDispatcher = store
	manager.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.worker(ctx)
	loop.rebuild(now)
	loop.handleDue(ctx, now)

	deadline := time.Now().Add(5 * time.Second)
	for len(store.requested()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(store.requested()) != 1 {
		t.Fatalf("requests = %+v", store.requested())
	}
	for {
		manager.mu.RLock()
		pending := manager.refreshJobs["a"]
		next := manager.auths["a"].NextRefreshAfter
		manager.mu.RUnlock()
		if pending == nil {
			if !next.After(now.Add(refreshPendingBackoff - time.Second)) {
				t.Fatalf("next refresh after = %v, want about a pending backoff from now", next)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := exec.refreshCalls.Load(); got != 0 {
		t.Fatalf("executor refreshed %d times inline, want 0", got)
	}
}

func TestFailedQueueingFailsClosedForSingleUseGrants(t *testing.T) {
	store := newQueueHostStore()
	store.failWith = errors.New("queue unreachable")
	exec := &countingRefreshExecutor{id: queueRefreshProvider}
	manager, loop := newQueueManager(t, store, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)
	manager.mu.Lock()
	manager.refreshDispatcher = store
	manager.mu.Unlock()

	job := dueQueueJob(t, manager, loop, "a", now)
	retryAt, settled := manager.dispatchRefresh(context.Background(), job)
	if !settled || !retryAt.IsZero() {
		t.Fatalf("dispatch = %v, %v; want settled and due again at once", retryAt, settled)
	}
}

func TestFailedQueueingRefreshesInlineForReusableGrants(t *testing.T) {
	store := newQueueHostStore()
	store.failWith = errors.New("queue unreachable")
	exec := &reusableGrantExecutor{countingRefreshExecutor{id: queueRefreshProvider}}
	manager, loop := newQueueManager(t, store, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)
	manager.mu.Lock()
	manager.refreshDispatcher = store
	manager.mu.Unlock()

	job := dueQueueJob(t, manager, loop, "a", now)
	if _, settled := manager.dispatchRefresh(context.Background(), job); settled {
		t.Fatal("a reusable grant should fall back to refreshing inline")
	}
}

func TestWithoutAQueueRefreshStaysInline(t *testing.T) {
	exec := &countingRefreshExecutor{id: queueRefreshProvider}
	manager, loop := newQueueManager(t, nil, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)

	job := dueQueueJob(t, manager, loop, "a", now)
	if _, settled := manager.dispatchRefresh(context.Background(), job); settled {
		t.Fatal("a manager without a queue dispatched a refresh")
	}
}

func TestStartAutoRefreshServesTheStoresQueue(t *testing.T) {
	store := newQueueHostStore()
	exec := &renewingQueueExecutor{countingRefreshExecutor{id: queueRefreshProvider}}
	manager, _ := newQueueManager(t, store, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.StartAutoRefresh(ctx, time.Hour)
	defer manager.StopAutoRefresh()

	var handle func(context.Context, RefreshRequest) error
	select {
	case handle = <-store.serving:
	case <-time.After(5 * time.Second):
		t.Fatal("the store's queue was not served")
	}
	if err := handle(ctx, RefreshRequest{AuthID: "a", Provider: queueRefreshProvider}); err != nil {
		t.Fatalf("handle = %v", err)
	}
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("a served request refreshed %d times, want 1", got)
	}
	if err := handle(ctx, RefreshRequest{AuthID: "a", Provider: queueRefreshProvider}); err != nil {
		t.Fatalf("second handle = %v", err)
	}
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("a request for a fresh credential refreshed again: %d calls", got)
	}
}

func TestServedRequestIgnoresTheRequestersPendingMarker(t *testing.T) {
	store := newQueueHostStore()
	exec := &countingRefreshExecutor{id: queueRefreshProvider}
	manager, loop := newQueueManager(t, store, exec)
	now := time.Now()
	registerQueueAuth(t, manager, "a", queueRefreshProvider, now)
	// The scheduler on this replica marked the credential pending when it queued the request.
	dueQueueJob(t, manager, loop, "a", now)

	if err := manager.handleRefreshRequest(context.Background(), RefreshRequest{AuthID: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := exec.refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshed %d times, want 1", got)
	}
}

func TestServedRequestForAMissingCredentialIsComplete(t *testing.T) {
	manager, _ := newQueueManager(t, nil, &countingRefreshExecutor{id: queueRefreshProvider})
	if err := manager.handleRefreshRequest(context.Background(), RefreshRequest{AuthID: "gone"}); err != nil {
		t.Fatalf("handle = %v, want nil", err)
	}
	if err := manager.handleRefreshRequest(context.Background(), RefreshRequest{}); err != nil {
		t.Fatalf("handle of an empty request = %v, want nil", err)
	}
}

func TestServedRequestAsksForRedeliveryOnlyWhenCanceled(t *testing.T) {
	exec := &blockingQueueExecutor{countingRefreshExecutor: countingRefreshExecutor{id: queueRefreshProvider}}
	manager, _ := newQueueManager(t, nil, exec)
	registerQueueAuth(t, manager, "a", queueRefreshProvider, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.handleRefreshRequest(ctx, RefreshRequest{AuthID: "a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled handle = %v, want context.Canceled", err)
	}

	exec.fail = errors.New("provider unavailable")
	if err := manager.handleRefreshRequest(context.Background(), RefreshRequest{AuthID: "a"}); err != nil {
		t.Fatalf("a failed refresh is recorded, not redelivered; got %v", err)
	}
}

// blockingQueueExecutor honors cancellation and can be made to fail.
type blockingQueueExecutor struct {
	countingRefreshExecutor
	fail error
}

func (e *blockingQueueExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.fail != nil {
		return nil, e.fail
	}
	return e.countingRefreshExecutor.Refresh(ctx, auth)
}

// renewingQueueExecutor returns a credential that is fresh for an hour.
type renewingQueueExecutor struct{ countingRefreshExecutor }

func (e *renewingQueueExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	updated, err := e.countingRefreshExecutor.Refresh(ctx, auth)
	if err != nil {
		return nil, err
	}
	updated.Metadata["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return updated, nil
}
