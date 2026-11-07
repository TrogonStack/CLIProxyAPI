package turncost

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newTestStore(ttl time.Duration, maxTurns, maxResponses int) (*Store, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	s := NewStore(ttl, maxTurns, maxResponses)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestStoreAddAndGet(t *testing.T) {
	s, _ := newTestStore(time.Hour, 10, 10)
	s.Add("turn-1", Response{ID: "r1", Usage: Usage{InputTokens: 10}})
	s.Add("turn-1", Response{ID: "r2", Usage: Usage{InputTokens: 20}})
	turn, ok := s.Get("turn-1")
	if !ok || len(turn.Responses) != 2 || turn.Responses[1].Usage.InputTokens != 20 {
		t.Fatalf("Get() = %+v, %v", turn, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("unexpected hit for missing turn")
	}
}

func TestStoreReplacesRepeatedResponseID(t *testing.T) {
	s, _ := newTestStore(time.Hour, 10, 10)
	s.Add("turn-1", Response{ID: "r1", Usage: Usage{InputTokens: 1}})
	s.Add("turn-1", Response{ID: "r1", Usage: Usage{InputTokens: 2}})
	turn, _ := s.Get("turn-1")
	if len(turn.Responses) != 1 || turn.Responses[0].Usage.InputTokens != 2 {
		t.Fatalf("responses = %+v", turn.Responses)
	}
}

func TestStoreCapsResponsesPerTurn(t *testing.T) {
	s, _ := newTestStore(time.Hour, 10, 3)
	for i := 0; i < 10; i++ {
		s.Add("turn-1", Response{ID: fmt.Sprintf("r%d", i)})
	}
	turn, _ := s.Get("turn-1")
	if len(turn.Responses) != 3 {
		t.Fatalf("len(responses) = %d, want 3", len(turn.Responses))
	}
}

func TestStoreEvictsLeastRecentlyUpdatedTurn(t *testing.T) {
	s, _ := newTestStore(time.Hour, 2, 10)
	s.Add("a", Response{ID: "r"})
	s.Add("b", Response{ID: "r"})
	s.Add("a", Response{ID: "r2"}) // a is now newest
	s.Add("c", Response{ID: "r"})
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
	if _, ok := s.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a should have been kept")
	}
}

func TestStoreExpiresTurns(t *testing.T) {
	s, now := newTestStore(time.Hour, 10, 10)
	s.Add("old", Response{ID: "r"})
	*now = now.Add(2 * time.Hour)
	if _, ok := s.Get("old"); ok {
		t.Fatal("expired turn should be gone")
	}
	s.Add("old2", Response{ID: "r"})
	*now = now.Add(2 * time.Hour)
	s.Add("new", Response{ID: "r"})
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after prune on add", s.Len())
	}
}

func TestStoreRejectsOversizedAndEmptyIDs(t *testing.T) {
	s, _ := newTestStore(time.Hour, 10, 10)
	long := make([]byte, maxIDLength+1)
	for i := range long {
		long[i] = 'x'
	}
	s.Add(string(long), Response{ID: "r"})
	s.Add("turn", Response{ID: string(long)})
	s.Add("", Response{ID: "r"})
	s.Add("turn", Response{})
	if s.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", s.Len())
	}
}

func TestStoreTracksAuthAndThread(t *testing.T) {
	s := NewStore(time.Hour, 10, 10)
	s.Add("t", Response{ID: "r1", AuthID: "a", ThreadID: "th"})
	s.Add("t", Response{ID: "r2", AuthID: "a"})
	turn, _ := s.Get("t")
	if turn.AuthID != "a" || turn.ThreadID != "th" || turn.MixedAuth {
		t.Fatalf("turn = %+v", turn)
	}
	s.Add("t", Response{ID: "r3", AuthID: "b", ThreadID: "th"})
	if turn, _ = s.Get("t"); turn.AuthID != "b" || !turn.MixedAuth {
		t.Fatalf("turn = %+v, want mixed", turn)
	}
	s.Add("long", Response{ID: "r", AuthID: strings.Repeat("x", maxIDLength+1)})
	if turn, _ = s.Get("long"); turn.AuthID != "" {
		t.Fatalf("overlong auth id kept: %q", turn.AuthID)
	}
}
