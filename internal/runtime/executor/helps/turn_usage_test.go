package helps

import (
	"context"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestParseCodexUsageCapturesResponseID(t *testing.T) {
	data := []byte(`{"type":"response.completed","response":{"id":"resp_abc","usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`)
	detail, ok := ParseCodexUsage(data)
	if !ok {
		t.Fatal("ParseCodexUsage() ok = false, want true")
	}
	if detail.ResponseID != "resp_abc" {
		t.Fatalf("ResponseID = %q, want resp_abc", detail.ResponseID)
	}
}

func TestUsageReporterTurnIDFromHeaderThenBody(t *testing.T) {
	ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{TurnID: "turn-header"})
	reporter := newCodexTestReporter(ctx, "gpt-5", nil)
	if got := reporter.buildRecord(usage.Detail{}, false).TurnID; got != "turn-header" {
		t.Fatalf("TurnID = %q, want turn-header", got)
	}

	reporter.SetTranslatedRequestMetadata([]byte(`{"client_metadata":{"turn_id":"turn-body"}}`), "codex")
	if got := reporter.buildRecord(usage.Detail{}, false).TurnID; got != "turn-body" {
		t.Fatalf("TurnID = %q, want turn-body", got)
	}

	reporter.SetTranslatedRequestMetadata([]byte(`{"model":"gpt-5"}`), "codex")
	if got := reporter.buildRecord(usage.Detail{}, false).TurnID; got != "turn-body" {
		t.Fatalf("TurnID = %q, want turn-body kept when a body has none", got)
	}
}
