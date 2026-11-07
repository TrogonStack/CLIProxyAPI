// Package natsstore holds the NATS JetStream connection, settings and bucket
// bootstrap that back the NATS token store.
package natsstore

import (
	"fmt"
	"strings"
)

const redacted = "[redacted]"

// Secret wraps a credential so it never reaches logs, errors or serialized
// output by accident. Only Reveal returns the value.
type Secret struct {
	value string
}

// NewSecret wraps value, reporting false for an empty one because empty counts as unset.
func NewSecret(value string) (Secret, bool) {
	if value == "" {
		return Secret{}, false
	}
	return Secret{value: value}, true
}

// Reveal returns the wrapped value for the one place that hands it to the NATS client.
func (s Secret) Reveal() string { return s.value }

func (s Secret) String() string { return redacted }

func (s Secret) GoString() string { return redacted }

// Format redacts the secret under every fmt verb, including %v and %+v on a struct holding it.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// redactServer strips URL userinfo so a server address is safe to log.
func redactServer(server string) string {
	rest := server
	scheme := ""
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme, rest = rest[:i+3], rest[i+3:]
	}
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		return scheme + redacted + "@" + rest[at+1:]
	}
	return server
}

func redactServers(servers []string) []string {
	out := make([]string, len(servers))
	for i, server := range servers {
		out[i] = redactServer(server)
	}
	return out
}
