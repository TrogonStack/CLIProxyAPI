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
	if !cfg.CodexTurnCosts.UpstreamEstimates {
		t.Fatal("upstream-estimates should default to enabled")
	}
}

func TestCodexTurnCostsLoadsV8Layout(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`config-version: 8
observability:
  codex-turn-costs:
    enabled: false
    upstream-estimates: false
    forbidden-ttl: 24h
    state-file: state.json
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
	if cfg.CodexTurnCosts.UpstreamEstimates {
		t.Fatal("upstream-estimates: false was not honored")
	}
	if cfg.CodexTurnCosts.ForbiddenTTLDuration().Hours() != 24 || cfg.CodexTurnCosts.StateFile != "state.json" {
		t.Fatalf("forbidden-ttl/state-file = %+v", cfg.CodexTurnCosts)
	}
	got := cfg.CodexTurnCosts.Prices["gpt-5.5"]
	if got != (CodexTurnCostPrice{Input: 5, CachedInput: 0.5, Output: 30}) {
		t.Fatalf("prices = %+v", cfg.CodexTurnCosts.Prices)
	}
}
