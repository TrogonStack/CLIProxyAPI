package usage

import (
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

const maxTurnIDLength = 128

// TurnIDFromTurnMetadata extracts turn_id from the JSON carried by the
// x-codex-turn-metadata header or client_metadata entry.
func TurnIDFromTurnMetadata(metadata string) string {
	metadata = strings.TrimSpace(metadata)
	if metadata == "" || !ojson.Valid(metadata) {
		return ""
	}
	return cleanTurnID(ojson.Get(metadata, "turn_id").String())
}

// TurnIDFromRequestBody extracts the Codex turn id from a Responses request body,
// where Codex sends it as client_metadata.turn_id and inside the
// client_metadata["x-codex-turn-metadata"] JSON string.
func TurnIDFromRequestBody(body []byte) string {
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return ""
	}
	if id := cleanTurnID(ojson.GetBytesNoCopy(body, "client_metadata.turn_id").String()); id != "" {
		return id
	}
	return TurnIDFromTurnMetadata(ojson.GetBytesNoCopy(body, "client_metadata.x-codex-turn-metadata").String())
}

// ThreadIDFromTurnMetadata extracts thread_id from the x-codex-turn-metadata JSON.
func ThreadIDFromTurnMetadata(metadata string) string {
	metadata = strings.TrimSpace(metadata)
	if metadata == "" || !ojson.Valid(metadata) {
		return ""
	}
	return cleanTurnID(ojson.Get(metadata, "thread_id").String())
}

// ThreadIDFromHeaders returns the Codex thread id of a request, preferring the turn
// metadata header and falling back to the thread-id header.
func ThreadIDFromHeaders(headers http.Header) string {
	if id := ThreadIDFromTurnMetadata(headers.Get("X-Codex-Turn-Metadata")); id != "" {
		return id
	}
	return cleanTurnID(headers.Get("Thread-Id"))
}

// ThreadIDFromRequestBody extracts the Codex thread id from a Responses request body,
// where Codex sends it as client_metadata.thread_id and inside the
// client_metadata["x-codex-turn-metadata"] JSON string.
func ThreadIDFromRequestBody(body []byte) string {
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return ""
	}
	if id := cleanTurnID(ojson.GetBytesNoCopy(body, "client_metadata.thread_id").String()); id != "" {
		return id
	}
	return ThreadIDFromTurnMetadata(ojson.GetBytesNoCopy(body, "client_metadata.x-codex-turn-metadata").String())
}

func cleanTurnID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > maxTurnIDLength {
		return ""
	}
	return strings.Clone(id)
}
