package natsstore

import (
	"bytes"
	"encoding/json"
)

// envelopeVersion marks a value written inside an envelope. A legacy value is
// the raw payload with no marker, which no credential or config carries.
const envelopeVersion = 1

// envelope wraps a stored payload with where it came from, so a replica can
// recognize its own writes when the bucket echoes them back.
type envelope struct {
	Version int `json:"natsstore_envelope"`
	// Origin identifies the replica that wrote the value.
	Origin string `json:"origin"`
	// Base is the revision the write replaced, zero for a create.
	Base uint64 `json:"base_revision"`
	// Data is the payload byte for byte, so the file it came from is restored exactly.
	Data []byte `json:"data"`
}

// encodeEnvelope wraps the payload for storage.
func encodeEnvelope(origin string, base uint64, data []byte) ([]byte, error) {
	return json.Marshal(envelope{Version: envelopeVersion, Origin: origin, Base: base, Data: data})
}

// decodeEnvelope unwraps a stored value. A value without the marker is the raw
// format the earlier backend wrote; it is returned unchanged with no origin.
func decodeEnvelope(raw []byte) (data []byte, origin string, base uint64) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return raw, "", 0
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != envelopeVersion {
		return raw, "", 0
	}
	return env.Data, env.Origin, env.Base
}
