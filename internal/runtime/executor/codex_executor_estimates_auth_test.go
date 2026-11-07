package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexPrepareRequestUsesOAuthTokenForUsageEstimates(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"use_siwc":     true,
			"access_token": "oauth-token",
			"siwc":         map[string]any{"access_token": "siwc-token"},
		},
	}
	for url, want := range map[string]string{
		"https://chatgpt.com/backend-api/wham/usage/thread-estimates/query": "Bearer oauth-token",
		"https://chatgpt.com/backend-api/codex/responses":                   "Bearer siwc-token",
	} {
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := (&CodexExecutor{}).PrepareRequest(req, auth); err != nil {
			t.Fatalf("PrepareRequest(%s): %v", url, err)
		}
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("%s Authorization = %q, want %q", url, got, want)
		}
	}
}
