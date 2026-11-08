package executor

import cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"

// Providers whose refresh grants stay valid after use declare so here; a second
// replica sending the same grant is harmless, so they take no journal claim.
// Antigravity will take a short lease with the refresh queue. Every executor
// that declares nothing is treated as single-use.

// RefreshPolicy declares that Antigravity grants are reusable.
func (e *AntigravityExecutor) RefreshPolicy() cliproxyauth.RefreshPolicy {
	return cliproxyauth.RefreshNone
}

// RefreshPolicy declares that Devin grants are reusable.
func (e *DevinExecutor) RefreshPolicy() cliproxyauth.RefreshPolicy { return cliproxyauth.RefreshNone }

// RefreshPolicy declares that Meta grants are reusable.
func (e *MetaExecutor) RefreshPolicy() cliproxyauth.RefreshPolicy { return cliproxyauth.RefreshNone }

// RefreshPolicy declares that Vertex credentials are reusable.
func (e *GeminiVertexExecutor) RefreshPolicy() cliproxyauth.RefreshPolicy {
	return cliproxyauth.RefreshNone
}
