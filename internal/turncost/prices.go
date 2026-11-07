package turncost

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Price holds USD prices per 1M tokens for one model.
type Price struct {
	Input       float64
	CachedInput float64
	Output      float64
}

// Usage holds the billable token counts of one response. InputTokens includes
// CachedTokens and OutputTokens includes reasoning tokens, matching the OpenAI
// Responses usage object.
type Usage struct {
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
}

// Cost returns the USD cost of usage at this price.
func (p Price) Cost(u Usage) float64 {
	cached := u.CachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > u.InputTokens {
		cached = u.InputTokens
	}
	uncached := u.InputTokens - cached
	if uncached < 0 {
		uncached = 0
	}
	output := u.OutputTokens
	if output < 0 {
		output = 0
	}
	return (float64(uncached)*p.Input + float64(cached)*p.CachedInput + float64(output)*p.Output) / 1e6
}

// defaultPrices lists standard-tier USD prices per 1M tokens. Entries match a model
// name exactly or as a prefix that ends at a "-" boundary, so dated snapshots such as
// "gpt-5.1-2025-11-13" resolve to "gpt-5.1". Operators can add or override entries
// with codex-turn-costs.prices.
var defaultPrices = map[string]Price{
	"gpt-5":              {Input: 1.25, CachedInput: 0.125, Output: 10},
	"gpt-5-codex":        {Input: 1.25, CachedInput: 0.125, Output: 10},
	"gpt-5-mini":         {Input: 0.25, CachedInput: 0.025, Output: 2},
	"gpt-5-nano":         {Input: 0.05, CachedInput: 0.005, Output: 0.40},
	"gpt-5.1":            {Input: 1.25, CachedInput: 0.125, Output: 10},
	"gpt-5.1-codex":      {Input: 1.25, CachedInput: 0.125, Output: 10},
	"gpt-5.1-codex-max":  {Input: 1.25, CachedInput: 0.125, Output: 10},
	"gpt-5.1-codex-mini": {Input: 0.25, CachedInput: 0.025, Output: 2},
	"gpt-5.2":            {Input: 1.75, CachedInput: 0.175, Output: 14},
	"gpt-5.2-codex":      {Input: 1.75, CachedInput: 0.175, Output: 14},
	"gpt-5.3-codex":      {Input: 1.75, CachedInput: 0.175, Output: 14},
	"gpt-5.4":            {Input: 2.50, CachedInput: 0.25, Output: 15},
	"gpt-5.4-mini":       {Input: 0.75, CachedInput: 0.075, Output: 4.50},
	"gpt-5.4-nano":       {Input: 0.20, CachedInput: 0.02, Output: 1.25},
	"gpt-5.5":            {Input: 5, CachedInput: 0.50, Output: 30},
	"gpt-5.5-pro":        {Input: 30, CachedInput: 30, Output: 180},
	"gpt-5.5-cyber":      {Input: 12.50, CachedInput: 1.25, Output: 75},
	"gpt-5.6-sol":        {Input: 4, CachedInput: 0.40, Output: 20},
	"gpt-5.6-terra":      {Input: 2, CachedInput: 0.20, Output: 12},
	"gpt-5.6-luna":       {Input: 0.20, CachedInput: 0.02, Output: 1.20},
	"gpt-5.6-cyber":      {Input: 12.50, CachedInput: 1.25, Output: 75},
	"gpt-6-sol":          {Input: 2, CachedInput: 0.20, Output: 10},
	"gpt-6-luna":         {Input: 0.10, CachedInput: 0.01, Output: 0.50},
	"gpt-6-astra":        {Input: 10, CachedInput: 1, Output: 50},
	"gpt-6-astra-law":    {Input: 10, CachedInput: 1, Output: 50},
	"gpt-6-rosalind":     {Input: 15, CachedInput: 1.50, Output: 75},
	"gpt-6-cyber":        {Input: 20, CachedInput: 2, Output: 100},
	"gpt-6.1-sol":        {Input: 2, CachedInput: 0.10, Output: 10},
	"gpt-6.1-terra":      {Input: 1, CachedInput: 0.05, Output: 5},
	"codex-mini-latest":  {Input: 1.50, CachedInput: 0.375, Output: 6},
	"gpt-4.1":            {Input: 2, CachedInput: 0.50, Output: 8},
	"gpt-4.1-mini":       {Input: 0.40, CachedInput: 0.10, Output: 1.60},
	"o3":                 {Input: 2, CachedInput: 0.50, Output: 8},
	"o4-mini":            {Input: 1.10, CachedInput: 0.275, Output: 4.40},
}

// PriceTable resolves model names to prices.
type PriceTable struct {
	prices map[string]Price
}

// NewPriceTable merges the built-in prices with operator overrides.
func NewPriceTable(overrides map[string]config.CodexTurnCostPrice) *PriceTable {
	prices := make(map[string]Price, len(defaultPrices)+len(overrides))
	for model, price := range defaultPrices {
		prices[model] = price
	}
	for model, price := range overrides {
		model = normalizeModel(model)
		if model == "" {
			continue
		}
		prices[model] = Price{Input: price.Input, CachedInput: price.CachedInput, Output: price.Output}
	}
	return &PriceTable{prices: prices}
}

// Lookup returns the price for model, preferring an exact match and then the longest
// "-" delimited prefix.
func (t *PriceTable) Lookup(model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	model = normalizeModel(model)
	for model != "" {
		if price, ok := t.prices[model]; ok {
			return price, true
		}
		idx := strings.LastIndexByte(model, '-')
		if idx < 0 {
			break
		}
		model = model[:idx]
	}
	return Price{}, false
}

// normalizeModel lower-cases the name and drops a thinking suffix such as "(high)".
func normalizeModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if idx := strings.IndexByte(model, '('); idx >= 0 {
		model = strings.TrimSpace(model[:idx])
	}
	return model
}
