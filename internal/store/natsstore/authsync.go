package natsstore

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

// applyEvent mirrors a watched bucket event into the spool, where the file
// watcher and synthesizer pick it up like any other edit.
func (s *AuthStore) applyEvent(ev Event) {
	if ev.Synced {
		s.prune(ev.Initial, ev.InitialMax)
		return
	}
	if ev.Change == nil {
		return
	}
	if err := s.applyChange(*ev.Change); err != nil {
		log.WithError(err).Warnf("nats store: failed to apply auth %s to the spool", ev.Change.ID)
	}
}

// applyChange writes or removes one credential file. A change at or below the
// highest revision already seen is dropped, which orders replays and discards
// the bucket echoing this store's own writes.
func (s *AuthStore) applyChange(change Change) error {
	path, err := s.absoluteAuthPath(change.ID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if change.Revision <= s.floor[change.ID] {
		return nil
	}
	if change.Deleted {
		s.noteRevision(change.ID, change.Revision)
		delete(s.revisions, change.ID)
		return removeSpoolFile(path)
	}
	// The spool already holds what this replica wrote, and may hold a newer
	// local edit that rewriting it would lose.
	own := change.Origin != "" && change.Origin == s.kv.Origin()
	if !own {
		if err := writeSpoolFile(path, change.Value); err != nil {
			return err
		}
	}
	s.noteRevision(change.ID, change.Revision)
	s.revisions[change.ID] = change.Revision
	return nil
}

// prune forgets credentials the bucket no longer holds. A delete that happened
// while the watch was down leaves no event behind once its marker is gone, so
// the replay is compared with what this store last saw. A credential written
// after the replay was taken is newer than every replayed revision and stays.
func (s *AuthStore) prune(initial map[string]struct{}, initialMax uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rev := range s.revisions {
		if _, ok := initial[id]; ok || rev > initialMax {
			continue
		}
		path, err := s.absoluteAuthPath(id)
		if err != nil {
			continue
		}
		delete(s.revisions, id)
		if err := removeSpoolFile(path); err != nil {
			log.WithError(err).Warnf("nats store: failed to remove auth %s from the spool", id)
		}
	}
}

// writeSpoolFile replaces a spool file unless it already holds the bytes, so a
// repeated value produces no file event for the watcher to react to.
func writeSpoolFile(path string, data []byte) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func removeSpoolFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
