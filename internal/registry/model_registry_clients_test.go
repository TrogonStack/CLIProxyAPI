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
