package turncost

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTTL is how long a turn stays queryable after its last response.
	DefaultTTL = 24 * time.Hour
	// DefaultMaxTurns caps the number of retained turns.
	DefaultMaxTurns = 20000
	// DefaultMaxResponsesPerTurn caps the responses retained for one turn.
	DefaultMaxResponsesPerTurn = 1024

	maxIDLength    = 128
	maxLabelLength = 128
)

// Response is the usage of one completed upstream response.
type Response struct {
	ID    string
	Usage Usage
	// Model is the model the upstream reported serving, and RequestedModel the one the
	// client asked for. Either can be empty.
	Model           string
	RequestedModel  string
	ReasoningEffort string
}

// Turn is a copy of the responses recorded for one turn.
type Turn struct {
	ID        string
	Responses []Response
}

type turnEntry struct {
	id        string
	updatedAt time.Time
	responses []Response
	index     map[string]int
}

// Store keeps per-turn response usage in memory. It is bounded by turn count,
// responses per turn, and TTL, so a flood of unique ids cannot grow memory without limit.
type Store struct {
	mu           sync.Mutex
	ttl          time.Duration
	maxTurns     int
	maxResponses int
	now          func() time.Time
	turns        map[string]*list.Element
	order        *list.List // front is the most recently updated turn
}

// NewStore returns a store with the given bounds; non-positive values use the defaults.
func NewStore(ttl time.Duration, maxTurns, maxResponses int) *Store {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	if maxResponses <= 0 {
		maxResponses = DefaultMaxResponsesPerTurn
	}
	return &Store{
		ttl:          ttl,
		maxTurns:     maxTurns,
		maxResponses: maxResponses,
		now:          time.Now,
		turns:        make(map[string]*list.Element),
		order:        list.New(),
	}
}

// Add records a response under turnID. A repeated response id replaces the earlier record.
func (s *Store) Add(turnID string, resp Response) {
	if s == nil || turnID == "" || resp.ID == "" || len(turnID) > maxIDLength || len(resp.ID) > maxIDLength {
		return
	}
	turnID = strings.Clone(turnID)
	resp.ID = strings.Clone(resp.ID)
	resp.Model = clip(resp.Model)
	resp.RequestedModel = clip(resp.RequestedModel)
	resp.ReasoningEffort = clip(resp.ReasoningEffort)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)

	var entry *turnEntry
	if elem, ok := s.turns[turnID]; ok {
		entry = elem.Value.(*turnEntry)
		s.order.MoveToFront(elem)
	} else {
		entry = &turnEntry{id: turnID, index: make(map[string]int)}
		s.turns[turnID] = s.order.PushFront(entry)
	}
	entry.updatedAt = now
	if idx, ok := entry.index[resp.ID]; ok {
		entry.responses[idx] = resp
	} else if len(entry.responses) < s.maxResponses {
		entry.index[resp.ID] = len(entry.responses)
		entry.responses = append(entry.responses, resp)
	}
	for s.order.Len() > s.maxTurns {
		s.removeLocked(s.order.Back())
	}
}

// Get returns a copy of the turn, if present and not expired.
func (s *Store) Get(turnID string) (Turn, bool) {
	if s == nil {
		return Turn{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	elem, ok := s.turns[turnID]
	if !ok {
		return Turn{}, false
	}
	entry := elem.Value.(*turnEntry)
	if s.now().Sub(entry.updatedAt) > s.ttl {
		s.removeLocked(elem)
		return Turn{}, false
	}
	return Turn{ID: entry.id, Responses: append([]Response(nil), entry.responses...)}, true
}

// Len returns the number of retained turns.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

func (s *Store) pruneLocked(now time.Time) {
	for {
		back := s.order.Back()
		if back == nil || now.Sub(back.Value.(*turnEntry).updatedAt) <= s.ttl {
			return
		}
		s.removeLocked(back)
	}
}

func (s *Store) removeLocked(elem *list.Element) {
	if elem == nil {
		return
	}
	delete(s.turns, elem.Value.(*turnEntry).id)
	s.order.Remove(elem)
}

func clip(value string) string {
	if len(value) > maxLabelLength {
		value = value[:maxLabelLength]
	}
	return strings.Clone(value)
}
