package turncost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	statusAllowed   = "allowed"
	statusForbidden = "forbidden"

	// DefaultForbiddenTTL is how long a probe result is remembered. A plan type does not
	// predict access, so each account is probed once and then trusted for a long time.
	DefaultForbiddenTTL = 90 * 24 * time.Hour
	// DefaultStateFileName sits next to the auth dir unless a state file is configured.
	DefaultStateFileName = "turncost-state.json"

	maxEntitlements   = 4096
	stateFileVersion  = 1
	refreshAllowedAge = 24 * time.Hour
	stateFilePermBits = 0o600
	stateDirPermBits  = 0o700
	stateFileTempBase = ".turncost-state-*.tmp"
)

type entitlement struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
}

type stateFile struct {
	Version  int                    `json:"version"`
	Accounts map[string]entitlement `json:"accounts"`
}

// entitlements remembers, per ChatGPT account, whether the estimates endpoint answered
// (allowed) or refused (forbidden). Accounts are keyed by a sha256 of the account id, so the
// raw id is never held in the persisted file. The file is best effort: a missing, corrupt,
// or unwritable file only costs one extra probe per account.
type entitlements struct {
	mu      sync.Mutex
	path    string
	ttl     time.Duration
	now     func() time.Time
	entries map[string]entitlement
}

func newEntitlements(now func() time.Time) *entitlements {
	return &entitlements{ttl: DefaultForbiddenTTL, now: now, entries: make(map[string]entitlement)}
}

func accountKey(accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return hex.EncodeToString(sum[:])
}

// configure applies the TTL and, when the path changes, loads the state from it.
func (e *entitlements) configure(path string, ttl time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ttl <= 0 {
		ttl = DefaultForbiddenTTL
	}
	e.ttl = ttl
	if path == e.path {
		return
	}
	e.path = path
	if path == "" {
		return
	}
	e.loadLocked()
}

func (e *entitlements) loadLocked() {
	data, err := os.ReadFile(e.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Debugf("codex turn costs: read state file: %v", err)
		}
		return
	}
	var decoded stateFile
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Version != stateFileVersion {
		log.Debug("codex turn costs: ignoring unreadable state file")
		return
	}
	now := e.now()
	for key, entry := range decoded.Accounts {
		if len(key) != sha256.Size*2 || (entry.Status != statusAllowed && entry.Status != statusForbidden) || e.expired(entry, now) {
			continue
		}
		if len(e.entries) >= maxEntitlements {
			break
		}
		e.entries[key] = entry
	}
}

func (e *entitlements) expired(entry entitlement, now time.Time) bool {
	return now.Sub(entry.CheckedAt) > e.ttl
}

// status returns the remembered status of the account, or "" when it is unknown or expired.
func (e *entitlements) status(accountID string) string {
	key := accountKey(accountID)
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.entries[key]
	if !ok {
		return ""
	}
	if e.expired(entry, e.now()) {
		delete(e.entries, key)
		return ""
	}
	return entry.Status
}

// set records a probe result and persists it when it changes what is remembered.
func (e *entitlements) set(accountID, status string) {
	key := accountKey(accountID)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if prev, ok := e.entries[key]; ok && prev.Status == status && now.Sub(prev.CheckedAt) < refreshAllowedAge && !e.expired(prev, now) {
		return
	} else if !ok && len(e.entries) >= maxEntitlements && !e.evictLocked(now) {
		return
	}
	e.entries[key] = entitlement{Status: status, CheckedAt: now}
	e.saveLocked()
}

// evictLocked frees one slot, dropping an expired entry first and the oldest otherwise.
func (e *entitlements) evictLocked(now time.Time) bool {
	var oldestKey string
	var oldest time.Time
	for key, entry := range e.entries {
		if e.expired(entry, now) {
			delete(e.entries, key)
			return true
		}
		if oldestKey == "" || entry.CheckedAt.Before(oldest) {
			oldestKey, oldest = key, entry.CheckedAt
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(e.entries, oldestKey)
	return true
}

func (e *entitlements) saveLocked() {
	if e.path == "" {
		return
	}
	data, err := json.Marshal(stateFile{Version: stateFileVersion, Accounts: e.entries})
	if err != nil {
		return
	}
	if err := writeFileAtomic(e.path, data); err != nil {
		log.Debugf("codex turn costs: write state file: %v", err)
	}
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, stateDirPermBits); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, stateFileTempBase)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmpName, stateFilePermBits)
	}
	if werr == nil {
		werr = os.Rename(tmpName, path)
	}
	if werr != nil {
		_ = os.Remove(tmpName)
	}
	return werr
}
