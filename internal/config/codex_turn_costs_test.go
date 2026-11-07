package config

import "testing"

func TestCodexTurnCostsDefaultsEnabled(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nserver:\n  port: 8317\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CodexTurnCosts.Enabled {
		t.Fatal("codex-turn-costs should default to enabled")
	}
}

func TestCodexTurnCostsLoadsV8Layout(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`config-version: 8
observability:
  codex-turn-costs:
    enabled: false
    prices:
      gpt-5.5:
        input: 5
        cached-input: 0.5
        output: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexTurnCosts.Enabled {
		t.Fatal("enabled: false was not honored")
	}
	got := cfg.CodexTurnCosts.Prices["gpt-5.5"]
	if got != (CodexTurnCostPrice{Input: 5, CachedInput: 0.5, Output: 30}) {
		t.Fatalf("prices = %+v", cfg.CodexTurnCosts.Prices)
	}
}
