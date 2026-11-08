package natsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	// cooldownGrace keeps a record a little past the retry time it describes,
	// so a replica with a slow clock still sees the cooldown end.
	cooldownGrace = time.Minute
	// cooldownIdleTTL bounds a record that names no retry time.
	cooldownIdleTTL = time.Hour
	// cooldownTombstoneTTL is how long a cleared record keeps refusing older
	// writes from a replica that has not seen it cleared yet.
	cooldownTombstoneTTL = 10 * time.Minute
	// maxCooldownWriteAttempts bounds the retries of one record when replicas race.
	maxCooldownWriteAttempts = 4
)

var (
	_ cliproxyauth.CooldownStateStore         = (*CooldownStore)(nil)
	_ cliproxyauth.CooldownStateStoreProvider = (*Store)(nil)
)

// CooldownStore keeps runtime cooldown state in a bucket with per-key TTL, so a
// cooldown disappears on its own once its retry time has passed. It merges the
// way the PostgreSQL store does: a write never replaces a record that carries a
// newer UpdatedAt, and a cleared record leaves a tombstone that refuses older writes.
type CooldownStore struct {
	kv       *KV
	mu       sync.Mutex
	previous map[string]time.Time
}

// cooldownEntry is the stored form of one auth/model pair.
type cooldownEntry struct {
	Deleted   bool                              `json:"deleted,omitempty"`
	UpdatedAt time.Time                         `json:"updated_at"`
	Record    *cliproxyauth.CooldownStateRecord `json:"record,omitempty"`
}

// NewCooldownStore wraps the cooldown bucket.
func NewCooldownStore(kv *KV) *CooldownStore {
	return &CooldownStore{kv: kv, previous: make(map[string]time.Time)}
}

// CooldownStateStore returns the cooldown store the manager persists through.
func (s *Store) CooldownStateStore() cliproxyauth.CooldownStateStore {
	if s == nil || s.cooldown == nil {
		return nil
	}
	return s.cooldown
}

func cooldownID(record cliproxyauth.CooldownStateRecord) string {
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return ""
	}
	return authID + "\x1f" + strings.TrimSpace(record.Model)
}

func normalizeCooldownTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		value = fallback
	}
	return value.UTC().Truncate(time.Microsecond)
}

// Load returns every live record and remembers what it saw, so the next Save
// can tell which records were cleared since.
func (s *CooldownStore) Load(ctx context.Context) ([]cliproxyauth.CooldownStateRecord, error) {
	if s == nil || s.kv == nil {
		return nil, errors.New("nats cooldown store: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.kv.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("nats cooldown store: load state: %w", err)
	}
	records := make([]cliproxyauth.CooldownStateRecord, 0, len(entries))
	previous := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		var stored cooldownEntry
		if err := json.Unmarshal(entry.Value, &stored); err != nil {
			return nil, fmt.Errorf("nats cooldown store: decode state: %w", err)
		}
		if stored.Deleted || stored.Record == nil {
			continue
		}
		if cooldownID(*stored.Record) != entry.ID {
			return nil, errors.New("nats cooldown store: decoded state does not match its key")
		}
		records = append(records, *stored.Record)
		previous[entry.ID] = stored.UpdatedAt
	}
	s.previous = previous
	return records, nil
}

// Save writes the records and clears the ones a previous Load or Save knew that are
// missing now. A record newer than the one being written wins.
func (s *CooldownStore) Save(ctx context.Context, records []cliproxyauth.CooldownStateRecord) error {
	if s == nil || s.kv == nil {
		return errors.New("nats cooldown store: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := normalizeCooldownTime(time.Now(), time.Time{})
	current := make(map[string]time.Time, len(records))
	type write struct {
		id    string
		entry cooldownEntry
		ttl   time.Duration
	}
	writes := make([]write, 0, len(records))
	for i := range records {
		record := records[i]
		id := cooldownID(record)
		if id == "" {
			return errors.New("nats cooldown store: state has empty auth ID")
		}
		record.UpdatedAt = normalizeCooldownTime(record.UpdatedAt, now)
		current[id] = record.UpdatedAt
		writes = append(writes, write{id: id, entry: cooldownEntry{UpdatedAt: record.UpdatedAt, Record: &record}, ttl: cooldownTTL(record, now)})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range writes {
		if err := s.put(ctx, w.id, w.entry, w.ttl, func(stored cooldownEntry) bool {
			return !stored.UpdatedAt.After(w.entry.UpdatedAt)
		}); err != nil {
			return fmt.Errorf("nats cooldown store: save state: %w", err)
		}
	}
	for id, seen := range s.previous {
		if _, ok := current[id]; ok {
			continue
		}
		clearedAt := now
		if !clearedAt.After(seen) {
			clearedAt = seen.Add(time.Microsecond)
		}
		// Only the record this replica saw is cleared. One written since by
		// another replica, or already cleared, stays as it is.
		err := s.put(ctx, id, cooldownEntry{Deleted: true, UpdatedAt: clearedAt}, cooldownTombstoneTTL, func(stored cooldownEntry) bool {
			return !stored.Deleted && !stored.UpdatedAt.After(seen)
		})
		if err != nil {
			return fmt.Errorf("nats cooldown store: clear state: %w", err)
		}
	}
	s.previous = current
	return nil
}

// cooldownTTL keeps a record until its retry time has passed.
func cooldownTTL(record cliproxyauth.CooldownStateRecord, now time.Time) time.Duration {
	if record.NextRetryAfter.IsZero() {
		return cooldownIdleTTL
	}
	ttl := record.NextRetryAfter.Sub(now) + cooldownGrace
	if ttl < cooldownGrace {
		return cooldownGrace
	}
	return ttl
}

// put writes entry when the key is empty or replace allows it. A key's TTL is set
// only when it is created, so replacing a record deletes it at the revision that
// was read and creates it again; losing either step to another replica rereads.
func (s *CooldownStore) put(ctx context.Context, id string, entry cooldownEntry, ttl time.Duration, replace func(cooldownEntry) bool) error {
	value, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode %s: %w", id, err)
	}
	for range maxCooldownWriteAttempts {
		stored, err := s.kv.Get(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		default:
			var existing cooldownEntry
			if json.Unmarshal(stored.Value, &existing) == nil && !replace(existing) {
				return nil
			}
			if err := s.kv.Delete(ctx, id, stored.Revision); err != nil {
				if errors.Is(err, ErrConflict) {
					continue
				}
				return err
			}
		}
		if _, err := s.kv.CreateWithTTL(ctx, id, value, ttl); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return &ConflictError{Bucket: s.kv.bucket, ID: id}
}
