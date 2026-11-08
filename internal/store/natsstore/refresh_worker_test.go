package natsstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestStoreRefreshRequestReachesExactlyOneReplica(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var handled atomic.Int32
	got := make(chan cliproxyauth.RefreshRequest, 4)
	handle := func(_ context.Context, req cliproxyauth.RefreshRequest) error {
		handled.Add(1)
		got <- req
		return nil
	}
	go func() { _ = a.ServeRefreshRequests(ctx, handle) }()
	go func() { _ = b.ServeRefreshRequests(ctx, handle) }()

	want := cliproxyauth.RefreshRequest{AuthID: "example/one.json", Provider: "example", CredentialVersion: 7}
	if err := a.RequestRefresh(ctx, want); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-got:
		if req != want {
			t.Fatalf("request = %+v, want %+v", req, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request was not handled")
	}
	time.Sleep(time.Second)
	if n := handled.Load(); n != 1 {
		t.Fatalf("handled %d times, want once", n)
	}
}

func TestStoreRefreshRequestIsRedeliveredAfterAHandlerError(t *testing.T) {
	f := newStoreFixture(t)
	s := f.open()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	go func() {
		_ = s.ServeRefreshRequests(ctx, func(context.Context, cliproxyauth.RefreshRequest) error {
			if calls.Add(1) == 1 {
				return errors.New("provider unavailable")
			}
			return nil
		})
	}()
	if err := s.RequestRefresh(ctx, cliproxyauth.RefreshRequest{AuthID: "example/two.json", Provider: "example", CredentialVersion: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second delivery", func() bool { return calls.Load() >= 2 })
}

func TestStoreRefreshRequestRepeatIsAccepted(t *testing.T) {
	f := newStoreFixture(t)
	s := f.open()
	req := cliproxyauth.RefreshRequest{AuthID: "example/three.json", Provider: "example", CredentialVersion: 1}
	for range 2 {
		if err := s.RequestRefresh(context.Background(), req); err != nil {
			t.Fatalf("repeat request: %v", err)
		}
	}
}
