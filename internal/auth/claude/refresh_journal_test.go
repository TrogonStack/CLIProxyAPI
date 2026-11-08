package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// claimSpy records whether the grant was claimed before any request was sent.
type claimSpy struct {
	codex.RefreshJournal
	claimed atomic.Bool
}

func (s *claimSpy) RefreshClaim(ctx context.Context, key string) (bool, error) {
	claimed, err := s.RefreshJournal.RefreshClaim(ctx, key)
	if claimed {
		s.claimed.Store(true)
	}
	return claimed, err
}

func TestRefreshTokensJournalClaimsBeforeSendingAndServesLateCallers(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	spy := &claimSpy{RefreshJournal: codex.NewMemoryRefreshJournal()}
	var calls atomic.Int32
	var claimedBeforeSend atomic.Bool
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		claimedBeforeSend.Store(spy.claimed.Load())
		if strings.Contains(req.URL.Host, "api.anthropic.com") {
			// The profile lookup after the rotation is not part of the grant.
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)),
			Header:     make(http.Header),
		}, nil
	})
	ctx := codex.WithRefreshJournal(context.Background(), spy)
	first := &ClaudeAuth{httpClient: &http.Client{Transport: transport}}
	got, err := first.RefreshTokens(ctx, "rotating-grant")
	if err != nil || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("first refresh = %+v, %v", got, err)
	}
	if !claimedBeforeSend.Load() {
		t.Fatal("the grant was sent before it was claimed")
	}
	sent := calls.Load()

	// Another replica sharing the journal adopts the saved receipt.
	second := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("a late caller sent the grant again")
		return nil, errors.New("unexpected request")
	})}}
	late, err := second.RefreshTokens(ctx, "rotating-grant")
	if err != nil || late.AccessToken != "new-access" || late.RefreshToken != "new-refresh" || late.Expire != got.Expire {
		t.Fatalf("late refresh = %+v, %v", late, err)
	}
	if calls.Load() != sent {
		t.Fatalf("requests after the receipt = %d, want %d", calls.Load(), sent)
	}
}

func TestRefreshTokensJournalNeverReplaysALostResponse(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls atomic.Int32
	lost := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: &errorReader{err: io.ErrUnexpectedEOF}, Header: make(http.Header)}, nil
	})}}
	ctx := codex.WithRefreshJournal(context.Background(), codex.NewMemoryRefreshJournal())
	if _, err := lost.RefreshTokensWithRetry(ctx, "lost-grant", 3); !codex.IsRefreshOutcomeUnknown(err) {
		t.Fatalf("lost response = %v, want refresh_outcome_unknown", err)
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		other := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		})}}
		if _, err := other.RefreshTokensWithRetry(ctx, "lost-grant", 3); !codex.IsRefreshOutcomeUnknown(err) {
			t.Fatalf("retry after a lost response = %v, want refresh_outcome_unknown", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("requests = %d, want exactly the first", calls.Load())
	}
}

func TestRefreshTokensJournalRecordsOnlyQualifiedRejections(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	journal := codex.NewMemoryRefreshJournal()
	ctx := codex.WithRefreshJournal(context.Background(), journal)
	status := http.StatusBadRequest
	var calls atomic.Int32
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Header: make(http.Header)}, nil
	})}}
	_, err := auth.RefreshTokens(ctx, "rejected-grant")
	if err == nil || codex.IsRefreshOutcomeUnknown(err) {
		t.Fatalf("rejected grant = %v, want a plain rejection", err)
	}
	// The rejection is final for every replica: nothing is sent again.
	_, err = auth.RefreshTokens(ctx, "rejected-grant")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || calls.Load() != 1 {
		t.Fatalf("second attempt = %v after %d requests, want the recorded rejection", err, calls.Load())
	}
}
