package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func registerBoundModelFixture(t *testing.T) *auth.Manager {
	t.Helper()
	manager := auth.NewManager(nil, nil, nil)
	modelRegistry := registry.GetGlobalRegistry()
	for id, model := range map[string]string{"bound-models-personal": "bound-personal-model", "bound-models-work": "bound-work-model"} {
		modelRegistry.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { modelRegistry.UnregisterClient(id) })
		if _, err := manager.Register(auth.WithSkipPersist(context.Background()), &auth.Auth{
			ID: id, FileName: id + ".json", Provider: "codex", Status: auth.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return manager
}

func listModelIDs(models []map[string]any) map[string]bool {
	ids := map[string]bool{}
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids
}

func ginWithAllowed(allowed []string) *gin.Context {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if allowed != nil {
		c.Set(sdkaccess.AllowedAuthsContextKey, allowed)
	}
	return c
}

func TestModelsForRequestListsOnlyBoundCredentialModels(t *testing.T) {
	manager := registerBoundModelFixture(t)
	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))

	bound := listModelIDs(handler.ModelsForRequest(ginWithAllowed([]string{"bound-models-personal.json"})))
	if !bound["bound-personal-model"] || bound["bound-work-model"] || len(bound) != 1 {
		t.Fatalf("bound listing = %v, want only bound-personal-model", bound)
	}

	byID := listModelIDs(handler.ModelsForRequest(ginWithAllowed([]string{"bound-models-work"})))
	if !byID["bound-work-model"] || byID["bound-personal-model"] {
		t.Fatalf("ID-bound listing = %v", byID)
	}

	unbound := listModelIDs(handler.ModelsForRequest(ginWithAllowed(nil)))
	if !unbound["bound-personal-model"] || !unbound["bound-work-model"] {
		t.Fatalf("unbound listing = %v, want both models", unbound)
	}

	none := handler.ModelsForRequest(ginWithAllowed([]string{"does-not-exist.json"}))
	if len(none) != 0 {
		t.Fatalf("binding to a missing credential must list nothing, got %v", none)
	}
}

func TestOpenAIModelsEndpointFiltersPerKey(t *testing.T) {
	manager := registerBoundModelFixture(t)
	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(sdkaccess.AllowedAuthsContextKey, []string{"bound-models-personal.json"})
	handler.OpenAIModels(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ids := listModelIDs(body.Data)
	if !ids["bound-personal-model"] || ids["bound-work-model"] || len(ids) != 1 {
		t.Fatalf("endpoint listing = %v, want only bound-personal-model", ids)
	}
}

func TestModelsForRequestHomeModeFailsClosed(t *testing.T) {
	manager := registerBoundModelFixture(t)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	if got := handler.ModelsForRequest(ginWithAllowed([]string{"bound-models-personal.json"})); len(got) != 0 {
		t.Fatalf("bound key in home mode listed %v", got)
	}
}
