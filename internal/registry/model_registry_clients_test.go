package registry

import "testing"

func modelIDsOf(models []map[string]any) map[string]bool {
	ids := map[string]bool{}
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids
}

func TestGetAvailableModelsForClientsLimitsToGivenClients(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("personal", "codex", []*ModelInfo{{ID: "shared", Created: 1}, {ID: "personal-only", Created: 1}})
	r.RegisterClient("work", "codex", []*ModelInfo{{ID: "shared", Created: 1}, {ID: "work-only", Created: 1}})

	got := modelIDsOf(r.GetAvailableModelsForClients("openai", map[string]struct{}{"personal": {}}))
	if !got["shared"] || !got["personal-only"] || got["work-only"] || len(got) != 2 {
		t.Fatalf("personal listing = %v", got)
	}
	if got = modelIDsOf(r.GetAvailableModelsForClients("openai", map[string]struct{}{"personal": {}, "work": {}})); len(got) != 3 {
		t.Fatalf("combined listing = %v", got)
	}
	if got = modelIDsOf(r.GetAvailableModelsForClients("openai", nil)); len(got) != 0 {
		t.Fatalf("empty client set must list nothing, got %v", got)
	}
	if all := modelIDsOf(r.GetAvailableModels("openai")); len(all) != 3 {
		t.Fatalf("global listing changed: %v", all)
	}
}

func TestGetAvailableModelsForClientsScopesSuspensionAndQuotaToAllowedClients(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("personal", "codex", []*ModelInfo{{ID: "shared", Created: 1}})
	r.RegisterClient("work", "codex", []*ModelInfo{{ID: "shared", Created: 1}})

	// The unrelated client being suspended must not hide the model from the allowed client.
	r.SuspendClientModel("work", "shared", "test")
	if got := modelIDsOf(r.GetAvailableModelsForClients("openai", map[string]struct{}{"personal": {}})); !got["shared"] {
		t.Fatalf("suspension of an unrelated client hid the model: %v", got)
	}

	// The allowed client being suspended hides it even though another client still serves it.
	r.ResumeClientModel("work", "shared")
	r.SuspendClientModel("personal", "shared", "test")
	if got := modelIDsOf(r.GetAvailableModelsForClients("openai", map[string]struct{}{"personal": {}})); got["shared"] {
		t.Fatalf("suspended allowed client still lists the model: %v", got)
	}
	if got := modelIDsOf(r.GetAvailableModels("openai")); !got["shared"] {
		t.Fatalf("global listing should still include the model: %v", got)
	}
}

func TestScopedModelInfoComesFromSelectedClientsOnly(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("a-personal", "codex", []*ModelInfo{{ID: "shared", Created: 1, ContextLength: 100}})
	r.RegisterClient("b-work", "claude", []*ModelInfo{{ID: "shared", Created: 1, ContextLength: 900, SupportsWebSearch: true}})

	r.mutex.RLock()
	defer r.mutex.RUnlock()
	info, byProvider := r.scopedModelInfoLocked("shared", map[string]struct{}{"a-personal": {}})
	if info == nil || info.ContextLength != 100 || info.SupportsWebSearch {
		t.Fatalf("scoped info = %+v, want the personal credential's metadata only", info)
	}
	if len(byProvider) != 1 || byProvider["codex"] == nil || byProvider["claude"] != nil {
		t.Fatalf("scoped InfoByProvider = %v, want codex only", byProvider)
	}
	info, byProvider = r.scopedModelInfoLocked("shared", map[string]struct{}{"a-personal": {}, "b-work": {}})
	if info.ContextLength != 100 || !info.SupportsWebSearch || len(byProvider) != 2 {
		t.Fatalf("combined scoped info = %+v providers=%v, want lowest client's info and OR of web search", info, byProvider)
	}
	// The scoped registrations used for listings carry the scoped metadata too.
	for _, reg := range r.availableRegistrationsForClientsLocked(map[string]struct{}{"a-personal": {}}, r.now()) {
		if reg.Info.ContextLength != 100 || reg.InfoByProvider["claude"] != nil {
			t.Fatalf("scoped registration leaked global metadata: %+v %v", reg.Info, reg.InfoByProvider)
		}
	}
}
