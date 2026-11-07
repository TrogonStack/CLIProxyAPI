package turncost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// fakeSource serves credentials from memory and sends requests with a bearer token the way
// the executor does.
type fakeSource struct {
	auths map[string]*cliproxyauth.Auth
}

func (f *fakeSource) GetByID(id string) (*cliproxyauth.Auth, bool) {
	a, ok := f.auths[id]
	return a, ok
}

func (f *fakeSource) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	token, _ := auth.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req.WithContext(ctx))
}

func codexAuth(id, token, account string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "codex",
		Metadata: map[string]any{"access_token": token, "account_id": account},
	}
}

type upstreamCall struct {
	token   string
	account string
	body    estimatesRequest
}

type fakeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []upstreamCall
	hits   atomic.Int32
}

func (u *fakeUpstream) recorded() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

func newFakeUpstream(t *testing.T, handler func(call upstreamCall, w http.ResponseWriter)) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != estimatesPath {
			http.NotFound(w, r)
			return
		}
		call := upstreamCall{token: r.Header.Get("Authorization"), account: r.Header.Get("Chatgpt-Account-Id")}
		if err := json.NewDecoder(r.Body).Decode(&call.body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.calls = append(u.calls, call)
		u.mu.Unlock()
		handler(call, w)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func upstreamService(t *testing.T, u *fakeUpstream, auths ...*cliproxyauth.Auth) *Service {
	t.Helper()
	s := NewService(NewStore(time.Hour, 100, 100))
	s.Configure(&config.Config{CodexTurnCosts: config.CodexTurnCostsConfig{Enabled: true, UpstreamEstimates: true}})
	src := &fakeSource{auths: map[string]*cliproxyauth.Auth{}}
	for _, a := range auths {
		src.auths[a.ID] = a
	}
	s.SetAuthSource(src)
	s.upstream.baseURL = u.server.URL
	return s
}

func record(authID, threadID, turnID, responseID string, input int64) coreusage.Record {
	rec := codexRecord(turnID, responseID, "gpt-5.1-codex", coreusage.Detail{InputTokens: input})
	rec.AuthID = authID
	rec.ThreadID = threadID
	return rec
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestQueryPassesThroughUpstreamEstimate(t *testing.T) {
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		writeJSON(w, `{"threads":[{"thread_id":"th-1","turns":[{"turn_id":"turn-1","model":"gpt-5.6","estimated_usage_usd_micros":465710,"settled_response_ids":["r1","r2","r3"]}]}]}`)
	})
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"))
	s.HandleUsage(context.Background(), record("auth-a", "th-1", "turn-1", "r1", 1000))
	s.HandleUsage(context.Background(), record("auth-a", "th-1", "turn-1", "r2", 1000))

	got := s.Query(context.Background(), []string{"turn-1"})[0]
	if got.Status != statusPriced || got.TotalUSD != "0.465710" || got.Model != "gpt-5.6" {
		t.Fatalf("Query() = %+v", got)
	}
	if got.EventCount == nil || *got.EventCount != 3 || len(got.Responses) != 0 {
		t.Fatalf("event count = %v, responses = %v", got.EventCount, got.Responses)
	}
	calls := u.recorded()
	if len(calls) != 1 || calls[0].token != "Bearer tok-a" || calls[0].account != "acct-a" {
		t.Fatalf("calls = %+v", calls)
	}
	if !calls[0].body.IncludeSettledResponseID || len(calls[0].body.Threads) != 1 ||
		calls[0].body.Threads[0].ThreadID != "th-1" || calls[0].body.Threads[0].TurnIDs[0] != "turn-1" {
		t.Fatalf("body = %+v", calls[0].body)
	}
}

func TestQueryUnsettledFallsBackToLocalOrPending(t *testing.T) {
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		writeJSON(w, `{"threads":[{"thread_id":"th-1","turns":[
			{"turn_id":"local","estimated_usage_usd_micros":999999,"settled_response_ids":["a1"]},
			{"turn_id":"nopriced","estimated_usage_usd_micros":999999,"settled_response_ids":["b1"]},
			{"turn_id":"pending","estimated_usage_usd_micros":999999,"settled_response_ids":[]},
			{"turn_id":"nullamt","estimated_usage_usd_micros":null,"settled_response_ids":["c1"]},
			{"turn_id":"negamt","estimated_usage_usd_micros":-5,"settled_response_ids":["d1"]},
			{"turn_id":"nosettled","estimated_usage_usd_micros":5,"settled_response_ids":null}]}]}`)
	})
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"))
	ctx := context.Background()
	s.HandleUsage(ctx, record("auth-a", "th-1", "local", "a1", 1_000_000))
	s.HandleUsage(ctx, record("auth-a", "th-1", "local", "a2", 1_000_000))
	unknown := codexRecord("nopriced", "b1", "no-such-model", coreusage.Detail{InputTokens: 10})
	unknown.AuthID, unknown.ThreadID = "auth-a", "th-1"
	s.HandleUsage(ctx, unknown)
	stuck := codexRecord("pending", "p1", "no-such-model", coreusage.Detail{InputTokens: 10})
	stuck.AuthID, stuck.ThreadID = "auth-a", "th-1"
	s.HandleUsage(ctx, stuck)
	for turn, resp := range map[string]string{"nullamt": "c1", "negamt": "d1", "nosettled": "e1"} {
		s.HandleUsage(ctx, record("auth-a", "th-1", turn, resp, 1_000_000))
	}

	got := map[string]TurnCost{}
	for _, c := range s.Query(ctx, []string{"local", "nopriced", "pending", "nullamt", "negamt", "nosettled"}) {
		got[c.TurnID] = c
	}
	// a2 is not in the settled list, so the local table prices the turn.
	if c := got["local"]; c.Status != statusPriced || c.TotalUSD == "0.999999" || len(c.Responses) != 2 {
		t.Fatalf("local = %+v", c)
	}
	// Unknown to the local table but fully settled upstream: the upstream amount is used.
	if c := got["nopriced"]; c.Status != statusPriced || c.TotalUSD != "0.999999" {
		t.Fatalf("nopriced = %+v", c)
	}
	// Not settled upstream and not priceable locally: pending, never zero.
	if c := got["pending"]; c.Status != statusPending || c.TotalUSD != "" {
		t.Fatalf("pending = %+v", c)
	}
	for _, id := range []string{"nullamt", "negamt", "nosettled"} {
		if c := got[id]; c.Status != statusPriced || len(c.Responses) != 1 {
			t.Fatalf("%s = %+v, want local price", id, c)
		}
	}
}

func TestQueryForbiddenFallsBackAndIsCached(t *testing.T) {
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, `{"detail":"Forbidden"}`)
	})
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"))
	now := time.Now()
	s.now = func() time.Time { return now }
	s.HandleUsage(context.Background(), record("auth-a", "th-1", "turn-1", "r1", 1_000_000))

	for range 3 {
		got := s.Query(context.Background(), []string{"turn-1"})[0]
		if got.Status != statusPriced || len(got.Responses) != 1 {
			t.Fatalf("Query() = %+v, want local price", got)
		}
	}
	if hits := u.hits.Load(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 while forbidden is cached", hits)
	}

	now = now.Add(forbiddenTTL + time.Minute)
	s.Query(context.Background(), []string{"turn-1"})
	if hits := u.hits.Load(); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 after the cache expired", hits)
	}
}

func TestQueryUpstreamErrorsFallBackWithoutCaching(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusBadGateway} {
		u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) { w.WriteHeader(status) })
		s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"))
		s.HandleUsage(context.Background(), record("auth-a", "th-1", "turn-1", "r1", 1_000_000))
		for range 2 {
			if got := s.Query(context.Background(), []string{"turn-1"})[0]; got.Status != statusPriced || len(got.Responses) != 1 {
				t.Fatalf("status %d: Query() = %+v, want local price", status, got)
			}
		}
		if hits := u.hits.Load(); hits != 2 {
			t.Fatalf("status %d: upstream hits = %d, want 2 (not cached)", status, hits)
		}
	}
}

func TestQueryGroupsByAuthAndThread(t *testing.T) {
	u := newFakeUpstream(t, func(call upstreamCall, w http.ResponseWriter) {
		// Echo one settled turn per requested turn, priced by the credential that asked.
		micros := map[string]int{"Bearer tok-a": 100, "Bearer tok-b": 200}[call.token]
		out := `{"threads":[`
		for i, th := range call.body.Threads {
			if i > 0 {
				out += ","
			}
			out += `{"thread_id":"` + th.ThreadID + `","turns":[`
			for j, turn := range th.TurnIDs {
				if j > 0 {
					out += ","
				}
				out += `{"turn_id":"` + turn + `","estimated_usage_usd_micros":` + itoa(micros) + `,"settled_response_ids":["resp-` + turn + `"]}`
			}
			out += `]}`
		}
		writeJSON(w, out+`]}`)
	})
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"), codexAuth("auth-b", "tok-b", "acct-b"))
	ctx := context.Background()
	s.HandleUsage(ctx, record("auth-a", "th-1", "t1", "resp-t1", 10))
	s.HandleUsage(ctx, record("auth-a", "th-1", "t2", "resp-t2", 10))
	s.HandleUsage(ctx, record("auth-a", "th-2", "t3", "resp-t3", 10))
	s.HandleUsage(ctx, record("auth-b", "th-3", "t4", "resp-t4", 10))
	// No thread id, so it cannot be asked upstream.
	s.HandleUsage(ctx, record("auth-a", "", "t5", "resp-t5", 10))

	got := map[string]TurnCost{}
	for _, c := range s.Query(ctx, []string{"t1", "t2", "t3", "t4", "t5"}) {
		got[c.TurnID] = c
	}
	for id, want := range map[string]string{"t1": "0.000100", "t2": "0.000100", "t3": "0.000100", "t4": "0.000200"} {
		if got[id].TotalUSD != want {
			t.Errorf("%s = %+v, want %s", id, got[id], want)
		}
	}
	if got["t5"].Status != statusPriced || len(got["t5"].Responses) != 1 {
		t.Errorf("t5 = %+v, want local price", got["t5"])
	}

	calls := u.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want one per auth", len(calls))
	}
	for _, call := range calls {
		threads := map[string]int{}
		for _, th := range call.body.Threads {
			threads[th.ThreadID] = len(th.TurnIDs)
		}
		switch call.token {
		case "Bearer tok-a":
			if call.account != "acct-a" || len(threads) != 2 || threads["th-1"] != 2 || threads["th-2"] != 1 {
				t.Errorf("auth-a call = %+v", call)
			}
		case "Bearer tok-b":
			if call.account != "acct-b" || len(threads) != 1 || threads["th-3"] != 1 {
				t.Errorf("auth-b call = %+v", call)
			}
		default:
			t.Errorf("unexpected token %q", call.token)
		}
	}
}

func TestQueryMixedAuthTurnSkipsUpstream(t *testing.T) {
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		writeJSON(w, `{"threads":[]}`)
	})
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"), codexAuth("auth-b", "tok-b", "acct-b"))
	s.HandleUsage(context.Background(), record("auth-a", "th-1", "turn-1", "r1", 1_000_000))
	s.HandleUsage(context.Background(), record("auth-b", "th-1", "turn-1", "r2", 1_000_000))

	if got := s.Query(context.Background(), []string{"turn-1"})[0]; got.Status != statusPriced || len(got.Responses) != 2 {
		t.Fatalf("Query() = %+v, want local price", got)
	}
	if hits := u.hits.Load(); hits != 0 {
		t.Fatalf("upstream hits = %d, want 0", hits)
	}
}

func TestQueryUpstreamDisabledOrUnavailableUsesLocal(t *testing.T) {
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		writeJSON(w, `{"threads":[]}`)
	})
	apiKeyAuth := &cliproxyauth.Auth{ID: "auth-k", Provider: "codex", Metadata: map[string]any{"access_token": "tok"}}
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"), apiKeyAuth)
	s.HandleUsage(context.Background(), record("auth-a", "th-1", "t1", "r1", 1_000_000))
	s.HandleUsage(context.Background(), record("auth-k", "th-1", "t2", "r2", 1_000_000))
	s.HandleUsage(context.Background(), record("auth-gone", "th-1", "t3", "r3", 1_000_000))

	s.Configure(&config.Config{CodexTurnCosts: config.CodexTurnCostsConfig{Enabled: true}})
	for _, c := range s.Query(context.Background(), []string{"t1", "t2", "t3"}) {
		if c.Status != statusPriced || len(c.Responses) != 1 {
			t.Fatalf("disabled: %+v", c)
		}
	}
	if hits := u.hits.Load(); hits != 0 {
		t.Fatalf("upstream hits = %d with the switch off", hits)
	}

	s.Configure(&config.Config{CodexTurnCosts: config.CodexTurnCostsConfig{Enabled: true, UpstreamEstimates: true}})
	for _, c := range s.Query(context.Background(), []string{"t2", "t3"}) {
		if c.Status != statusPriced || len(c.Responses) != 1 {
			t.Fatalf("no account or missing auth: %+v", c)
		}
	}
	if hits := u.hits.Load(); hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 without an account id or auth", hits)
	}
}

func TestFormatMicros(t *testing.T) {
	for micros, want := range map[int64]string{
		0:          "0.000000",
		1:          "0.000001",
		465710:     "0.465710",
		1_000_000:  "1.000000",
		12_345_678: "12.345678",
	} {
		if got := formatMicros(micros); got != want {
			t.Errorf("formatMicros(%d) = %s, want %s", micros, got, want)
		}
	}
}

func itoa(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}
