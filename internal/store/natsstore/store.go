package natsstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Store is the NATS-backed token store. The buckets are the source of truth and
// the spool directory is a disposable mirror rebuilt on every start, so the
// config and credential files the rest of the process reads keep working.
type Store struct {
	nc         *nats.Conn
	replicaID  string
	auth       *AuthStore
	config     *ConfigStore
	journal    *RefreshJournal
	refresh    *RefreshQueue
	leases     *KV
	cooldown   *CooldownStore
	spoolRoot  string
	configPath string

	syncMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// configMu keeps a watched config landing in the spool and the revision it
	// advances to together, so a persist never reads the file between them and
	// writes an older config over the newer one.
	configMu sync.Mutex

	mu sync.Mutex
	// configData is the config last written to or read from the spool, so the
	// watcher echoing that write back is not stored again.
	configData []byte
}

// NewStore wires a bootstrapped connection to a spool rooted at spoolDir. The
// store closes nc when it is closed.
func NewStore(nc *nats.Conn, boot *Bootstrapped, settings StoreSettings, spoolDir string) (*Store, error) {
	root, err := filepath.Abs(spoolDir)
	if err != nil {
		return nil, fmt.Errorf("nats store: resolve spool directory: %w", err)
	}
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("nats store: create config directory: %w", err)
	}
	replicaID, err := newReplicaID()
	if err != nil {
		return nil, err
	}
	origin := WithOrigin(replicaID)
	auth, err := NewAuthStore(NewKV(settings.BucketName(BucketAuth), boot.Bucket(settings, BucketAuth), origin), filepath.Join(root, "auths"))
	if err != nil {
		return nil, err
	}
	cfg := NewConfigStore(NewKV(settings.BucketName(BucketConfig), boot.Bucket(settings, BucketConfig), origin))
	return &Store{
		nc:         nc,
		replicaID:  replicaID,
		auth:       auth,
		config:     cfg,
		journal:    NewRefreshJournal(NewKV(settings.BucketName(BucketRefresh), boot.Bucket(settings, BucketRefresh), origin)),
		refresh:    boot.Refresh,
		leases:     NewKV(settings.BucketName(BucketLease), boot.Bucket(settings, BucketLease), origin),
		cooldown:   NewCooldownStore(NewKV(settings.BucketName(BucketCooldown), boot.Bucket(settings, BucketCooldown), origin)),
		spoolRoot:  root,
		configPath: filepath.Join(configDir, "config.yaml"),
	}, nil
}

// newReplicaID returns an identifier unique to this process. It is never
// reused across restarts, so values a previous run wrote are not mistaken for
// this run's own.
func newReplicaID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("nats store: generate replica id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// ReplicaID identifies this process in the values it writes.
func (s *Store) ReplicaID() string { return s.replicaID }

// StartSync follows the buckets until the store is closed or ctx ends, mirroring
// changes other replicas make into the spool. It returns at once, runs in the
// background and does nothing when already started. Bootstrap first, so the
// watch only has to catch up on what changed since.
func (s *Store) StartSync(ctx context.Context) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	s.done = done
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.auth.kv.Follow(ctx, s.auth.applyEvent)
	}()
	go func() {
		defer wg.Done()
		s.config.kv.Follow(ctx, s.applyConfigEvent)
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
}

func (s *Store) stopSync() {
	s.syncMu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.syncMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Close stops following the buckets and closes the NATS connection.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.stopSync()
	if s.nc != nil {
		s.nc.Close()
	}
	return nil
}

// ConfigPath returns the config file inside the spool.
func (s *Store) ConfigPath() string { return s.configPath }

// AuthDir returns the directory credential files are mirrored into.
func (s *Store) AuthDir() string { return s.auth.AuthDir() }

// WorkDir returns the spool root.
func (s *Store) WorkDir() string { return s.spoolRoot }

// SetBaseDir satisfies the optional authenticator interface; the store owns its workspace.
func (s *Store) SetBaseDir(string) {}

// Save stores a credential.
func (s *Store) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	return s.auth.Save(ctx, auth)
}

// List returns every stored credential.
func (s *Store) List(ctx context.Context) ([]*cliproxyauth.Auth, error) { return s.auth.List(ctx) }

// Delete removes a credential.
func (s *Store) Delete(ctx context.Context, id string) error { return s.auth.Delete(ctx, id) }

// Bootstrap rebuilds the spool from the buckets. A config missing from the
// bucket is seeded from the local file or the example template, and replicas
// starting together converge on whichever one created it first. Running it
// again changes nothing.
func (s *Store) Bootstrap(ctx context.Context, exampleConfigPath string) error {
	if err := s.syncConfig(ctx, exampleConfigPath); err != nil {
		return err
	}
	return s.syncAuth(ctx)
}

func (s *Store) syncConfig(ctx context.Context, exampleConfigPath string) error {
	data, _, err := s.config.Read(ctx)
	if errors.Is(err, ErrNotFound) {
		if err := s.seedConfig(ctx, exampleConfigPath); err == nil {
			return nil
		} else if !errors.Is(err, ErrConflict) {
			return err
		}
		// Another replica seeded it first; adopt theirs.
		data, _, err = s.config.Read(ctx)
	}
	if err != nil {
		return fmt.Errorf("nats store: load config: %w", err)
	}
	return s.writeConfigFile(data)
}

// seedConfig stores the local config, or the example template, in the bucket.
func (s *Store) seedConfig(ctx context.Context, exampleConfigPath string) error {
	if _, err := os.Stat(s.configPath); errors.Is(err, fs.ErrNotExist) {
		if exampleConfigPath != "" {
			if err := misc.CopyConfigTemplate(exampleConfigPath, s.configPath); err != nil {
				return fmt.Errorf("nats store: copy example config: %w", err)
			}
		} else if err := os.WriteFile(s.configPath, nil, 0o600); err != nil {
			return fmt.Errorf("nats store: create empty config: %w", err)
		}
	}
	// Seed with the file the startup load leaves behind, so a plaintext
	// management secret never reaches the bucket.
	if err := config.PrepareConfigFileForUpload(s.configPath); err != nil {
		return fmt.Errorf("nats store: prepare local config for seeding: %w", err)
	}
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("nats store: read local config: %w", err)
	}
	if _, err := s.config.Write(ctx, data); err != nil {
		return err
	}
	s.rememberConfig(data)
	return nil
}

func (s *Store) writeConfigFile(data []byte) error {
	if err := writeFileAtomic(s.configPath, data); err != nil {
		return fmt.Errorf("nats store: write config to spool: %w", err)
	}
	s.rememberConfig(data)
	return nil
}

func (s *Store) rememberConfig(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configData = []byte(normalizeLineEndings(string(data)))
}

func (s *Store) syncAuth(ctx context.Context) error {
	entries, err := s.auth.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("nats store: load auth: %w", err)
	}
	if err := os.RemoveAll(s.auth.AuthDir()); err != nil {
		return fmt.Errorf("nats store: reset auth directory: %w", err)
	}
	if err := os.MkdirAll(s.auth.AuthDir(), 0o700); err != nil {
		return fmt.Errorf("nats store: recreate auth directory: %w", err)
	}
	for _, entry := range entries {
		path, err := s.auth.absoluteAuthPath(entry.ID)
		if err != nil {
			log.WithError(err).Warnf("nats store: skipping auth %s outside spool", entry.ID)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("nats store: create auth subdirectory: %w", err)
		}
		if err := os.WriteFile(path, entry.Value, 0o600); err != nil {
			return fmt.Errorf("nats store: write auth file: %w", err)
		}
	}
	return nil
}

// PersistConfig stores the spool config file in the bucket. A missing or empty
// file is ignored, since it is more likely a write in progress than an intent
// to wipe the config every replica shares.
func (s *Store) PersistConfig(ctx context.Context) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Warn("nats store: config file is missing, not persisting")
			return nil
		}
		return fmt.Errorf("nats store: read config file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		log.Warn("nats store: config file is empty, not persisting")
		return nil
	}
	s.mu.Lock()
	unchanged := bytes.Equal(s.configData, []byte(normalizeLineEndings(string(data))))
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	if _, err := s.config.Write(ctx, data); err != nil {
		return fmt.Errorf("nats store: persist config: %w", err)
	}
	s.rememberConfig(data)
	return nil
}

// CommitConfig stores the spool config file now and reports a lost
// compare-and-swap as a ConflictError, for callers that must tell their user
// the edit did not land. On a conflict the spool adopts the config that won, so
// the next edit builds on it.
func (s *Store) CommitConfig(ctx context.Context) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("nats store: read config file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	s.mu.Lock()
	unchanged := bytes.Equal(s.configData, []byte(normalizeLineEndings(string(data))))
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	_, err = s.config.Write(ctx, data)
	if err == nil {
		s.rememberConfig(data)
		return nil
	}
	if !errors.Is(err, ErrConflict) {
		return fmt.Errorf("nats store: commit config: %w", err)
	}
	remote, _, errRead := s.config.Read(ctx)
	if errRead != nil {
		return fmt.Errorf("nats store: adopt the winning config: %w", errRead)
	}
	if errWrite := s.writeConfigFile(remote); errWrite != nil {
		return errWrite
	}
	return err
}

// PersistAuthFiles stores the given spool credential files in the bucket, and
// removes the credentials whose files are gone or empty.
func (s *Store) PersistAuthFiles(ctx context.Context, _ string, paths ...string) error {
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		id, err := s.auth.relativeAuthID(path)
		if err != nil {
			log.WithError(err).Warnf("nats store: ignoring auth path %s", path)
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.auth.AuthDir(), filepath.FromSlash(id)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("nats store: read auth file: %w", err)
		}
		if err != nil || len(data) == 0 {
			if err := s.auth.DeleteObserved(ctx, id); err != nil {
				return err
			}
			continue
		}
		if err := s.auth.PutFile(ctx, id, data); err != nil {
			return err
		}
	}
	return nil
}
