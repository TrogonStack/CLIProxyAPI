package natsstore

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// configID is the single key holding config.yaml in the config bucket.
const configID = "config.yaml"

// ConfigStore keeps the configuration file as one value in the config bucket.
// Every write is checked against the revision this store last read or wrote,
// and a lost race is returned as a conflict instead of overwriting the newer
// config, since operator edits are never merged.
type ConfigStore struct {
	kv *KV

	mu       sync.Mutex
	revision uint64
}

// NewConfigStore creates a store over the config bucket.
func NewConfigStore(kv *KV) *ConfigStore { return &ConfigStore{kv: kv} }

// Read returns the stored config and remembers its revision for the next
// write. It returns ErrNotFound when no config was stored yet.
func (s *ConfigStore) Read(ctx context.Context) ([]byte, uint64, error) {
	entry, err := s.kv.Get(ctx, configID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.revision = 0
		}
		return nil, 0, err
	}
	s.revision = entry.Revision
	return entry.Value, entry.Revision, nil
}

// Revision returns the revision the next write is checked against, zero when
// no config is known to exist.
func (s *ConfigStore) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Write stores the config. It creates the key when no config is known to
// exist and otherwise replaces the revision last seen, returning a
// ConflictError when another writer changed it since.
func (s *ConfigStore) Write(ctx context.Context, data []byte) (uint64, error) {
	data = []byte(normalizeLineEndings(string(data)))
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		rev uint64
		err error
	)
	if s.revision == 0 {
		rev, err = s.kv.Create(ctx, configID, data)
	} else {
		rev, err = s.kv.Update(ctx, configID, data, s.revision)
	}
	if err != nil {
		return 0, err
	}
	s.revision = rev
	return rev, nil
}

func normalizeLineEndings(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
