package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/json"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// recordingCodexExecutor records which credential serves each upstream realtime call.
type recordingCodexExecutor struct {
	mu       sync.Mutex
	selected []string
}

func (*recordingCodexExecutor) Identifier() string { return "codex" }
func (*recordingCodexExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (*recordingCodexExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}
func (*recordingCodexExecutor) Refresh(_ context.Context, credential *auth.Auth) (*auth.Auth, error) {
	return credential, nil
}
func (*recordingCodexExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (*recordingCodexExecutor) PrepareRequest(req *http.Request, credential *auth.Auth) error {
	token, _ := credential.Metadata["access_token"].(string)
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}
func (e *recordingCodexExecutor) HttpRequest(_ context.Context, credential *auth.Auth, req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	e.mu.Lock()
	e.selected = append(e.selected, credential.ID)
	e.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     http.Header{"Content-Type": []string{"application/sdp"}, "Location": []string{"/v1/realtime/calls/call-binding"}},
		Body:       io.NopCloser(strings.NewReader("v=0\r\n")),
	}, nil
}

func (e *recordingCodexExecutor) servedBy() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.selected...)
}

// TestRealtimeClientSecretKeepsTheIssuerBinding mints realtime client secrets through
// the real routes and middleware, then opens calls with them. A secret minted by a key
// bound to credential A must only ever be served by A, even though B is registered too.
func TestRealtimeClientSecretKeepsTheIssuerBinding(t *testing.T) {
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []proxyconfig.APIKeyEntry{
		{Key: "plain-key"},
		{Key: "key-a", Auths: []string{"rt-a.json"}},
		{Key: "key-nothing", Auths: []string{"rt-missing.json"}},
	}}}
	server := newTestServerWithConfig(t, cfg)
	executor := &recordingCodexExecutor{}
	manager := server.handlers.AuthManager
	manager.RegisterExecutor(executor)
	for id, file := range map[string]string{"rt-a": "rt-a.json", "rt-b": "rt-b.json"} {
		if _, err := manager.Register(auth.WithSkipPersist(context.Background()), &auth.Auth{
			ID: id, FileName: file, Provider: "codex", Status: auth.StatusActive,
			Metadata: map[string]any{"access_token": "token-" + id},
		}); err != nil {
			t.Fatal(err)
		}
	}

	mint := func(key string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/realtime/client_secrets", strings.NewReader(`{"session":{"type":"realtime","model":"gpt-realtime"}}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("mint with %s: status = %d body=%s", key, rr.Code, rr.Body.String())
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Value == "" {
			t.Fatalf("mint with %s: body=%s err=%v", key, rr.Body.String(), err)
		}
		return body.Value
	}
	call := func(secret string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", strings.NewReader("v=0\r\n"))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/sdp")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		return rr.Code
	}

	boundSecret := mint("key-a")
	for i := 0; i < 8; i++ {
		if code := call(boundSecret); code != http.StatusCreated {
			t.Fatalf("bound call %d: status = %d", i, code)
		}
	}
	for _, id := range executor.servedBy() {
		if id != "rt-a" {
			t.Fatalf("a secret minted by a key bound to rt-a was served by %q", id)
		}
	}
	if got := len(executor.servedBy()); got != 8 {
		t.Fatalf("served calls = %d, want 8", got)
	}

	// A key bound to nothing mints a secret that can never be served.
	before := len(executor.servedBy())
	nothingSecret := mint("key-nothing")
	if code := call(nothingSecret); code == http.StatusCreated {
		t.Fatalf("a secret minted by a key bound to a missing credential was served")
	}
	if got := len(executor.servedBy()); got != before {
		t.Fatalf("a key bound to nothing reached an upstream credential")
	}

	// Control: an unrestricted key's secret still rotates over both credentials, so the
	// bound result above comes from the restriction and not from a missing credential.
	plainSecret := mint("plain-key")
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		if code := call(plainSecret); code != http.StatusCreated {
			t.Fatalf("plain call %d: status = %d", i, code)
		}
	}
	for _, id := range executor.servedBy()[before:] {
		seen[id] = true
	}
	if !seen["rt-a"] || !seen["rt-b"] {
		t.Fatalf("unrestricted secret served by %v, want both credentials", seen)
	}
}
