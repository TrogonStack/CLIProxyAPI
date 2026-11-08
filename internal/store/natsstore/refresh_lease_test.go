package natsstore

import (
	"context"
	"errors"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRefreshLeaseIsHeldByOneReplicaAtATime(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx := context.Background()

	release, err := a.AcquireRefreshLease(ctx, "antigravity/one.json", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AcquireRefreshLease(ctx, "antigravity/one.json", time.Minute); !errors.Is(err, cliproxyauth.ErrRefreshLeaseHeld) {
		t.Fatalf("second replica err = %v, want held", err)
	}
	if _, err := b.AcquireRefreshLease(ctx, "antigravity/other.json", time.Minute); err != nil {
		t.Fatalf("lease for another credential: %v", err)
	}

	release()
	if _, err := b.AcquireRefreshLease(ctx, "antigravity/one.json", time.Minute); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestRefreshLeaseRunsOutWhenTheHolderDies(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx := context.Background()

	if _, err := a.AcquireRefreshLease(ctx, "antigravity/two.json", time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "lease to run out", func() bool {
		release, err := b.AcquireRefreshLease(ctx, "antigravity/two.json", time.Minute)
		if err == nil {
			release()
		}
		return err == nil
	})
}

func TestStaleReleaseDoesNotDeleteANewHoldersLease(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx := context.Background()

	staleRelease, err := a.AcquireRefreshLease(ctx, "antigravity/three.json", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var fresh func()
	waitFor(t, "lease to run out", func() bool {
		fresh, err = b.AcquireRefreshLease(ctx, "antigravity/three.json", time.Minute)
		return err == nil
	})
	defer fresh()

	staleRelease()
	if _, err := a.AcquireRefreshLease(ctx, "antigravity/three.json", time.Minute); !errors.Is(err, cliproxyauth.ErrRefreshLeaseHeld) {
		t.Fatalf("err = %v, want the new holder's lease to survive a stale release", err)
	}
}

func TestRefreshJournalBucketNeverTakesAKeyTTL(t *testing.T) {
	f := newStoreFixture(t)
	s := f.open()
	if _, err := s.leases.CreateWithTTL(context.Background(), "probe", []byte("x"), time.Minute); err != nil {
		t.Fatalf("lease bucket must take key TTLs: %v", err)
	}
	if _, err := s.journal.kv.CreateWithTTL(context.Background(), "probe", []byte("x"), time.Minute); err == nil {
		t.Fatal("journal bucket accepted a key TTL; its claims must never expire")
	}
}
