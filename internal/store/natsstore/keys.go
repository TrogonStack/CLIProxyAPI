package natsstore

import (
	"encoding/base64"
	"fmt"
)

// maxKeyLength bounds an encoded key. A key becomes the last token of a
// subject, so it stays far below the server control line limit.
const maxKeyLength = 512

// keyEncoding is unpadded base64url. Its alphabet is a subset of what KV keys
// allow and has no dot, so an encoded key is always one subject token.
var keyEncoding = base64.RawURLEncoding

// EncodeKey turns an identifier, which may hold slashes, dots, @ or any UTF-8,
// into a KV key. The mapping is reversible and collision free.
func EncodeKey(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("key id is empty")
	}
	key := keyEncoding.EncodeToString([]byte(id))
	if len(key) > maxKeyLength {
		return "", fmt.Errorf("key id is too long: encoded length %d exceeds %d", len(key), maxKeyLength)
	}
	return key, nil
}

// DecodeKey returns the identifier a key was encoded from.
func DecodeKey(key string) (string, error) {
	raw, err := keyEncoding.DecodeString(key)
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("key %q is not an encoded identifier", key)
	}
	return string(raw), nil
}
