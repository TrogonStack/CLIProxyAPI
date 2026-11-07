package turncost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultUpstreamBaseURL = "https://chatgpt.com/backend-api"
	estimatesPath          = "/wham/usage/thread-estimates/query"

	upstreamTimeout   = 10 * time.Second
	forbiddenTTL      = time.Hour
	maxForbiddenAuths = 1024
	maxUpstreamBody   = 4 << 20

	upstreamUserAgent  = "codex-cli"
	upstreamOriginator = "codex_cli_rs"
)

// AuthSource resolves the credential that served a turn and sends requests with it, so
// refreshed tokens, proxies, and per-credential settings are the ones the executor uses.
// *auth.Manager implements it.
type AuthSource interface {
	GetByID(id string) (*cliproxyauth.Auth, bool)
	HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error)
}

type estimatesRequest struct {
	Threads                  []estimatesThread `json:"threads"`
	IncludeSettledResponseID bool              `json:"include_settled_response_ids"`
}

type estimatesThread struct {
	ThreadID string   `json:"thread_id"`
	TurnIDs  []string `json:"turn_ids"`
}

type estimatesResponse struct {
	Threads []struct {
		ThreadID string `json:"thread_id"`
		Turns    []struct {
			TurnID             string   `json:"turn_id"`
			Model              *string  `json:"model"`
			EstimatedUSDMicros *int64   `json:"estimated_usage_usd_micros"`
			SettledResponseIDs []string `json:"settled_response_ids"`
		} `json:"turns"`
	} `json:"threads"`
}

// upstreamState holds what the ChatGPT pass-through needs besides the config switch.
type upstreamState struct {
	mu        sync.Mutex
	source    AuthSource
	baseURL   string
	forbidden map[string]time.Time
}

// SetAuthSource sets where credentials come from. Without one, only local prices are used.
func (s *Service) SetAuthSource(source AuthSource) {
	if s == nil {
		return
	}
	s.upstream.mu.Lock()
	s.upstream.source = source
	s.upstream.mu.Unlock()
}

func (s *Service) authSource() AuthSource {
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()
	return s.upstream.source
}

func (s *Service) upstreamBaseURL() string {
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()
	if s.upstream.baseURL != "" {
		return s.upstream.baseURL
	}
	return defaultUpstreamBaseURL
}

func (s *Service) isForbidden(authID string) bool {
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()
	until, ok := s.upstream.forbidden[authID]
	if !ok {
		return false
	}
	if !s.now().Before(until) {
		delete(s.upstream.forbidden, authID)
		return false
	}
	return true
}

// markForbidden remembers that ChatGPT refuses estimates for the credential, which is the
// case for plans without workspace usage access.
func (s *Service) markForbidden(authID string) {
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()
	now := s.now()
	if s.upstream.forbidden == nil {
		s.upstream.forbidden = make(map[string]time.Time)
	}
	if _, ok := s.upstream.forbidden[authID]; !ok && len(s.upstream.forbidden) >= maxForbiddenAuths {
		for id, until := range s.upstream.forbidden {
			if !now.Before(until) {
				delete(s.upstream.forbidden, id)
			}
		}
		if len(s.upstream.forbidden) >= maxForbiddenAuths {
			return
		}
	}
	s.upstream.forbidden[authID] = now.Add(forbiddenTTL)
}

// turnsByAuth groups the turns that can be priced upstream by credential, then thread.
func turnsByAuth(turns map[string]Turn) map[string]map[string][]string {
	grouped := make(map[string]map[string][]string)
	for id, turn := range turns {
		if turn.AuthID == "" || turn.ThreadID == "" || turn.MixedAuth || len(turn.Responses) == 0 {
			continue
		}
		threads := grouped[turn.AuthID]
		if threads == nil {
			threads = make(map[string][]string)
			grouped[turn.AuthID] = threads
		}
		threads[turn.ThreadID] = append(threads[turn.ThreadID], id)
	}
	return grouped
}

// fetchUpstream asks ChatGPT for the estimates of every turn the proxy recorded, one
// request per credential. Turns it cannot price are simply absent from the result.
func (s *Service) fetchUpstream(ctx context.Context, source AuthSource, turns map[string]Turn) map[string]TurnCost {
	grouped := turnsByAuth(turns)
	if len(grouped) == 0 {
		return nil
	}
	var (
		mu  sync.Mutex
		out = make(map[string]TurnCost)
		wg  sync.WaitGroup
	)
	for authID, threads := range grouped {
		wg.Add(1)
		go func() {
			defer wg.Done()
			priced := s.fetchForAuth(ctx, source, authID, threads, turns)
			mu.Lock()
			defer mu.Unlock()
			for id, cost := range priced {
				out[id] = cost
			}
		}()
	}
	wg.Wait()
	return out
}

func (s *Service) fetchForAuth(ctx context.Context, source AuthSource, authID string, threads map[string][]string, turns map[string]Turn) map[string]TurnCost {
	if s.isForbidden(authID) {
		return nil
	}
	auth, ok := source.GetByID(authID)
	if !ok || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), codexProvider) {
		return nil
	}
	accountID, _ := auth.Metadata["account_id"].(string)
	if strings.TrimSpace(accountID) == "" {
		return nil
	}

	payload := estimatesRequest{IncludeSettledResponseID: true}
	for threadID, turnIDs := range threads {
		payload.Threads = append(payload.Threads, estimatesThread{ThreadID: threadID, TurnIDs: turnIDs})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.upstreamBaseURL(), "/")+estimatesPath, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", upstreamUserAgent)
	req.Header.Set("Originator", upstreamOriginator)
	req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))

	resp, err := source.HttpRequest(ctx, auth, req)
	if err != nil {
		log.Debugf("codex turn costs: upstream estimate request failed: %v", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusForbidden:
		s.markForbidden(authID)
		return nil
	case resp.StatusCode != http.StatusOK:
		log.Debugf("codex turn costs: upstream estimates returned status %d", resp.StatusCode)
		return nil
	}
	var decoded estimatesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&decoded); err != nil {
		log.Debugf("codex turn costs: decode upstream estimates: %v", err)
		return nil
	}

	out := make(map[string]TurnCost)
	for _, thread := range decoded.Threads {
		requested := threads[thread.ThreadID]
		for _, est := range thread.Turns {
			if !contains(requested, est.TurnID) || est.EstimatedUSDMicros == nil || *est.EstimatedUSDMicros < 0 {
				continue
			}
			// A projection can appear before every response settles, so every response the
			// proxy recorded for the turn must be covered. A missing amount is never zero.
			local := turns[est.TurnID]
			if !settles(local, est.SettledResponseIDs) {
				continue
			}
			count := len(est.SettledResponseIDs)
			last := local.Responses[len(local.Responses)-1]
			model := last.Model
			if model == "" {
				model = last.RequestedModel
			}
			if est.Model != nil && strings.TrimSpace(*est.Model) != "" {
				model = *est.Model
			}
			out[est.TurnID] = TurnCost{
				TurnID:          est.TurnID,
				Status:          statusPriced,
				TotalUSD:        formatMicros(*est.EstimatedUSDMicros),
				EventCount:      &count,
				Model:           model,
				ReasoningEffort: last.ReasoningEffort,
			}
		}
	}
	return out
}

func settles(turn Turn, settled []string) bool {
	if len(turn.Responses) == 0 || len(settled) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(settled))
	for _, id := range settled {
		have[id] = struct{}{}
	}
	for _, resp := range turn.Responses {
		if _, ok := have[resp.ID]; !ok {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// formatMicros renders non-negative micro-dollars as a decimal string without floats.
func formatMicros(micros int64) string {
	return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
}
