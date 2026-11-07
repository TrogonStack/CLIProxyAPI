package turncost

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const specAccount = "acct-spec"

// fixture is one captured upstream exchange from testdata/thread_estimates.
type fixture struct {
	Case        string          `json:"case"`
	Request     json.RawMessage `json:"request"`
	Status      int             `json:"status"`
	ContentType string          `json:"content_type"`
	Response    json.RawMessage `json:"response"`
	ResponseRaw string          `json:"response_raw"`
}

func (f fixture) body() []byte {
	if len(f.Response) > 0 {
		return f.Response
	}
	return []byte(f.ResponseRaw)
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "thread_estimates", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if f.Case != name {
		t.Fatalf("%s: case field is %q", name, f.Case)
	}
	return f
}

// turnsOf lists the (thread, turn) pairs a fixture asks about. Fixtures whose request is
// malformed or empty are about a client bug, so a default pair stands in for the real query.
func turnsOf(f fixture) (pairs [][2]string) {
	var req estimatesRequest
	if err := json.Unmarshal(f.Request, &req); err == nil {
		for _, th := range req.Threads {
			for _, turn := range th.TurnIDs {
				pairs = append(pairs, [2]string{th.ThreadID, turn})
			}
		}
	}
	if len(pairs) == 0 {
		pairs = [][2]string{{"thread-default", "turn-default"}}
	}
	return pairs
}

// settledOf returns the settled response ids the fixture reports per turn.
func settledOf(f fixture) map[string][]string {
	var resp estimatesResponse
	out := map[string][]string{}
	if err := json.Unmarshal(f.Response, &resp); err != nil {
		return out
	}
	for _, th := range resp.Threads {
		for _, turn := range th.Turns {
			if len(turn.SettledResponseIDs) > 0 {
				out[turn.TurnID] = turn.SettledResponseIDs
			}
		}
	}
	return out
}

func TestUpstreamSpec(t *testing.T) {
	realPriced := map[string]string{"002": "0.465710", "003": "3.077125", "004": "1.234598", "005": "0.164589", "006": "1.984503"}
	tests := []struct {
		name string
		// priced maps the last digits of a turn id to the total_usd it must report from
		// upstream. Every other turn must fall back to the local price.
		priced map[string]string
		// cache is the entitlement remembered for the account afterwards.
		cache string
	}{
		{name: "business_real_turns", priced: realPriced, cache: statusAllowed},
		{name: "business_without_settled_ids", cache: statusAllowed},
		{name: "business_unknown_turn"},
		{name: "business_missing_account_header"},
		{name: "business_mixed_known_unknown", priced: realPriced, cache: statusAllowed},
		{name: "business_empty_threads"},
		{name: "business_malformed_body"},
		{name: "prolite_forbidden", cache: statusForbidden},
		{name: "free_forbidden", cache: statusForbidden},
		{name: "invalid_token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := loadFixture(t, tc.name)
			u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
				w.Header().Set("Content-Type", f.ContentType)
				w.WriteHeader(f.Status)
				_, _ = w.Write(f.body())
			})
			s := upstreamService(t, u, codexAuth("auth-spec", "tok", specAccount))

			settled := settledOf(f)
			var ids []string
			for _, pair := range turnsOf(f) {
				thread, turn := pair[0], pair[1]
				ids = append(ids, turn)
				responses := settled[turn]
				if len(responses) == 0 {
					responses = []string{"local-" + turn}
				}
				for _, resp := range responses {
					rec := codexRecord(turn, resp, "gpt-5.1-codex", coreusage.Detail{InputTokens: 1000})
					rec.AuthID, rec.ThreadID = "auth-spec", thread
					s.HandleUsage(context.Background(), rec)
				}
			}

			for _, got := range s.Query(context.Background(), ids) {
				suffix := got.TurnID[max(0, len(got.TurnID)-3):]
				if want, ok := tc.priced[suffix]; ok {
					if got.Status != statusPriced || got.TotalUSD != want || len(got.Responses) != 0 {
						t.Errorf("turn %s = %+v, want upstream price %s", suffix, got, want)
					}
					if got.EventCount == nil || *got.EventCount != len(settled[got.TurnID]) {
						t.Errorf("turn %s event_count = %v, want %d", suffix, got.EventCount, len(settled[got.TurnID]))
					}
					continue
				}
				// Local fallback: priced from the one local response, never an empty or zero amount.
				if got.Status != statusPriced || len(got.Responses) != 1 || got.TotalUSD == "" || strings.Trim(got.TotalUSD, "0.") == "" {
					t.Errorf("turn %s = %+v, want local fallback", suffix, got)
				}
			}
			if got := s.entitlements.status(specAccount); got != tc.cache {
				t.Errorf("entitlement = %q, want %q", got, tc.cache)
			}
		})
	}
}

// TestUpstreamSpecNullTurnWithoutLocalPriceIsPending covers the unknown-turn fixture when the
// local table cannot price the model either: the answer is pending, not $0.
func TestUpstreamSpecNullTurnWithoutLocalPriceIsPending(t *testing.T) {
	f := loadFixture(t, "business_unknown_turn")
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		w.Header().Set("Content-Type", f.ContentType)
		_, _ = w.Write(f.body())
	})
	s := upstreamService(t, u, codexAuth("auth-spec", "tok", specAccount))
	pair := turnsOf(f)[0]
	rec := codexRecord(pair[1], "local-resp", "no-such-model", coreusage.Detail{InputTokens: 1000})
	rec.AuthID, rec.ThreadID = "auth-spec", pair[0]
	s.HandleUsage(context.Background(), rec)

	got := s.Query(context.Background(), []string{pair[1]})[0]
	if got.Status != statusPending || got.TotalUSD != "" {
		t.Fatalf("Query() = %+v, want pending", got)
	}
}
