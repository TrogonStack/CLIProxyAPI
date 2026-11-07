package usage

import (
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

func cleanTurnID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > maxTurnIDLength {
		return ""
	}
	return strings.Clone(id)
}
