package usage

import (
	"net/http"
	"strings"
	"testing"
)

func TestTurnIDFromTurnMetadata(t *testing.T) {
	if got := TurnIDFromTurnMetadata(` {"turn_id":"abc","window_id":"w"} `); got != "abc" {
		t.Fatalf("got %q", got)
	}
	for _, in := range []string{"", "not json", `{"window_id":"w"}`, `{"turn_id":"` + strings.Repeat("x", 200) + `"}`} {
		if got := TurnIDFromTurnMetadata(in); got != "" {
			t.Errorf("TurnIDFromTurnMetadata(%q) = %q, want empty", in, got)
		}
	}
}

func TestTurnIDFromRequestBody(t *testing.T) {
	flat := []byte(`{"client_metadata":{"turn_id":"flat","x-codex-turn-metadata":"{\"turn_id\":\"nested\"}"}}`)
	if got := TurnIDFromRequestBody(flat); got != "flat" {
		t.Fatalf("flat = %q", got)
	}
	nested := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"nested\"}"}}`)
	if got := TurnIDFromRequestBody(nested); got != "nested" {
		t.Fatalf("nested = %q", got)
	}
	for _, in := range []string{``, `{}`, `{"client_metadata":{}}`, `nope`} {
		if got := TurnIDFromRequestBody([]byte(in)); got != "" {
			t.Errorf("TurnIDFromRequestBody(%q) = %q, want empty", in, got)
		}
	}
}

func TestThreadIDSources(t *testing.T) {
	if got := ThreadIDFromTurnMetadata(`{"turn_id":"t","thread_id":"th"}`); got != "th" {
		t.Fatalf("metadata = %q", got)
	}
	headers := http.Header{}
	headers.Set("Thread-Id", "from-header")
	if got := ThreadIDFromHeaders(headers); got != "from-header" {
		t.Fatalf("header fallback = %q", got)
	}
	headers.Set("X-Codex-Turn-Metadata", `{"thread_id":"from-meta"}`)
	if got := ThreadIDFromHeaders(headers); got != "from-meta" {
		t.Fatalf("metadata first = %q", got)
	}
	flat := []byte(`{"client_metadata":{"thread_id":"flat","x-codex-turn-metadata":"{\"thread_id\":\"nested\"}"}}`)
	if got := ThreadIDFromRequestBody(flat); got != "flat" {
		t.Fatalf("flat = %q", got)
	}
	nested := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"nested\"}"}}`)
	if got := ThreadIDFromRequestBody(nested); got != "nested" {
		t.Fatalf("nested = %q", got)
	}
	for _, in := range []string{``, `{}`, `nope`} {
		if got := ThreadIDFromRequestBody([]byte(in)); got != "" {
			t.Errorf("ThreadIDFromRequestBody(%q) = %q", in, got)
		}
	}
}
