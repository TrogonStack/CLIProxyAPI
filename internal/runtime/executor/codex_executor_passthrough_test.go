package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const passthroughUpstream = "event: response.created\n" +
	`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}` + "\n\n" +
	"event: response.in_progress\n" +
	`data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_1","status":"in_progress"}}` + "\n\n"

const passthroughErrorFrame = "event: error\n" +
	`data: {"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"sequence_number":3}` + "\n\n"

func runCodexPassthrough(t *testing.T, enabled bool) (string, error, bool) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(passthroughUpstream + passthroughErrorFrame))
	}))
	defer server.Close()

	cfg := &config.Config{}
	exec := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	if enabled {
		auth.Attributes[cliproxyauth.AttributeCodexResponsesPassthrough] = "true"
	}
	ctx, marker := cliproxyexecutor.WithStreamPassthrough(context.Background())
	res, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var body strings.Builder
	var streamErr error
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		body.Write(chunk.Payload)
	}
	return body.String(), streamErr, marker.Enabled()
}

func TestCodexExecutorStreamPassthroughForwardsErrorEventVerbatim(t *testing.T) {
	body, streamErr, marked := runCodexPassthrough(t, true)
	if !marked {
		t.Fatal("stream was not marked as pass-through")
	}
	if body != passthroughUpstream+passthroughErrorFrame {
		t.Fatalf("upstream bytes were not forwarded verbatim.\ngot:  %q\nwant: %q", body, passthroughUpstream+passthroughErrorFrame)
	}
	if streamErr == nil {
		t.Fatal("terminal failure must still surface as an error chunk so the credential state is updated")
	}
}

func TestCodexExecutorStreamPassthroughOffIsUnchanged(t *testing.T) {
	body, streamErr, marked := runCodexPassthrough(t, false)
	if marked {
		t.Fatal("stream marked as pass-through with the flag off")
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("default path must not forward the raw error frame: %q", body)
	}
	if streamErr == nil {
		t.Fatal("default path must report the terminal failure as an error chunk")
	}
}

func TestCodexExecutorStreamPassthroughForwardsCompletionVerbatim(t *testing.T) {
	completed := "event: response.completed\n" +
		`data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(passthroughUpstream + completed))
	}))
	defer server.Close()

	cfg := &config.Config{}
	exec := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test", cliproxyauth.AttributeCodexResponsesPassthrough: "true"}}
	res, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var body strings.Builder
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		body.Write(chunk.Payload)
	}
	if body.String() != passthroughUpstream+completed {
		t.Fatalf("completion was not forwarded verbatim.\ngot:  %q\nwant: %q", body.String(), passthroughUpstream+completed)
	}
}

func TestCodexResponsesPassthroughPrecedence(t *testing.T) {
	attr := func(v string) *cliproxyauth.Auth {
		return &cliproxyauth.Auth{Attributes: map[string]string{cliproxyauth.AttributeCodexResponsesPassthrough: v}}
	}
	meta := func(v any) *cliproxyauth.Auth {
		return &cliproxyauth.Auth{Metadata: map[string]any{"responses_passthrough": v}}
	}
	plain := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k"}}
	tests := []struct {
		name string
		cfg  bool
		env  string
		auth *cliproxyauth.Auth
		want bool
	}{
		{"nothing set is off", false, "", &cliproxyauth.Auth{}, false},
		{"nil auth and nothing set is off", false, "", nil, false},
		{"env on applies", false, "true", &cliproxyauth.Auth{}, true},
		{"env on applies to nil auth", false, "1", nil, true},
		{"env off", false, "false", &cliproxyauth.Auth{}, false},
		{"invalid env is off", false, "maybe", &cliproxyauth.Auth{}, false},
		{"config key on applies", true, "", &cliproxyauth.Auth{}, true},
		{"config key on applies to nil auth", true, "", nil, true},
		{"invalid env falls through to config key", true, "maybe", plain, true},
		{"env off beats config key on", true, "false", plain, false},
		{"env on without config key", false, "true", plain, true},
		{"account on without env or config", false, "", attr("true"), true},
		{"account on beats env off", false, "false", attr("true"), true},
		{"account off beats env on", false, "true", attr("false"), false},
		{"account off beats config key on", true, "", attr("false"), false},
		{"account on beats config key off", false, "", attr("true"), true},
		{"account metadata bool on", false, "", meta(true), true},
		{"account metadata off beats env on", false, "true", meta(false), false},
		{"account metadata string on", false, "", meta("true"), true},
		{"attribute beats metadata", false, "", &cliproxyauth.Auth{Attributes: map[string]string{cliproxyauth.AttributeCodexResponsesPassthrough: "false"}, Metadata: map[string]any{"responses_passthrough": true}}, false},
		{"unset account inherits env", false, "true", plain, true},
		{"unset account inherits config key", true, "", plain, true},
		{"invalid account value inherits env", false, "true", attr("nope"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(codexResponsesPassthroughEnv, tc.env)
			cfg := &config.Config{}
			cfg.Codex.ResponsesPassthrough = tc.cfg
			if got := codexResponsesPassthroughEnabled(cfg, tc.auth); got != tc.want {
				t.Fatalf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
