package natsstore

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func openRefreshJournals(t *testing.T, replicas int) (StoreSettings, *Bootstrapped, []*RefreshJournal) {
	t.Helper()
	srv := natstest.Start(t)
	settings := autoSettings("journal")
	var journals []*RefreshJournal
	var boot *Bootstrapped
	for range replicas {
		nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		boot, err = Bootstrap(testContext(t), nc, settings)
		if err != nil {
			t.Fatal(err)
		}
		journals = append(journals, NewRefreshJournal(NewKV(settings.BucketName(BucketRefresh), boot.Bucket(settings, BucketRefresh))))
	}
	return settings, boot, journals
}

func grantKey(seed string) string { return strings.Repeat(seed, 64)[:64] }

func TestRefreshJournalClaimReceiptChain(t *testing.T) {
	_, _, journals := openRefreshJournals(t, 1)
	journal := journals[0]
	ctx := testContext(t)
	first, second := grantKey("a"), grantKey("b")

	if receipt, err := journal.RefreshRead(ctx, first); err != nil || receipt != nil {
		t.Fatalf("read of an unclaimed grant = %q, %v; want nil", receipt, err)
	}
	if err := journal.RefreshComplete(ctx, first, []byte(`{"value":{}}`)); err == nil {
		t.Fatal("completing an unclaimed grant succeeded")
	}
	if claimed, err := journal.RefreshClaim(ctx, first); err != nil || !claimed {
		t.Fatalf("first claim = %v, %v", claimed, err)
	}
	if receipt, err := journal.RefreshRead(ctx, first); err != nil || string(receipt) != codex.PendingRefreshReceipt {
		t.Fatalf("read of a pending claim = %q, %v", receipt, err)
	}
	// An incomplete claim is never reclaimed, however often it is retried.
	for range 3 {
		if claimed, err := journal.RefreshClaim(ctx, first); err != nil || claimed {
			t.Fatalf("reclaim of a pending claim = %v, %v; want false", claimed, err)
		}
	}

	rotated := []byte(`{"value":{"access_token":"x","refresh_token":"next"}}`)
	if err := journal.RefreshComplete(ctx, first, rotated); err != nil {
		t.Fatal(err)
	}
	if err := journal.RefreshComplete(ctx, first, []byte(`{"rejected":"invalid_grant"}`)); err == nil {
		t.Fatal("a receipt was overwritten")
	}
	if receipt, err := journal.RefreshRead(ctx, first); err != nil || string(receipt) != string(rotated) {
		t.Fatalf("read of a completed claim = %q, %v", receipt, err)
	}
	if claimed, err := journal.RefreshClaim(ctx, first); err != nil || claimed {
		t.Fatalf("reclaim of a completed claim = %v, %v; want false", claimed, err)
	}

	// The next grant in the chain is an independent claim.
	if claimed, err := journal.RefreshClaim(ctx, second); err != nil || !claimed {
		t.Fatalf("claim of the rotated grant = %v, %v", claimed, err)
	}
}

func TestRefreshJournalRejectsMalformedKeys(t *testing.T) {
	_, _, journals := openRefreshJournals(t, 1)
	ctx := testContext(t)
	for _, key := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 65)} {
		if _, err := journals[0].RefreshClaim(ctx, key); err == nil {
			t.Errorf("claim accepted key %q", key)
		}
		if _, err := journals[0].RefreshRead(ctx, key); err == nil {
			t.Errorf("read accepted key %q", key)
		}
		if err := journals[0].RefreshComplete(ctx, key, []byte("{}")); err == nil {
			t.Errorf("complete accepted key %q", key)
		}
	}
}

func TestRefreshJournalConcurrentClaimersHaveOneWinner(t *testing.T) {
	_, _, journals := openRefreshJournals(t, 4)
	ctx := testContext(t)
	key := grantKey("c")

	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, journal := range journals {
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				claimed, err := journal.RefreshClaim(ctx, key)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if claimed {
					winners.Add(1)
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("claim winners = %d, want 1", winners.Load())
	}
	// Every replica sees the same pending claim.
	for _, journal := range journals {
		if receipt, err := journal.RefreshRead(ctx, key); err != nil || string(receipt) != codex.PendingRefreshReceipt {
			t.Fatalf("read = %q, %v", receipt, err)
		}
	}
}

func TestRefreshJournalBucketIsDurableWithoutExpiry(t *testing.T) {
	settings, boot, _ := openRefreshJournals(t, 1)
	status, err := boot.Bucket(settings, BucketRefresh).Status(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := status.(*jetstream.KeyValueBucketStatus).StreamInfo().Config
	if cfg.Storage != jetstream.FileStorage || cfg.MaxAge != 0 || cfg.SubjectDeleteMarkerTTL != 0 || cfg.Replicas != settings.Replicas {
		t.Fatalf("journal bucket = storage %v, max age %v, marker ttl %v, replicas %d", cfg.Storage, cfg.MaxAge, cfg.SubjectDeleteMarkerTTL, cfg.Replicas)
	}
}

func TestRefreshJournalDeletedClaimIsNotReclaimed(t *testing.T) {
	settings, boot, journals := openRefreshJournals(t, 1)
	ctx := testContext(t)
	key := grantKey("d")
	if claimed, err := journals[0].RefreshClaim(ctx, key); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	// Anything allowed to publish on the bucket can write a delete marker;
	// the journal never does.
	encoded, err := EncodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := boot.Bucket(settings, BucketRefresh).Delete(ctx, encoded); err != nil {
		t.Fatal(err)
	}
	if claimed, err := journals[0].RefreshClaim(ctx, key); err != nil || claimed {
		t.Fatalf("reclaim over a delete marker = %v, %v; want false", claimed, err)
	}
	if err := journals[0].RefreshComplete(ctx, key, []byte(`{"value":{}}`)); err == nil {
		t.Fatal("a receipt was written over a delete marker")
	}
}
