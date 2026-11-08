package codex

import "net/http"

// NewCodexAuthWithTransport lets tests of other packages stub the token endpoint.
func NewCodexAuthWithTransport(transport http.RoundTripper) *CodexAuth {
	return &CodexAuth{httpClient: &http.Client{Transport: transport}}
}
