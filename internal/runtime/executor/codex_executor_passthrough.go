package executor

import (
	"bytes"
	"context"
	"net/http"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// codexResponsesPassthroughEligible reports whether an HTTP/SSE Codex stream may be forwarded to
// the client byte for byte: the flag is on, no translation is needed in either direction, and no
// feature that has to hold or rewrite frames (bootstrap buffering, Grok keepalive rewriting) is active.
func (e *CodexExecutor) codexResponsesPassthroughEligible(from, responseFormat sdktranslator.Format, buffering, isGrokClient bool) bool {
	return e.cfg != nil && e.cfg.Codex.ResponsesPassthrough && !buffering && !isGrokClient &&
		from == sdktranslator.FormatOpenAIResponse && responseFormat == sdktranslator.FormatOpenAIResponse
}

// streamCodexPassthrough forwards upstream SSE lines unchanged. Every chunk is one whole SSE event
// with its lines and blank separator exactly as read. Events are still
// observed for usage, replay caching and failure classification, but never rewritten.
//
// A terminal failure that arrives after bytes were emitted is forwarded raw and followed by an
// error chunk that downstream consumers must not render: the error only lets the conductor
// update credential state, the same as in the default path. A failure before any byte was emitted
// is reported as an error chunk alone so the attempt can still be retried.
func (e *CodexExecutor) streamCodexPassthrough(
	ctx context.Context,
	httpResp *http.Response,
	reporter *helps.UsageReporter,
	replayScope codexReasoningReplayScope,
	upstreamBody []byte,
) *cliproxyexecutor.StreamResult {
	scanner := helps.NewCodexSSEScanner(ctx, httpResp.Body, 52_428_800)
	scanner.SetAssembly(false)
	scanner.SetOnLine(func(rawLine []byte) {
		helps.AppendAPIResponseChunk(ctx, e.cfg, rawLine)
	})
	cliproxyexecutor.MarkStreamPassthrough(ctx)

	out := make(chan cliproxyexecutor.StreamChunk, 16)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		sawOutputDelta := false
		emitted := 0
		// frame holds the upstream lines of the SSE event being read, newlines included. It is sent
		// as one chunk when its blank separator line arrives, which keeps chunk boundaries on event
		// boundaries as the rest of the pipeline expects.
		var frame []byte
		sendFrame := func(incomplete bool) bool {
			if len(frame) == 0 {
				return true
			}
			chunk := frame
			frame = nil
			if !send(cliproxyexecutor.StreamChunk{Payload: chunk, Incomplete: incomplete}) {
				return false
			}
			emitted++
			return true
		}
		// fail forwards the frame the failure arrived in, then reports the error. The error chunk
		// only lets the conductor update credential state; it is not rendered for the client. A
		// failure before anything was emitted is reported alone so the attempt can still be retried.
		fail := func(streamErr error) {
			if emitted > 0 {
				frame = append(frame, '\n')
				if !sendFrame(false) {
					return
				}
			}
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			reporter.PublishFailure(ctx, streamErr)
			send(cliproxyexecutor.StreamChunk{Err: streamErr})
		}

		for scanner.Scan() {
			line := scanner.Bytes()
			frame = append(frame, line...)
			frame = append(frame, '\n')

			if len(bytes.TrimSpace(line)) == 0 {
				if !sendFrame(false) {
					return
				}
				continue
			}
			if !bytes.HasPrefix(line, dataTag) {
				continue
			}
			data := bytes.TrimSpace(line[5:])
			observeCodexTokenEvent(reporter, data)
			eventType := ojson.GetBytesNoCopy(data, "type").String()
			if streamErr, terminalBody, ok := codexTerminalFailureErrWithCooling(data, e.modelLevelCooling()); ok {
				if errClear := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClear != nil {
					fail(errClear)
					return
				}
				fail(streamErr)
				return
			}
			if helps.HasMeaningfulCodexOutputDelta(data) {
				sawOutputDelta = true
			}
			if helps.IsCodexTerminalEmptyIncomplete(data, len(outputItemsByIndex)+len(outputItemsFallback), sawOutputDelta) {
				fail(newCodexEmptyIncompleteStreamError())
				return
			}
			switch eventType {
			case "response.output_item.done":
				collectCodexOutputItemDone(data, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.incomplete", "response.done":
				observed := normalizeCodexWebsocketCompletion(data)
				if detail, ok := helps.ParseCodexUsageWithOutputItems(observed, outputItemsByIndex, outputItemsFallback); ok {
					reporter.Publish(ctx, detail)
				} else {
					reporter.EnsurePublished(ctx)
				}
				publishCodexImageToolUsageWithOutputItems(ctx, reporter, upstreamBody, observed, outputItemsByIndex, outputItemsFallback)
				if eventType == "response.completed" || eventType == "response.done" {
					cacheCodexReasoningReplayFromCompleted(replayScope, observed)
				}
				// Close the frame the terminal data line belongs to, then stop.
				frame = append(frame, '\n')
				sendFrame(eventType == "response.incomplete")
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if ctx.Err() != nil {
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
		}
		if emitted == 0 && len(bytes.TrimSpace(frame)) == 0 {
			emptyErr := statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before first payload"}
			helps.RecordAPIResponseError(ctx, e.cfg, emptyErr)
			reporter.PublishFailure(ctx, emptyErr)
			return
		}
		if !sendFrame(false) {
			return
		}
		fail(newCodexIncompleteStreamError())
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}
}
