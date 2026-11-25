package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const passthroughUpstreamStream = "event: response.created\n" +
	`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}` + "\n\n" +
	"event: response.in_progress\n" +
	`data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_1","status":"in_progress"}}` + "\n\n" +
	"event: error\n" +
	`data: {"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"sequence_number":3}` + "\n\n"

// passthroughExecutor emits one chunk per upstream event, as the Codex executor does in pass-through mode.
type passthroughExecutor struct{ mark bool }

func (*passthroughExecutor) Identifier() string { return "passthrough-responses" }
func (*passthroughExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}
func (p *passthroughExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 16)
	for _, frame := range strings.SplitAfter(passthroughUpstreamStream, "\n\n") {
		if frame != "" {
			chunks <- coreexecutor.StreamChunk{Payload: []byte(frame)}
		}
	}
	chunks <- coreexecutor.StreamChunk{Err: errors.New(`{"error":{"code":"server_is_overloaded"}}`)}
	close(chunks)
	if p.mark {
		coreexecutor.MarkStreamPassthrough(ctx)
	}
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}
func (*passthroughExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*passthroughExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}
func (*passthroughExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func serveResponsesStream(t *testing.T, mark bool, userAgent string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	executor := &passthroughExecutor{mark: mark}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "passthrough-auth-" + t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "passthrough-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"passthrough-model","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestResponsesPassthroughForwardsUpstreamBytesVerbatim(t *testing.T) {
	for _, ua := range []string{"codex_cli_rs/0.1", "OpenAI/Python 1.0"} {
		got := serveResponsesStream(t, true, ua)
		if got != passthroughUpstreamStream {
			t.Fatalf("ua %q: stream was not forwarded verbatim.\ngot:  %q\nwant: %q", ua, got, passthroughUpstreamStream)
		}
	}
}

func TestResponsesPassthroughOffKeepsReshapedError(t *testing.T) {
	got := serveResponsesStream(t, false, "OpenAI/Python 1.0")
	if got == passthroughUpstreamStream {
		t.Fatalf("stream was forwarded verbatim without the executor marking it: %q", got)
	}
	if !strings.Contains(got, "event: response.failed") {
		t.Fatalf("default path should still emit response.failed: %q", got)
	}
}
