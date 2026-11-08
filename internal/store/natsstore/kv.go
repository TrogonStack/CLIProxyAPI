package natsstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

var (
	// ErrNotFound is returned when a key has no live value.
	ErrNotFound = errors.New("key not found")
	// ErrConflict is matched by a ConflictError.
	ErrConflict = errors.New("revision conflict")
)

// ConflictError reports that a write lost a compare-and-swap, because the key
// was created, changed or deleted since the revision the caller read.
type ConflictError struct {
	Bucket string
	ID     string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("revision conflict on %s in bucket %s", e.ID, e.Bucket)
}

// Is makes errors.Is(err, ErrConflict) match.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Entry is one live value with the revision a later write must present.
type Entry struct {
	ID       string
	Value    []byte
	Revision uint64
	// Modified is when the revision was written.
	Modified time.Time
}

// KV wraps a bucket so callers work with identifiers and revisions and never
// with encoded keys. Every write is a compare-and-swap; there is no blind put.
type KV struct {
	bucket string
	kv     jetstream.KeyValue
}

// NewKV wraps the bucket handle.
func NewKV(bucket string, kv jetstream.KeyValue) *KV { return &KV{bucket: bucket, kv: kv} }

// Get returns the live value for the identifier, or ErrNotFound.
func (k *KV) Get(ctx context.Context, id string) (Entry, error) {
	key, err := EncodeKey(id)
	if err != nil {
		return Entry{}, err
	}
	entry, err := k.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("failed to read %s from %s: %w", id, k.bucket, err)
	}
	return entryOf(id, entry), nil
}

// Create writes a value only if the key has no live value.
func (k *KV) Create(ctx context.Context, id string, value []byte) (uint64, error) {
	key, err := EncodeKey(id)
	if err != nil {
		return 0, err
	}
	rev, err := k.kv.Create(ctx, key, value)
	if err != nil {
		return 0, k.writeError("create", id, err)
	}
	return rev, nil
}

// Update writes a value only if the key is still at the expected revision.
func (k *KV) Update(ctx context.Context, id string, value []byte, expected uint64) (uint64, error) {
	key, err := EncodeKey(id)
	if err != nil {
		return 0, err
	}
	rev, err := k.kv.Update(ctx, key, value, expected)
	if err != nil {
		return 0, k.writeError("update", id, err)
	}
	return rev, nil
}

// Delete removes the key only if it is still at the expected revision.
func (k *KV) Delete(ctx context.Context, id string, expected uint64) error {
	key, err := EncodeKey(id)
	if err != nil {
		return err
	}
	if err := k.kv.Delete(ctx, key, jetstream.LastRevision(expected)); err != nil {
		return k.writeError("delete", id, err)
	}
	return nil
}

// List returns every live entry in key order. Keys deleted while listing are skipped.
func (k *KV) List(ctx context.Context) ([]Entry, error) {
	keys, err := k.kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list keys of %s: %w", k.bucket, err)
	}
	defer func() { _ = keys.Stop() }()

	var entries []Entry
	for key := range keys.Keys() {
		id, err := DecodeKey(key)
		if err != nil {
			// Not written by this backend; leave it alone.
			continue
		}
		entry, err := k.kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("failed to read %s from %s: %w", id, k.bucket, err)
		}
		entries = append(entries, entryOf(id, entry))
	}
	return entries, nil
}

func (k *KV) writeError(op, id string, err error) error {
	if errors.Is(err, jetstream.ErrKeyExists) {
		return &ConflictError{Bucket: k.bucket, ID: id}
	}
	return fmt.Errorf("failed to %s %s in %s: %w", op, id, k.bucket, err)
}

func entryOf(id string, entry jetstream.KeyValueEntry) Entry {
	return Entry{ID: id, Value: entry.Value(), Revision: entry.Revision(), Modified: entry.Created()}
}
