package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func postTurnCosts(t *testing.T, server *Server, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/analytics/codex/turn-costs", strings.NewReader(`{"turn_ids":["turn-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	return rr
}

func TestCodexTurnCostsRouteRequiresAPIKey(t *testing.T) {
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	cfg.CodexTurnCosts.Enabled = true
	server := newTestServerWithConfig(t, cfg)

	if rr := postTurnCosts(t, server, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	rr := postTurnCosts(t, server, "Bearer test-key")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestCodexTurnCostsRouteDisabledReturnsNotFound(t *testing.T) {
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	server := newTestServerWithConfig(t, cfg)

	if rr := postTurnCosts(t, server, "Bearer test-key"); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}
