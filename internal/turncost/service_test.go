package turncost

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func newTestService(enabled bool, prices map[string]config.CodexTurnCostPrice) *Service {
	s := NewService(NewStore(time.Hour, 100, 100))
	s.Configure(&config.Config{CodexTurnCosts: config.CodexTurnCostsConfig{Enabled: enabled, Prices: prices}})
	return s
}

func codexRecord(turnID, responseID, model string, detail coreusage.Detail) coreusage.Record {
	detail.ResponseID = responseID
	return coreusage.Record{
		Provider:        "codex",
		Model:           model,
		TurnID:          turnID,
		ReasoningEffort: "high",
		Detail:          detail,
	}
}

func TestHandleUsageIgnoresIrrelevantRecords(t *testing.T) {
	s := newTestService(true, nil)
	detail := coreusage.Detail{InputTokens: 10}

	failed := codexRecord("t", "r", "gpt-5", detail)
	failed.Failed = true
	other := codexRecord("t", "r", "gpt-5", detail)
	other.Provider = "claude"
	for _, rec := range []coreusage.Record{
		failed,
		other,
		codexRecord("", "r", "gpt-5", detail),
		codexRecord("t", "", "gpt-5", detail),
	} {
		s.HandleUsage(context.Background(), rec)
	}
	if s.store.Len() != 0 {
		t.Fatalf("store has %d turns, want 0", s.store.Len())
	}

	disabled := newTestService(false, nil)
	disabled.HandleUsage(context.Background(), codexRecord("t", "r", "gpt-5", detail))
	if disabled.store.Len() != 0 {
		t.Fatal("disabled service must not record")
	}
}

func TestQueryPricesTurn(t *testing.T) {
	s := newTestService(true, nil)
	s.HandleUsage(context.Background(), codexRecord("turn-1", "r1", "gpt-5.1-codex", coreusage.Detail{
		InputTokens: 1_000_000, CachedTokens: 400_000, OutputTokens: 100_000, ReasoningTokens: 50_000,
	}))
	s.HandleUsage(context.Background(), codexRecord("turn-1", "r2", "gpt-5.1-codex", coreusage.Detail{
		InputTokens: 1_000_000, OutputTokens: 0,
	}))

	got := s.Query(context.Background(), []string{"turn-1"})
	if len(got) != 1 || got[0].Status != "priced" {
		t.Fatalf("Query() = %+v", got)
	}
	turn := got[0]
	if turn.TotalUSD != "3.0500000000" {
		t.Fatalf("TotalUSD = %s, want 3.0500000000", turn.TotalUSD)
	}
	if turn.EventCount == nil || *turn.EventCount != 2 || len(turn.Responses) != 2 {
		t.Fatalf("responses = %+v, count = %v", turn.Responses, turn.EventCount)
	}
	if turn.Responses[0].TotalUSD != "1.8000000000" || turn.Responses[1].TotalUSD != "1.2500000000" {
		t.Fatalf("response costs = %+v", turn.Responses)
	}
	if turn.Model != "gpt-5.1-codex" || turn.ReasoningEffort != "high" {
		t.Fatalf("model = %q effort = %q", turn.Model, turn.ReasoningEffort)
	}
}

func TestQueryPrefersResponseModelThenFallsBack(t *testing.T) {
	s := newTestService(true, nil)
	rec := codexRecord("turn-1", "r1", "gpt-5.1-codex", coreusage.Detail{InputTokens: 1_000_000})
	rec.ResponseModel = "not-priced"
	s.HandleUsage(context.Background(), rec)
	got := s.Query(context.Background(), []string{"turn-1"})
	if got[0].Status != "priced" || got[0].TotalUSD != "1.2500000000" {
		t.Fatalf("Query() = %+v", got[0])
	}
}

func TestQueryPendingCases(t *testing.T) {
	s := newTestService(true, nil)
	s.HandleUsage(context.Background(), codexRecord("unpriced", "r1", "mystery-model", coreusage.Detail{InputTokens: 1}))
	s.HandleUsage(context.Background(), codexRecord("mixed", "r1", "gpt-5", coreusage.Detail{InputTokens: 1}))
	s.HandleUsage(context.Background(), codexRecord("mixed", "r2", "mystery-model", coreusage.Detail{InputTokens: 1}))

	for _, got := range s.Query(context.Background(), []string{"unknown", "unpriced", "mixed"}) {
		if got.Status != "pending" || got.TotalUSD != "" || got.EventCount != nil || got.Responses != nil {
			t.Errorf("turn %s = %+v, want bare pending", got.TurnID, got)
		}
	}
}

func TestQueryUsesConfiguredPrices(t *testing.T) {
	s := newTestService(true, map[string]config.CodexTurnCostPrice{
		"mystery-model": {Input: 2, CachedInput: 1, Output: 4},
	})
	s.HandleUsage(context.Background(), codexRecord("t", "r", "mystery-model", coreusage.Detail{InputTokens: 1_000_000}))
	if got := s.Query(context.Background(), []string{"t"})[0]; got.Status != "priced" || got.TotalUSD != "2.0000000000" {
		t.Fatalf("Query() = %+v", got)
	}
}

func postTurnCosts(s *Service, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/analytics/codex/turn-costs", s.Handler)
	req := httptest.NewRequest(http.MethodPost, "/v1/analytics/codex/turn-costs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestHandlerResponseShape(t *testing.T) {
	s := newTestService(true, nil)
	s.HandleUsage(context.Background(), codexRecord("turn-1", "r1", "gpt-5", coreusage.Detail{InputTokens: 1_000_000}))

	rec := postTurnCosts(s, `{"turn_ids":["turn-1","turn-2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Turns) != 2 {
		t.Fatalf("turns = %+v", raw.Turns)
	}
	priced, pending := raw.Turns[0], raw.Turns[1]
	if priced["status"] != "priced" || priced["total_usd"] != "1.2500000000" || priced["event_count"] != float64(1) {
		t.Fatalf("priced = %+v", priced)
	}
	responses, _ := priced["responses"].([]any)
	if len(responses) != 1 || responses[0].(map[string]any)["response_id"] != "r1" {
		t.Fatalf("responses = %+v", priced["responses"])
	}
	if len(pending) != 2 || pending["turn_id"] != "turn-2" || pending["status"] != "pending" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestHandlerDisabledReturnsNotFound(t *testing.T) {
	if rec := postTurnCosts(newTestService(false, nil), `{"turn_ids":["a"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandlerRejectsBadBody(t *testing.T) {
	if rec := postTurnCosts(newTestService(true, nil), `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlerCapsQueriedTurns(t *testing.T) {
	ids := make([]string, maxQueryTurns+50)
	for i := range ids {
		ids[i] = "turn"
	}
	body, _ := json.Marshal(queryRequest{TurnIDs: ids})
	rec := postTurnCosts(newTestService(true, nil), string(body))
	var resp queryResponse
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Turns) != maxQueryTurns {
		t.Fatalf("len(turns) = %d, want %d", len(resp.Turns), maxQueryTurns)
	}
}
