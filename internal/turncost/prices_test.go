package turncost

import (
	"math"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPriceCost(t *testing.T) {
	price := Price{Input: 1.25, CachedInput: 0.125, Output: 10}
	got := price.Cost(Usage{InputTokens: 1_000_000, CachedTokens: 400_000, OutputTokens: 100_000})
	// 600k uncached * 1.25 + 400k cached * 0.125 + 100k output * 10, per 1M.
	want := 0.75 + 0.05 + 1.0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("Cost() = %v, want %v", got, want)
	}
}

func TestPriceCostClampsCachedAboveInput(t *testing.T) {
	price := Price{Input: 1, CachedInput: 0.5, Output: 2}
	got := price.Cost(Usage{InputTokens: 1_000_000, CachedTokens: 5_000_000})
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("Cost() = %v, want 0.5", got)
	}
}

func TestPriceTableLookup(t *testing.T) {
	table := NewPriceTable(nil)
	tests := []struct {
		model string
		want  Price
		ok    bool
	}{
		{"gpt-5.1-codex", defaultPrices["gpt-5.1-codex"], true},
		{"GPT-5.1-Codex(high)", defaultPrices["gpt-5.1-codex"], true},
		{"gpt-5.1-codex-mini", defaultPrices["gpt-5.1-codex-mini"], true},
		{"gpt-5.1-2025-11-13", defaultPrices["gpt-5.1"], true},
		{"gpt-5-mini-2025-08-07", defaultPrices["gpt-5-mini"], true},
		{"gpt-5.10", Price{}, false},
		{"unknown-model", Price{}, false},
		{"", Price{}, false},
	}
	for _, tc := range tests {
		got, ok := table.Lookup(tc.model)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Lookup(%q) = %v, %v; want %v, %v", tc.model, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPriceTableOverrides(t *testing.T) {
	table := NewPriceTable(map[string]config.CodexTurnCostPrice{
		"GPT-5.5":    {Input: 1, CachedInput: 0.1, Output: 2},
		"my-private": {Input: 3, CachedInput: 0.3, Output: 4},
	})
	if got, ok := table.Lookup("gpt-5.5"); !ok || got != (Price{Input: 1, CachedInput: 0.1, Output: 2}) {
		t.Fatalf("override not applied: %v, %v", got, ok)
	}
	if got, ok := table.Lookup("my-private"); !ok || got.Output != 4 {
		t.Fatalf("added model not found: %v, %v", got, ok)
	}
	if _, ok := table.Lookup("gpt-5.4"); !ok {
		t.Fatal("built-in price lost after override")
	}
}
