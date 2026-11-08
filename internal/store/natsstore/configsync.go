package natsstore

import (
	"bytes"

	log "github.com/sirupsen/logrus"
)

// applyConfigEvent mirrors a watched config change into the spool, where the
// file watcher reloads it through the path every config edit already takes.
func (s *Store) applyConfigEvent(ev Event) {
	if ev.Change == nil || ev.Change.ID != configID {
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	data, ok := s.config.observe(*ev.Change)
	if !ok {
		return
	}
	s.mu.Lock()
	same := bytes.Equal(s.configData, []byte(normalizeLineEndings(string(data))))
	s.mu.Unlock()
	if same {
		// Already in the spool, so rewriting it would only wake the watcher.
		return
	}
	if err := s.writeConfigFile(data); err != nil {
		log.WithError(err).Warn("nats store: failed to apply the config to the spool")
	}
}
