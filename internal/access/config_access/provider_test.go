package configaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func authenticate(t *testing.T, p *provider, key string) ([]string, bool) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		return nil, false
	}
	return result.AllowedAuths, true
}

func TestProviderReturnsAllowedAuthsForBoundKey(t *testing.T) {
	p := newProvider("", normalizeKeys([]sdkconfig.APIKeyEntry{
		{Key: "plain"},
		{Key: " personal ", Auths: []string{" codex-personal.json ", "", "claude-personal.json"}},
	}))
	if auths, ok := authenticate(t, p, "plain"); !ok || len(auths) != 0 {
		t.Fatalf("plain key: auths=%v ok=%v, want unrestricted", auths, ok)
	}
	auths, ok := authenticate(t, p, "personal")
	if !ok {
		t.Fatal("bound key rejected")
	}
	if want := []string{"codex-personal.json", "claude-personal.json"}; !reflect.DeepEqual(auths, want) {
		t.Fatalf("auths = %v, want %v", auths, want)
	}
	if _, ok = authenticate(t, p, "unknown"); ok {
		t.Fatal("unknown key accepted")
	}
}

func TestProviderFirstDuplicateKeyWins(t *testing.T) {
	p := newProvider("", normalizeKeys([]sdkconfig.APIKeyEntry{
		{Key: "dup", Auths: []string{"a.json"}},
		{Key: "dup"},
	}))
	if auths, ok := authenticate(t, p, "dup"); !ok || !reflect.DeepEqual(auths, []string{"a.json"}) {
		t.Fatalf("auths=%v ok=%v, want the first (bound) entry", auths, ok)
	}
}

func TestProviderAllowedAuthsAcceptedFromEveryKeySource(t *testing.T) {
	p := newProvider("", normalizeKeys([]sdkconfig.APIKeyEntry{{Key: "k", Auths: []string{"a.json"}}}))
	for name, set := range map[string]func(r *http.Request){
		"x-api-key":      func(r *http.Request) { r.Header.Set("X-Api-Key", "k") },
		"x-goog-api-key": func(r *http.Request) { r.Header.Set("X-Goog-Api-Key", "k") },
	} {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		set(req)
		result, authErr := p.Authenticate(context.Background(), req)
		if authErr != nil || !reflect.DeepEqual(result.AllowedAuths, []string{"a.json"}) {
			t.Fatalf("%s: result=%v err=%v", name, result, authErr)
		}
	}
}
