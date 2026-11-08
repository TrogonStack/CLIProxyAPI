package natsstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

// Is makes errors.Is match ErrConflict and the SDK-level auth.ErrStoreConflict.
func (e *ConflictError) Is(target error) bool {
	return target == ErrConflict || target == coreauth.ErrStoreConflict
}

// Entry is one live value with the revision a later write must present.
type Entry struct {
	ID       string
	Value    []byte
	Revision uint64
	// Modified is when the revision was written.
	Modified time.Time
	// Origin is the replica that wrote the revision, empty for a value written
	// before envelopes existed.
	Origin string
	// Base is the revision the write replaced, zero for a create or a legacy value.
	Base uint64
}

// KV wraps a bucket so callers work with identifiers and revisions and never
// with encoded keys. Every write is a compare-and-swap; there is no blind put.
type KV struct {
	bucket string
	kv     jetstream.KeyValue
	origin string
	// onWatch observes each watch Follow opens, for tests that drop it.
	onWatch func(jetstream.KeyWatcher)
}

// KVOption customizes a KV.
type KVOption func(*KV)

// WithOrigin makes the KV wrap every value it writes with the replica that
// wrote it. Without it values are stored raw, as the earlier backend did.
// Reads accept both formats either way.
func WithOrigin(origin string) KVOption { return func(k *KV) { k.origin = origin } }

// NewKV wraps the bucket handle.
func NewKV(bucket string, kv jetstream.KeyValue, opts ...KVOption) *KV {
	k := &KV{bucket: bucket, kv: kv}
	for _, opt := range opts {
		opt(k)
	}
	return k
}

// Origin returns the replica identifier written with values, empty when values are stored raw.
func (k *KV) Origin() string { return k.origin }

// wrap prepares a value for storage.
func (k *KV) wrap(value []byte, base uint64) ([]byte, error) {
	if k.origin == "" {
		return value, nil
	}
	return encodeEnvelope(k.origin, base, value)
}

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
	stored, err := k.wrap(value, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to encode %s for %s: %w", id, k.bucket, err)
	}
	rev, err := k.kv.Create(ctx, key, stored)
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
	stored, err := k.wrap(value, expected)
	if err != nil {
		return 0, fmt.Errorf("failed to encode %s for %s: %w", id, k.bucket, err)
	}
	rev, err := k.kv.Update(ctx, key, stored, expected)
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
	value, origin, base := decodeEnvelope(entry.Value())
	return Entry{ID: id, Value: value, Revision: entry.Revision(), Modified: entry.Created(), Origin: origin, Base: base}
}
