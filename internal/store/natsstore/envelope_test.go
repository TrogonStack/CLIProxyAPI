package natsstore

import (
	"bytes"
	"testing"
)

func TestEnvelopeRoundTripKeepsBytesExact(t *testing.T) {
	payloads := map[string][]byte{
		"json":    []byte("{\"type\": \"test\",\n  \"token\": \"a b\"}\n"),
		"yaml":    []byte("port: 8317\nhosts:\n  - \"x\"\n"),
		"binary":  {0x00, 0xff, 0x7b, 0x22},
		"empty":   {},
		"looks":   []byte(`{"natsstore_envelope":1,"origin":"x","data":"AA=="}`),
		"unicode": []byte("name: é\r\n"),
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			raw, err := encodeEnvelope("replica-a", 7, payload)
			if err != nil {
				t.Fatal(err)
			}
			data, origin, base := decodeEnvelope(raw)
			if !bytes.Equal(data, payload) || origin != "replica-a" || base != 7 {
				t.Fatalf("decoded = %q, %q, %d", data, origin, base)
			}
		})
	}
}

func TestDecodeEnvelopeAcceptsLegacyValues(t *testing.T) {
	legacy := map[string]string{
		"credential":     `{"type":"test","access_token":"t"}`,
		"yaml":           "port: 8317\n",
		"flow yaml":      "{port: 8317}\n",
		"other version":  `{"natsstore_envelope":2,"origin":"x","data":"AA=="}`,
		"not json":       "{broken",
		"empty":          "",
		"json array":     `[1,2]`,
		"spaced":         "  \n{\"type\":\"test\"}",
		"wrong data key": `{"natsstore_envelope":"1"}`,
	}
	for name, raw := range legacy {
		t.Run(name, func(t *testing.T) {
			data, origin, base := decodeEnvelope([]byte(raw))
			if string(data) != raw || origin != "" || base != 0 {
				t.Fatalf("decoded = %q, %q, %d, want the raw value", data, origin, base)
			}
		})
	}
}
