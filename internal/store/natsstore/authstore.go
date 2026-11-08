package natsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// maxSaveAttempts bounds how often a save reloads the remote value and reapplies
// its changes after losing a revision race.
const maxSaveAttempts = 5

// AuthStore keeps credentials in the auth bucket, one key per credential file,
// with the file bytes as the value. The bucket is the source of truth; files
// under authDir only exist so serializers that write to a path keep working.
type AuthStore struct {
	kv      *KV
	authDir string

	mu sync.Mutex
	// revisions is the revision last read or written per credential, the
	// expectation a later save is checked against.
	revisions map[string]uint64
	// floor is the highest revision seen per credential from any source,
	// including deletes. A watched change at or below it is stale or an echo.
	floor map[string]uint64
}

// NewAuthStore creates a store over the auth bucket that renders files under authDir.
func NewAuthStore(kv *KV, authDir string) (*AuthStore, error) {
	abs, err := filepath.Abs(authDir)
	if err != nil {
		return nil, fmt.Errorf("nats store: resolve auth directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("nats store: create auth directory: %w", err)
	}
	return &AuthStore{kv: kv, authDir: abs, revisions: make(map[string]uint64), floor: make(map[string]uint64)}, nil
}

// AuthDir returns the directory credential files are rendered into.
func (s *AuthStore) AuthDir() string { return s.authDir }

// Snapshot reads every credential and remembers its revision for later saves.
func (s *AuthStore) Snapshot(ctx context.Context) ([]Entry, error) {
	entries, err := s.kv.List(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisions = make(map[string]uint64, len(entries))
	for _, entry := range entries {
		s.revisions[entry.ID] = entry.Revision
		s.noteRevision(entry.ID, entry.Revision)
	}
	return entries, nil
}

// List returns every stored credential.
func (s *AuthStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	entries, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	auths := make([]*cliproxyauth.Auth, 0, len(entries))
	for _, entry := range entries {
		path, err := s.absoluteAuthPath(entry.ID)
		if err != nil {
			log.WithError(err).Warnf("nats store: skipping auth %s outside spool", entry.ID)
			continue
		}
		metadata := make(map[string]any)
		if err := json.Unmarshal(entry.Value, &metadata); err != nil {
			log.WithError(err).Warnf("nats store: skipping auth %s with invalid json", entry.ID)
			continue
		}
		cliproxyauth.NormalizeCredentialMetadata(metadata)
		if err := cliproxyauth.ValidateAuthWeight(&cliproxyauth.Auth{Metadata: metadata}); err != nil {
			log.WithError(err).Warnf("nats store: skipping auth %s with invalid weight", entry.ID)
			continue
		}
		provider := strings.TrimSpace(valueAsString(metadata["type"]))
		if provider == "" {
			provider = "unknown"
		}
		attr := map[string]string{
			cliproxyauth.AttributePath:          path,
			cliproxyauth.AttributeSourceBackend: cliproxyauth.AuthSourceNATS,
		}
		if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
			attr["email"] = email
		}
		id := filepath.ToSlash(filepath.Clean(entry.ID))
		auth := &cliproxyauth.Auth{
			ID:         id,
			Provider:   provider,
			FileName:   id,
			Label:      labelFor(metadata),
			Status:     cliproxyauth.StatusActive,
			Attributes: attr,
			Metadata:   metadata,
			CreatedAt:  entry.Modified,
			UpdatedAt:  entry.Modified,
		}
		cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
		if disabled, ok := metadata["disabled"].(bool); ok && disabled {
			auth.Disabled = true
			auth.Status = cliproxyauth.StatusDisabled
		}
		auths = append(auths, auth)
	}
	return auths, nil
}

// Save renders the credential file and stores it with a revision check. When
// another writer got there first, the remote value is reloaded, its
// user-configured fields are merged into the credential and the save repeats.
func (s *AuthStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("nats store: auth is nil")
	}
	cliproxyauth.NormalizeCredentialMetadata(auth.Metadata)
	if err := cliproxyauth.ValidateAuthWeight(auth); err != nil {
		return "", fmt.Errorf("nats store: %w", err)
	}
	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	id, err := s.relativeAuthID(path)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Runtime updates must not recreate a disabled credential that was
	// deliberately removed. Login and migration callers mark the save when
	// creating a missing disabled credential is intentional.
	creationIntent := cliproxyauth.HasAuthCreationIntent(ctx)
	if auth.Disabled && !creationIntent {
		if _, err := s.kv.Get(ctx, id); errors.Is(err, ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", err
		}
	}

	// Rendering consumes these, so a repeated attempt restores them.
	storage, patch := auth.Storage, auth.CodexRefreshPatch
	for attempt := 0; attempt < maxSaveAttempts; attempt++ {
		data, err := s.render(ctx, path, auth)
		if err != nil {
			return "", err
		}
		expected := s.revisions[id]
		var rev uint64
		if expected == 0 {
			rev, err = s.kv.Create(ctx, id, data)
		} else {
			rev, err = s.kv.Update(ctx, id, data, expected)
		}
		if err == nil {
			s.revisions[id] = rev
			s.noteRevision(id, rev)
			s.finishSave(auth, path)
			return path, nil
		}
		if !errors.Is(err, ErrConflict) {
			return "", err
		}

		current, err := s.kv.Get(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			delete(s.revisions, id)
			if !creationIntent {
				log.Warnf("nats store: auth %s was removed by another writer, dropping the update", id)
				return "", nil
			}
		case err != nil:
			return "", err
		default:
			s.revisions[id] = current.Revision
			s.noteRevision(id, current.Revision)
			if err := s.adoptRemote(path, current.Value, auth); err != nil {
				return "", err
			}
		}
		auth.Storage, auth.CodexRefreshPatch = storage, patch
	}
	return "", &ConflictError{Bucket: s.kv.bucket, ID: id}
}

// adoptRemote makes the remote value the local file and folds its
// user-configured fields into the credential about to be saved again.
func (s *AuthStore) adoptRemote(path string, remote []byte, auth *cliproxyauth.Auth) error {
	if err := writeFileAtomic(path, remote); err != nil {
		return fmt.Errorf("nats store: write reloaded auth file: %w", err)
	}
	var existing map[string]any
	if err := json.Unmarshal(remote, &existing); err == nil {
		cliproxyauth.MergeExistingAuthMetadata(auth, existing)
	}
	return nil
}

func (s *AuthStore) finishSave(auth *cliproxyauth.Auth, path string) {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[cliproxyauth.AttributePath] = path
	auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceNATS
	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}
}

// render writes the credential file and returns exactly the bytes it holds.
func (s *AuthStore) render(ctx context.Context, path string, auth *cliproxyauth.Auth) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("nats store: create auth directory: %w", err)
	}
	if handled, err := cliproxyauth.PersistCodexAuthFile(ctx, path, auth); handled {
		if err != nil {
			return nil, err
		}
	} else {
		switch {
		case auth.Storage != nil:
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["disabled"] = auth.Disabled
			if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
				setter.SetMetadata(auth.Metadata)
			}
			if err := auth.Storage.SaveTokenToFile(path); err != nil {
				return nil, err
			}
		case auth.Metadata != nil:
			auth.Metadata["disabled"] = auth.Disabled
			raw, err := json.Marshal(auth.Metadata)
			if err != nil {
				return nil, fmt.Errorf("nats store: marshal metadata: %w", err)
			}
			existing, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("nats store: read existing metadata: %w", err)
			}
			if err != nil || !sameJSON(existing, raw) {
				if err := writeFileAtomic(path, raw); err != nil {
					return nil, fmt.Errorf("nats store: write auth file: %w", err)
				}
			}
		default:
			return nil, fmt.Errorf("nats store: nothing to persist for %s", auth.ID)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("nats store: read rendered auth file: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("nats store: rendered auth file for %s is empty", auth.ID)
	}
	return data, nil
}

// Delete removes the credential file and its stored value. The delete wins over
// a concurrent update, so it repeats against the latest revision.
func (s *AuthStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("nats store: id is empty")
	}
	path := id
	if !filepath.IsAbs(id) {
		path = filepath.Join(s.authDir, filepath.FromSlash(id))
	}
	relID, err := s.relativeAuthID(path)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("nats store: delete auth file: %w", err)
	}
	for attempt := 0; attempt < maxSaveAttempts; attempt++ {
		current, err := s.kv.Get(ctx, relID)
		if errors.Is(err, ErrNotFound) {
			delete(s.revisions, relID)
			return nil
		}
		if err != nil {
			return err
		}
		err = s.kv.Delete(ctx, relID, current.Revision)
		if err == nil {
			delete(s.revisions, relID)
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return &ConflictError{Bucket: s.kv.bucket, ID: relID}
}

func (s *AuthStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		return filepath.Join(s.authDir, fileName), nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("nats store: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(auth.ID)), nil
}

// relativeAuthID returns the slash separated credential id for a path inside authDir.
func (s *AuthStore) relativeAuthID(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.authDir, path)
	}
	rel, err := filepath.Rel(s.authDir, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("nats store: path %s outside managed directory", path)
	}
	return filepath.ToSlash(rel), nil
}

// absoluteAuthPath maps a stored id to its file path, refusing ids that would escape authDir.
func (s *AuthStore) absoluteAuthPath(id string) (string, error) {
	path := filepath.Join(s.authDir, filepath.FromSlash(id))
	if _, err := s.relativeAuthID(path); err != nil {
		return "", fmt.Errorf("nats store: invalid auth identifier %s", id)
	}
	return path, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func sameJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func valueAsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func labelFor(metadata map[string]any) string {
	for _, key := range []string{"label", "email", "project_id"} {
		if v := strings.TrimSpace(valueAsString(metadata[key])); v != "" {
			return v
		}
	}
	return ""
}

// PutFile stores the bytes of a credential file that something else already
// wrote into the spool. A file equal to the stored value is a no-op, which
// absorbs the watcher echoing this process's own writes. Otherwise the write is
// checked against the last revision seen, and a stale or removed credential is
// reported as a conflict, so a stale local file never replaces newer
// credentials.
func (s *AuthStore) PutFile(ctx context.Context, id string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	expected := s.revisions[id]
	current, err := s.kv.Get(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		if expected != 0 {
			return &ConflictError{Bucket: s.kv.bucket, ID: id}
		}
		rev, err := s.kv.Create(ctx, id, data)
		if err != nil {
			return err
		}
		s.revisions[id] = rev
		s.noteRevision(id, rev)
		return nil
	case err != nil:
		return err
	case sameJSON(current.Value, data):
		s.revisions[id] = current.Revision
		s.noteRevision(id, current.Revision)
		return nil
	case current.Revision != expected:
		return &ConflictError{Bucket: s.kv.bucket, ID: id}
	}
	rev, err := s.kv.Update(ctx, id, data, expected)
	if err != nil {
		return err
	}
	s.revisions[id] = rev
	s.noteRevision(id, rev)
	return nil
}

// noteRevision raises the ordering floor. It expects s.mu to be held.
func (s *AuthStore) noteRevision(id string, rev uint64) {
	if rev > s.floor[id] {
		s.floor[id] = rev
	}
}

// DeleteObserved removes a credential whose file disappeared from the spool,
// but only the revision this store has seen. A credential it never saw, or one
// that changed since, is left alone: the file went missing because a remote
// delete was applied or a remote change has not arrived yet, not because the
// credential should go.
func (s *AuthStore) DeleteObserved(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	observed := s.revisions[id]
	if observed == 0 {
		return nil
	}
	err := s.kv.Delete(ctx, id, observed)
	if err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	// Either the delete landed or a newer revision won; the watch reports it.
	delete(s.revisions, id)
	return nil
}
