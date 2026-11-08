package natsstore

import (
	"context"
	"errors"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// errNoRefreshQueue is returned when the store was built without a refresh queue.
var errNoRefreshQueue = errors.New("nats store: refresh queue is not available")

var _ cliproxyauth.RefreshQueueHost = (*Store)(nil)

// RequestRefresh asks the replicas sharing the store to refresh a credential.
// A request that is already queued, or was made for the same credential
// version a moment ago, counts as made.
func (s *Store) RequestRefresh(ctx context.Context, req cliproxyauth.RefreshRequest) error {
	if s.refresh == nil {
		return errNoRefreshQueue
	}
	_, err := s.refresh.Publish(ctx, RefreshRequest{
		CredentialID: req.AuthID,
		Provider:     req.Provider,
		Version:      req.CredentialVersion,
		RequestedBy:  s.replicaID,
	})
	return err
}

// ServeRefreshRequests hands each request this replica wins to handle and
// returns when ctx ends. A request is complete once handle returns nil, and is
// delivered again, to this or another replica, when handle returns an error.
// The result of a refresh reaches the other replicas through the credential
// bucket they already follow, so nothing else is published.
func (s *Store) ServeRefreshRequests(ctx context.Context, handle func(context.Context, cliproxyauth.RefreshRequest) error) error {
	if s.refresh == nil {
		return errNoRefreshQueue
	}
	return s.refresh.Consume(ctx, func(ctx context.Context, req RefreshRequest) error {
		err := handle(ctx, cliproxyauth.RefreshRequest{AuthID: req.CredentialID, Provider: req.Provider, CredentialVersion: req.Version})
		if err == nil || ctx.Err() != nil {
			return err
		}
		return RetryRefresh(err, 0)
	})
}
