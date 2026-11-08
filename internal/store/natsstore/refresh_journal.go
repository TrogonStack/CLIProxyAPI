package natsstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// RefreshJournal keeps refresh grant claims and receipts in the refresh bucket.
// A claim is an atomic Create, so exactly one replica wins a grant, and the
// receipt replaces the pending marker with a revision compare-and-swap. Nothing
// here deletes, purges or expires an entry: an incomplete claim is never
// reclaimed, so a crash between claim and receipt reads back as pending and the
// grant is not replayed, exactly as with the file journal.
type RefreshJournal struct {
	kv *KV
}

var _ codex.RefreshJournal = (*RefreshJournal)(nil)

// NewRefreshJournal wraps the refresh bucket. The bucket must have no TTL, as
// BucketSpec describes; The startup probe checks that.
func NewRefreshJournal(kv *KV) *RefreshJournal { return &RefreshJournal{kv: kv} }

// validRefreshKey accepts only the sha256 hex digest codex.refreshGrantKey
// produces, so a malformed key cannot name another entry.
func validRefreshKey(key string) error {
	if len(key) != 64 {
		return fmt.Errorf("invalid refresh journal key")
	}
	for _, char := range key {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return fmt.Errorf("invalid refresh journal key")
		}
	}
	return nil
}

// RefreshClaim records the grant as consumed. It reports false when another
// claim, completed or not, already exists. A delete marker counts as a claim,
// so a key someone deleted cannot be claimed again; the grant then reads back
// as an unresolved outcome rather than as unclaimed.
func (j *RefreshJournal) RefreshClaim(ctx context.Context, key string) (bool, error) {
	if err := validRefreshKey(key); err != nil {
		return false, err
	}
	if _, err := j.kv.CreateOnce(ctx, key, []byte(codex.PendingRefreshReceipt)); err != nil {
		if errors.Is(err, ErrConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RefreshRead returns the receipt, the pending marker for a claim without one,
// or nil when the grant was never claimed.
func (j *RefreshJournal) RefreshRead(ctx context.Context, key string) ([]byte, error) {
	if err := validRefreshKey(key); err != nil {
		return nil, err
	}
	entry, err := j.kv.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return entry.Value, nil
}

// RefreshComplete saves the receipt over the pending claim. It fails when the
// claim is missing or already completed, and never overwrites a receipt.
func (j *RefreshJournal) RefreshComplete(ctx context.Context, key string, receipt []byte) error {
	if err := validRefreshKey(key); err != nil {
		return err
	}
	entry, err := j.kv.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("refresh receipt claim missing")
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(entry.Value, []byte(codex.PendingRefreshReceipt)) {
		return fmt.Errorf("refresh receipt already completed")
	}
	if _, err := j.kv.Update(ctx, key, receipt, entry.Revision); err != nil {
		if errors.Is(err, ErrConflict) {
			return fmt.Errorf("refresh receipt already completed")
		}
		return err
	}
	return nil
}
