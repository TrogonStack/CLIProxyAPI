package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// AvailableModelsForRequest lists the models for handlerType that the request's client
// key may use. Unrestricted keys see the global list; a key bound to credentials only
// sees models served by those credentials. Home mode cannot enforce a binding, so a
// bound key sees no models there, matching the dispatch refusal.
func (h *BaseAPIHandler) AvailableModelsForRequest(c *gin.Context, handlerType string) []map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	refs := allowedAuthsFromGin(c)
	if len(refs) == 0 {
		return modelRegistry.GetAvailableModels(handlerType)
	}
	if h == nil || h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		return []map[string]any{}
	}
	return modelRegistry.GetAvailableModelsForClients(handlerType, h.AuthManager.AllowedAuthIDs(refs))
}
