package configaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func TestProviderDuplicateKeyFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		keys []sdkconfig.APIKeyEntry
		want []string
	}{
		"plain before bound": {[]sdkconfig.APIKeyEntry{{Key: "dup"}, {Key: "dup", Auths: []string{"a.json"}}}, []string{"a.json"}},
		"bound before plain": {[]sdkconfig.APIKeyEntry{{Key: "dup", Auths: []string{"a.json"}}, {Key: "dup"}}, []string{"a.json"}},
		"two bound overlap":  {[]sdkconfig.APIKeyEntry{{Key: "dup", Auths: []string{"a.json", "b.json"}}, {Key: "dup", Auths: []string{"b.json", "c.json"}}}, []string{"b.json"}},
		"two bound disjoint": {[]sdkconfig.APIKeyEntry{{Key: "dup", Auths: []string{"a.json"}}, {Key: "dup", Auths: []string{"b.json"}}}, []string{blockedAuthRef}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newProvider("", normalizeKeys(tc.keys))
			auths, ok := authenticate(t, p, "dup")
			if !ok || !reflect.DeepEqual(auths, tc.want) {
				t.Fatalf("auths=%v ok=%v, want %v", auths, ok, tc.want)
			}
		})
	}
}

func TestProviderBoundEntryWithOnlyBlankRefsStaysBound(t *testing.T) {
	p := newProvider("", normalizeKeys([]sdkconfig.APIKeyEntry{{Key: "k", Auths: []string{"", "  "}}}))
	auths, ok := authenticate(t, p, "k")
	if !ok || len(auths) == 0 {
		t.Fatalf("auths=%v ok=%v, want a non-empty restriction that matches nothing", auths, ok)
	}
	for _, ref := range auths {
		if strings.TrimSpace(ref) != "" {
			t.Fatalf("restriction %v must not name any credential", auths)
		}
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
