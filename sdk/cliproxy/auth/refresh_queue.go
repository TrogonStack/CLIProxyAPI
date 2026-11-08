package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// refreshDispatchTimeout bounds handing a request to the queue, so an
// unreachable queue costs one scheduler worker seconds, not its whole run.
const refreshDispatchTimeout = 10 * time.Second

// RefreshRequest asks whichever replica takes it to refresh one credential. It
// names the credential and carries no secret: the replica that takes it reads
// the credential itself.
type RefreshRequest struct {
	AuthID   string
	Provider string
	// CredentialVersion is the version of the credential the requester saw
	// expiring. It identifies a request, so asking again for the same version
	// is not queued twice.
	CredentialVersion uint64
}

// RefreshDispatcher hands a refresh to the replicas that share it, so exactly
// one of them performs it.
type RefreshDispatcher interface {
	// RequestRefresh queues the request. It returns nil when the request is
	// queued or an equivalent one already is.
	RequestRefresh(ctx context.Context, req RefreshRequest) error
}

// RefreshQueueHost is implemented by a store whose replicas share a refresh
// queue. The manager then asks the queue for the refreshes its scheduler finds
// due, and serves the requests it is given, instead of refreshing inline.
type RefreshQueueHost interface {
	RefreshDispatcher
	// ServeRefreshRequests calls handle for each request this replica wins,
	// until ctx ends. A request is complete when handle returns nil; handle
	// returns an error only when the request should be delivered again.
	ServeRefreshRequests(ctx context.Context, handle func(context.Context, RefreshRequest) error) error
}

// startRefreshQueue attaches the store's refresh queue to the scheduler, when
// it has one, and serves its requests until ctx ends.
func (m *Manager) startRefreshQueue(ctx context.Context) {
	m.mu.Lock()
	m.refreshDispatcher = nil
	store := m.store
	m.mu.Unlock()
	host, ok := store.(RefreshQueueHost)
	if !ok || host == nil || m.HomeEnabled() {
		return
	}
	m.mu.Lock()
	m.refreshDispatcher = host
	m.mu.Unlock()
	go func() {
		if err := host.ServeRefreshRequests(ctx, m.handleRefreshRequest); err != nil && ctx.Err() == nil {
			log.WithError(err).Error("refresh queue consumer stopped; this replica no longer performs queued refreshes")
		}
	}()
}

// dispatchRefresh asks the queue to refresh the credential of a due job. It
// reports whether the job is settled, and when the scheduler may consider the
// credential again. It reports false when the caller must refresh inline,
// which is only ever the case for a grant that stays valid after use: a
// single-use grant waits for the queue instead, since the queue is what keeps
// two replicas from spending it together.
func (m *Manager) dispatchRefresh(ctx context.Context, job *authRefreshJob) (retryAt time.Time, settled bool) {
	m.mu.RLock()
	dispatcher := m.refreshDispatcher
	var (
		req  RefreshRequest
		exec ProviderExecutor
	)
	if auth := m.auths[job.id]; auth != nil {
		req = RefreshRequest{AuthID: auth.ID, Provider: auth.Provider, CredentialVersion: auth.CredentialVersion}
		exec, _ = m.executorLocked(executorKeyFromAuth(auth))
	}
	m.mu.RUnlock()
	if dispatcher == nil || req.AuthID == "" {
		return time.Time{}, false
	}

	ctx, cancel := context.WithTimeout(ctx, refreshDispatchTimeout)
	defer cancel()
	err := dispatcher.RequestRefresh(ctx, req)
	if err == nil {
		// Whoever takes the request refreshes it and the store tells this
		// replica; asking again before then only repeats the request.
		return job.pendingUntil, true
	}
	log.WithFields(log.Fields{"auth_id": req.AuthID, "provider": req.Provider}).Warnf("queue refresh request failed: %v", err)
	if exec != nil && RefreshPolicyOf(exec) != RefreshSingleUse {
		return time.Time{}, false
	}
	return time.Time{}, true
}

// handleRefreshRequest refreshes the credential of a request this replica won.
// It looks at the credential again first: another replica may have refreshed it
// since the request was made. The refresh itself is the one every refresh uses,
// so a single-use grant is claimed in the journal before it is sent, and the
// result is written back with the store's revision check. A refresh that
// fails is recorded on the credential and is not delivered again; the
// scheduler asks again when the credential is next due.
func (m *Manager) handleRefreshRequest(ctx context.Context, req RefreshRequest) error {
	id := strings.TrimSpace(req.AuthID)
	if id == "" {
		return nil
	}
	m.mu.RLock()
	auth := m.auths[id].Clone()
	m.mu.RUnlock()
	if auth == nil {
		return nil
	}
	// The requester's own pending marker must not make it skip its request.
	probe := auth.Clone()
	probe.NextRefreshAfter = time.Time{}
	if !m.shouldRefresh(probe, time.Now()) {
		return nil
	}
	if _, err := m.refreshAuthForRequestAtEpoch(ctx, id, "", auth.RegistrationEpoch); err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return err
		}
		log.WithFields(log.Fields{"auth_id": id, "provider": auth.Provider}).Debugf("queued refresh failed: %v", err)
	}
	return nil
}
