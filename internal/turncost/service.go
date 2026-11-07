// Package turncost prices Codex turns from the usage the proxy already observes and
// serves them through the analytics endpoint that Codex polls.
package turncost

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	statusPending = "pending"
	statusPriced  = "priced"

	codexProvider = "codex"
)

// ResponseCost is the priced cost of one response in a turn.
type ResponseCost struct {
	ResponseID string `json:"response_id"`
	TotalUSD   string `json:"total_usd"`
}

// TurnCost is one entry of the endpoint response.
type TurnCost struct {
	TurnID          string         `json:"turn_id"`
	Status          string         `json:"status"`
	TotalUSD        string         `json:"total_usd,omitempty"`
	EventCount      *int           `json:"event_count,omitempty"`
	Responses       []ResponseCost `json:"responses,omitempty"`
	Model           string         `json:"model,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

// Service records per-turn usage and prices it on demand.
type Service struct {
	store   *Store
	prices  atomic.Pointer[PriceTable]
	enabled atomic.Bool
	// estimates is the upstream-estimates switch.
	estimates atomic.Bool
	upstream  upstreamState
	// entitlements remembers which accounts ChatGPT answers estimates for.
	entitlements *entitlements
	now          func() time.Time
}

// NewService returns a disabled service backed by store.
func NewService(store *Store) *Service {
	s := &Service{store: store, now: time.Now}
	s.entitlements = newEntitlements(func() time.Time { return s.now() })
	s.prices.Store(NewPriceTable(nil))
	return s
}

var defaultService = NewService(NewStore(DefaultTTL, DefaultMaxTurns, DefaultMaxResponsesPerTurn))

func init() {
	coreusage.RegisterPlugin(defaultService)
}

// Default returns the process-wide service fed by the usage manager.
func Default() *Service { return defaultService }

// Configure applies the codex-turn-costs settings. It is safe to call on every reload.
func (s *Service) Configure(cfg *config.Config) {
	if s == nil {
		return
	}
	if cfg == nil {
		s.enabled.Store(false)
		s.estimates.Store(false)
		return
	}
	s.prices.Store(NewPriceTable(cfg.CodexTurnCosts.Prices))
	s.enabled.Store(cfg.CodexTurnCosts.Enabled)
	s.estimates.Store(cfg.CodexTurnCosts.UpstreamEstimates)
	s.entitlements.configure(stateFilePath(cfg), cfg.CodexTurnCosts.ForbiddenTTLDuration())
}

// stateFilePath is the configured state file, else a file next to the auth dir. Empty
// disables persistence.
func stateFilePath(cfg *config.Config) string {
	if path := strings.TrimSpace(cfg.CodexTurnCosts.StateFile); path != "" {
		return path
	}
	if strings.TrimSpace(cfg.AuthDir) == "" {
		return ""
	}
	authDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil || authDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(authDir), DefaultStateFileName)
}

// Enabled reports whether the feature is on.
func (s *Service) Enabled() bool { return s != nil && s.enabled.Load() }

// HandleUsage implements the usage plugin interface.
func (s *Service) HandleUsage(_ context.Context, record coreusage.Record) {
	if !s.Enabled() || record.Failed {
		return
	}
	turnID := strings.TrimSpace(record.TurnID)
	responseID := strings.TrimSpace(record.Detail.ResponseID)
	if turnID == "" || responseID == "" || !strings.EqualFold(strings.TrimSpace(record.Provider), codexProvider) {
		return
	}
	detail := record.Detail
	cached := detail.CachedTokens
	if detail.CacheReadTokens > cached {
		cached = detail.CacheReadTokens
	}
	s.store.Add(turnID, Response{
		ID: responseID,
		Usage: Usage{
			InputTokens:  detail.InputTokens,
			CachedTokens: cached,
			OutputTokens: detail.OutputTokens,
		},
		Model:           record.ResponseModel,
		RequestedModel:  record.Model,
		ReasoningEffort: record.ReasoningEffort,
		AuthID:          strings.TrimSpace(record.AuthID),
		ThreadID:        strings.TrimSpace(record.ThreadID),
	})
}

// Query prices each requested turn, preferring ChatGPT's own estimate when it covers every
// response the proxy recorded and falling back to the local price table. A turn that is
// unknown, expired, or has a response without a known model price is reported as pending,
// which Codex retries a few times before it stops asking about that turn.
func (s *Service) Query(ctx context.Context, turnIDs []string) []TurnCost {
	prices := s.prices.Load()
	var upstream map[string]TurnCost
	var source AuthSource
	if s.estimates.Load() {
		source = s.authSource()
	}
	turns := make(map[string]Turn, len(turnIDs))
	for _, id := range turnIDs {
		if turn, ok := s.store.Get(id); ok {
			turns[id] = turn
		}
	}
	if source != nil {
		upstream = s.fetchUpstream(ctx, source, turns)
	}
	out := make([]TurnCost, 0, len(turnIDs))
	for _, id := range turnIDs {
		if cost, ok := upstream[id]; ok {
			out = append(out, cost)
			continue
		}
		out = append(out, s.queryTurn(prices, id, turns))
	}
	return out
}

func (s *Service) queryTurn(prices *PriceTable, turnID string, turns map[string]Turn) TurnCost {
	pending := TurnCost{TurnID: turnID, Status: statusPending}
	turn, ok := turns[turnID]
	if !ok || len(turn.Responses) == 0 {
		return pending
	}
	var total float64
	responses := make([]ResponseCost, 0, len(turn.Responses))
	for _, resp := range turn.Responses {
		price, ok := prices.Lookup(resp.Model)
		if !ok {
			price, ok = prices.Lookup(resp.RequestedModel)
		}
		if !ok {
			return pending
		}
		cost := price.Cost(resp.Usage)
		total += cost
		responses = append(responses, ResponseCost{ResponseID: resp.ID, TotalUSD: formatUSD(cost)})
	}
	last := turn.Responses[len(turn.Responses)-1]
	model := last.Model
	if model == "" {
		model = last.RequestedModel
	}
	count := len(responses)
	return TurnCost{
		TurnID:          turnID,
		Status:          statusPriced,
		TotalUSD:        formatUSD(total),
		EventCount:      &count,
		Responses:       responses,
		Model:           model,
		ReasoningEffort: last.ReasoningEffort,
	}
}

func formatUSD(value float64) string {
	return strconv.FormatFloat(value, 'f', 10, 64)
}
