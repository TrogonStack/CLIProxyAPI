package config

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/json"
	"gopkg.in/yaml.v3"
)

// APIKeyEntry is one client API key accepted by this proxy.
//
// In YAML an entry is either a plain string (an unrestricted key) or a mapping
// with a "key" and an optional "auths" list that binds the key to specific
// credentials. A bound key can only be served by the listed credentials and
// only sees the models those credentials serve.
type APIKeyEntry struct {
	// Key is the client API key presented by callers.
	Key string `yaml:"key" json:"key"`

	// Auths lists the credentials the key is bound to. Each item matches either
	// the credential ID or the credential file name (for example
	// "team/work.json"). An empty list means the key is not bound.
	Auths []string `yaml:"auths,omitempty" json:"auths,omitempty"`
}

// Bound reports whether the entry restricts the key to specific credentials.
func (e APIKeyEntry) Bound() bool { return len(e.Auths) > 0 }

// APIKeyEntries converts plain keys into unrestricted entries.
func APIKeyEntries(keys ...string) []APIKeyEntry {
	out := make([]APIKeyEntry, 0, len(keys))
	for _, key := range keys {
		out = append(out, APIKeyEntry{Key: key})
	}
	return out
}

// APIKeyStrings returns the plain key values of the configured client API keys.
func (c *SDKConfig) APIKeyStrings() []string {
	if c == nil || len(c.APIKeys) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.APIKeys))
	for _, entry := range c.APIKeys {
		out = append(out, entry.Key)
	}
	return out
}

// SetAPIKeyStrings replaces the configured keys with keys, keeping the
// credential binding of any key that was already configured.
func (c *SDKConfig) SetAPIKeyStrings(keys []string) {
	bindings := make(map[string][]string, len(c.APIKeys))
	for _, entry := range c.APIKeys {
		if entry.Bound() {
			bindings[strings.TrimSpace(entry.Key)] = entry.Auths
		}
	}
	next := make([]APIKeyEntry, 0, len(keys))
	for _, key := range keys {
		next = append(next, APIKeyEntry{Key: key, Auths: bindings[strings.TrimSpace(key)]})
	}
	c.APIKeys = next
}

// UnmarshalYAML accepts a plain string or a {key, auths} mapping.
func (e *APIKeyEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var key string
		if err := node.Decode(&key); err != nil {
			return err
		}
		*e = APIKeyEntry{Key: key}
		return nil
	case yaml.MappingNode:
		type plain APIKeyEntry
		var decoded plain
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		entry := APIKeyEntry(decoded)
		if err := entry.validate(node.Content, node.Line); err != nil {
			return err
		}
		*e = entry
		return nil
	default:
		return fmt.Errorf("line %d: api-keys entries must be a string or a mapping with key and auths", node.Line)
	}
}

func (e APIKeyEntry) validate(content []*yaml.Node, line int) error {
	authsPresent := false
	for i := 0; i+1 < len(content); i += 2 {
		switch content[i].Value {
		case "key":
		case "auths":
			authsPresent = true
		default:
			return fmt.Errorf("line %d: api-keys entry has unknown field %q; only key and auths are allowed", content[i].Line, content[i].Value)
		}
	}
	if err := e.check(authsPresent); err != nil {
		return fmt.Errorf("line %d: %w", line, err)
	}
	return nil
}

// check rejects an entry that would silently widen access: a blank key, or an
// auths list that is present but empty or holds a blank item.
func (e APIKeyEntry) check(authsPresent bool) error {
	if strings.TrimSpace(e.Key) == "" {
		return fmt.Errorf("api-keys entry requires a non-empty key")
	}
	if authsPresent && len(e.Auths) == 0 {
		return fmt.Errorf("api-keys entry has an empty auths list; remove auths to leave the key unrestricted")
	}
	for _, ref := range e.Auths {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("api-keys entry has an empty auths item")
		}
	}
	return nil
}

// MarshalYAML writes unbound keys as plain strings so existing files keep their shape.
func (e APIKeyEntry) MarshalYAML() (any, error) {
	if !e.Bound() {
		return e.Key, nil
	}
	type plain APIKeyEntry
	return plain(e), nil
}

// UnmarshalJSON accepts a plain string or a {key, auths} object.
func (e *APIKeyEntry) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "\"") {
		var key string
		if err := json.Unmarshal(data, &key); err != nil {
			return err
		}
		*e = APIKeyEntry{Key: key}
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		if name != "key" && name != "auths" {
			return fmt.Errorf("api-keys entry has unknown field %q; only key and auths are allowed", name)
		}
	}
	type plain APIKeyEntry
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	_, authsPresent := fields["auths"]
	entry := APIKeyEntry(decoded)
	if err := entry.check(authsPresent); err != nil {
		return err
	}
	*e = entry
	return nil
}

// MarshalJSON mirrors MarshalYAML.
func (e APIKeyEntry) MarshalJSON() ([]byte, error) {
	if !e.Bound() {
		return json.Marshal(e.Key)
	}
	type plain APIKeyEntry
	return json.Marshal(plain(e))
}

// ValidateAPIKeys rejects client API keys that cannot be served safely. A key
// listed twice is ambiguous because the copies may carry different credential
// bindings, so it is a configuration error instead of "first one wins".
func (c *SDKConfig) ValidateAPIKeys() error {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(c.APIKeys))
	for index, entry := range c.APIKeys {
		trimmed := strings.TrimSpace(entry.Key)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			return fmt.Errorf("api-keys[%d] duplicates an earlier api-keys entry; list each key once", index)
		}
		seen[trimmed] = struct{}{}
	}
	return nil
}
