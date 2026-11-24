package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServerModelListIsFilteredPerBoundKey(t *testing.T) {
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []proxyconfig.APIKeyEntry{
		{Key: "plain-key"},
		{Key: "personal-key", Auths: []string{"binding-personal.json"}},
		{Key: "work-key", Auths: []string{"binding-work-id"}},
		{Key: "dangling-key", Auths: []string{"missing.json"}},
	}}}
	server := newTestServerWithConfig(t, cfg)
	modelRegistry := registry.GetGlobalRegistry()
	for id, model := range map[string]string{"binding-personal-id": "binding-personal-model", "binding-work-id": "binding-work-model"} {
		modelRegistry.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model, Object: "model"}})
		t.Cleanup(func() { modelRegistry.UnregisterClient(id) })
	}
	for id, file := range map[string]string{"binding-personal-id": "binding-personal.json", "binding-work-id": "binding-work.json"} {
		if _, err := server.handlers.AuthManager.Register(auth.WithSkipPersist(context.Background()), &auth.Auth{
			ID: id, FileName: file, Provider: "codex", Status: auth.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}

	list := func(key string) map[string]bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body=%s", key, rr.Code, rr.Body.String())
		}
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, model := range body.Data {
			if id, ok := model["id"].(string); ok {
				ids[id] = true
			}
		}
		return ids
	}

	if got := list("plain-key"); !got["binding-personal-model"] || !got["binding-work-model"] {
		t.Fatalf("plain key must see every model, got %v", got)
	}
	if got := list("personal-key"); !got["binding-personal-model"] || got["binding-work-model"] {
		t.Fatalf("personal key (file name) listing = %v", got)
	}
	if got := list("work-key"); !got["binding-work-model"] || got["binding-personal-model"] {
		t.Fatalf("work key (ID) listing = %v", got)
	}
	if got := list("dangling-key"); len(got) != 0 {
		t.Fatalf("a key bound to a missing credential must list nothing, got %v", got)
	}
}
