package configaccess

import (
	"context"
	"net/http"
	"strings"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	keys := normalizeKeys(cfg.APIKeys)
	if len(keys) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keys),
	)
}

type provider struct {
	name string
	// keys maps each accepted key to the credentials it is bound to. A nil
	// value means the key is unrestricted.
	keys map[string][]string
}

func newProvider(name string, keys []sdkconfig.APIKeyEntry) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	keySet := make(map[string][]string, len(keys))
	for _, entry := range keys {
		keySet[entry.Key] = entry.Auths
	}
	return &provider{name: providerName, keys: keySet}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	apiKey := extractBearerToken(authHeader)

	candidates := []struct {
		value  string
		source string
	}{
		{apiKey, "authorization"},
		{authHeaderGoogle, "x-goog-api-key"},
		{authHeaderAnthropic, "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}

	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if auths, ok := p.keys[candidate.value]; ok {
			return &sdkaccess.Result{
				Provider:     p.Identifier(),
				Principal:    candidate.value,
				AllowedAuths: append([]string(nil), auths...),
				Metadata: map[string]string{
					"source": candidate.source,
				},
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

// blockedAuthRef is a restriction item that matches no credential. A bound key
// whose usable refs are all gone keeps this single item so it stays bound to
// nothing instead of becoming unrestricted.
const blockedAuthRef = " "

// normalizeKeys trims keys and drops blanks. A duplicated key is rejected at
// config load; if one still reaches this point it fails closed: the copies are
// intersected, and any bound copy outweighs an unrestricted one. A bound entry
// is never turned into an unrestricted one.
func normalizeKeys(keys []sdkconfig.APIKeyEntry) []sdkconfig.APIKeyEntry {
	if len(keys) == 0 {
		return nil
	}
	normalized := make([]sdkconfig.APIKeyEntry, 0, len(keys))
	index := make(map[string]int, len(keys))
	for _, entry := range keys {
		trimmedKey := strings.TrimSpace(entry.Key)
		if trimmedKey == "" {
			continue
		}
		var auths []string
		if entry.Auths != nil {
			auths = make([]string, 0, len(entry.Auths))
			for _, ref := range entry.Auths {
				if trimmedRef := strings.TrimSpace(ref); trimmedRef != "" {
					auths = append(auths, trimmedRef)
				}
			}
			if len(auths) == 0 {
				auths = []string{blockedAuthRef}
			}
		}
		if at, exists := index[trimmedKey]; exists {
			normalized[at].Auths = intersectAuths(normalized[at].Auths, auths)
			continue
		}
		index[trimmedKey] = len(normalized)
		normalized = append(normalized, sdkconfig.APIKeyEntry{Key: trimmedKey, Auths: auths})
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// intersectAuths combines the restrictions of two copies of one key. A nil list
// is unrestricted, so the other list wins; two lists keep only the shared refs.
func intersectAuths(a, b []string) []string {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	in := make(map[string]struct{}, len(b))
	for _, ref := range b {
		in[ref] = struct{}{}
	}
	var out []string
	for _, ref := range a {
		if _, ok := in[ref]; ok && ref != blockedAuthRef {
			out = append(out, ref)
		}
	}
	if len(out) == 0 {
		return []string{blockedAuthRef}
	}
	return out
}
