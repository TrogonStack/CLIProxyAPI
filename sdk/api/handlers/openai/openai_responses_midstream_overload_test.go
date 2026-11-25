package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const midstreamOverloadPayload = `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error","headers":{"retry-after":"343","retry-after-ms":"343000"}},"sequence_number":3}`

func runMidstreamOverload(t *testing.T, userAgent string, viaErrChan bool) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	flusher := c.Writer.(http.Flusher)

	data := make(chan []byte, 4)
	errs := make(chan *interfaces.ErrorMessage, 1)
	data <- []byte("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n")
	data <- []byte("event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n")
	if viaErrChan {
		errs <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New(midstreamOverloadPayload)}
	} else {
		data <- []byte("data: " + midstreamOverloadPayload + "\n\n")
	}
	close(data)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	return recorder.Body.String()
}

func TestMidstreamOverloadEmitsResponseFailed(t *testing.T) {
	for _, tc := range []struct {
		name, ua string
		viaErr   bool
	}{
		{"codex data frame", "codex_cli_rs/0.1", false},
		{"codex err chan", "codex_cli_rs/0.1", true},
		{"generic data frame", "OpenAI/Python 1.0", false},
		{"generic err chan", "OpenAI/Python 1.0", true},
		{"no UA data frame", "", false},
		{"codex_vscode err chan", "codex_vscode/0.153.4 (Ubuntu 22.4.0; x86_64)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := runMidstreamOverload(t, tc.ua, tc.viaErr)
			t.Logf("%q", body)
			if !strings.Contains(body, "event: response.failed\ndata: ") {
				t.Fatalf("missing response.failed event: %q", body)
			}
			for _, want := range []string{`"type":"response.failed"`, `"status":"failed"`, `"code":"server_is_overloaded"`, `"sequence_number":3`} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %s in %q", want, body)
				}
			}
		})
	}
}
