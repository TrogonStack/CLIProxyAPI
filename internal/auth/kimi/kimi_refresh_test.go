package kimi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"golang.org/x/sync/singleflight"
)

type kimiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kimiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func resetKimiRefreshGroupForTest() {
	kimiRefreshGroup = singleflight.Group{}
}

func TestRefreshToken_DeduplicatesConcurrentRefreshAcrossInstances(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"token_type":"Bearer",
				"expires_in":3600
			}`)),
			Header:  make(http.Header),
			Request: req,
		}, nil
	})
	clientA := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}
	clientB := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	results := make(chan *KimiTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func(client *DeviceFlowClient, launched chan<- struct{}) {
		if launched != nil {
			close(launched)
		}
		tokenData, errRefresh := client.RefreshToken(context.Background(), "shared-refresh-token")
		results <- tokenData
		errs <- errRefresh
	}

	go runRefresh(clientA, nil)
	<-started

	secondLaunched := make(chan struct{})
	go runRefresh(clientB, secondLaunched)
	<-secondLaunched
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if errRefresh := <-errs; errRefresh != nil {
			t.Fatalf("expected refresh to succeed, got %v", errRefresh)
		}
		tokenData := <-results
		if tokenData == nil || tokenData.AccessToken != "new-access" || tokenData.RefreshToken != "new-refresh" {
			t.Fatalf("unexpected token data: %#v", tokenData)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected both refresh callers to share a single upstream call, got %d", got)
	}
}

func TestRefreshToken_JournalSendsOnceAndNeverReplaysALostResponse(t *testing.T) {
	resetKimiRefreshGroupForTest()
	t.Cleanup(resetKimiRefreshGroupForTest)

	var calls atomic.Int32
	var lose atomic.Bool
	transport := kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if lose.Load() {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header), Request: req}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600,"scope":"kimi"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	ctx := codex.WithRefreshJournal(context.Background(), codex.NewMemoryRefreshJournal())
	client := &DeviceFlowClient{httpClient: &http.Client{Transport: transport}}

	first, err := client.RefreshToken(ctx, "old-refresh")
	if err != nil || first.AccessToken != "new-access" || first.RefreshToken != "new-refresh" {
		t.Fatalf("first refresh = %+v, %v", first, err)
	}
	// The receipt is rebuilt for a replica that lost the claim, expiry and scope included.
	late, err := (&DeviceFlowClient{httpClient: &http.Client{Transport: transport}}).RefreshToken(ctx, "old-refresh")
	if err != nil || late.AccessToken != "new-access" || late.RefreshToken != "new-refresh" || late.ExpiresAt != first.ExpiresAt || late.Scope != "kimi" {
		t.Fatalf("late refresh = %+v, %v", late, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("requests = %d, want 1", calls.Load())
	}

	lose.Store(true)
	before := calls.Load()
	for range 2 {
		if _, err := client.RefreshToken(ctx, "lost-refresh"); !codex.IsRefreshOutcomeUnknown(err) {
			t.Fatalf("lost grant = %v, want refresh_outcome_unknown", err)
		}
	}
	if got := calls.Load() - before; got != 1 {
		t.Fatalf("requests for the lost grant = %d, want 1", got)
	}
}
