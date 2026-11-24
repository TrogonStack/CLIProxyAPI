package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type reboundWebsocketAccessProvider struct {
	allowed atomic.Value
}

func (p *reboundWebsocketAccessProvider) Identifier() string { return "rebound" }

func (p *reboundWebsocketAccessProvider) Authenticate(_ context.Context, _ *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	allowed, _ := p.allowed.Load().([]string)
	return &sdkaccess.Result{Principal: "key-1", Provider: p.Identifier(), AllowedAuths: allowed}, nil
}

func TestWebsocketTurnResultExposesCurrentBindingWithoutTouchingContext(t *testing.T) {
	provider := &reboundWebsocketAccessProvider{}
	provider.allowed.Store([]string{"cred-a.json"})
	access := sdkaccess.NewManager()
	access.SetProviders([]sdkaccess.Provider{provider})
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	h.AccessManager = access

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/ping", nil)
	c.Set(sdkaccess.AllowedAuthsContextKey, []string{"cred-a.json"})
	turn := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	result, errAuth := h.authenticateResponsesWebsocketTurnResult(c, turn, []byte(`{}`), "gpt-5.5", false)
	if errAuth != nil || result == nil {
		t.Fatalf("turn auth: result=%v err=%v", result, errAuth)
	}
	if !sameAllowedAuths(result.AllowedAuths, []string{"cred-a.json"}) {
		t.Fatalf("binding = %v, want cred-a.json", result.AllowedAuths)
	}

	provider.allowed.Store([]string{"cred-b.json"})
	result, errAuth = h.authenticateResponsesWebsocketTurnResult(c, turn, []byte(`{}`), "gpt-5.5", false)
	if errAuth != nil || result == nil {
		t.Fatalf("turn auth after rebind: result=%v err=%v", result, errAuth)
	}
	if sameAllowedAuths(result.AllowedAuths, []string{"cred-a.json"}) {
		t.Fatalf("rebound frame still reports the stale binding %v", result.AllowedAuths)
	}
	if got := sdkaccess.AllowedAuthsFromGin(c); len(got) != 1 || got[0] != "cred-a.json" {
		t.Fatalf("a follow-up frame rewrote the shared gin restriction: %v", got)
	}
}

func TestSameAllowedAuths(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both unrestricted", nil, []string{}, true},
		{"same order", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different order", []string{"a", "b"}, []string{"b", "a"}, true},
		{"unrestricted vs bound", nil, []string{"a"}, false},
		{"bound vs unrestricted", []string{"a"}, nil, false},
		{"different member", []string{"a"}, []string{"b"}, false},
		{"duplicate vs distinct", []string{"a", "a"}, []string{"a", "b"}, false},
	}
	for _, tc := range cases {
		if got := sameAllowedAuths(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameAllowedAuths(%v, %v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}
