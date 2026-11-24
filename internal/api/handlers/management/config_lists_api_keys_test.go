package management

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func apiKeysHandler(t *testing.T, entries ...config.APIKeyEntry) *Handler {
	t.Helper()
	return &Handler{
		cfg:            &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: entries}},
		configFilePath: writeTestConfigFile(t),
	}
}

func callAPIKeys(t *testing.T, call func(*gin.Context), method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	call(c)
	return rec
}

func TestPatchAPIKeysRenameKeepsBinding(t *testing.T) {
	for name, body := range map[string]string{
		"by index":     `{"index":1,"value":"renamed"}`,
		"by old / new": `{"old":"personal","new":"renamed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := apiKeysHandler(t, config.APIKeyEntry{Key: "plain"}, config.APIKeyEntry{Key: "personal", Auths: []string{"a.json"}})
			rec := callAPIKeys(t, h.PatchAPIKeys, http.MethodPatch, "/v0/management/api-keys", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			want := []config.APIKeyEntry{{Key: "plain"}, {Key: "renamed", Auths: []string{"a.json"}}}
			if !reflect.DeepEqual(h.cfg.APIKeys, want) {
				t.Fatalf("APIKeys = %#v, want %#v", h.cfg.APIKeys, want)
			}
		})
	}
}

func TestPatchAPIKeysAppendsUnboundAndRejectsDuplicates(t *testing.T) {
	h := apiKeysHandler(t, config.APIKeyEntry{Key: "personal", Auths: []string{"a.json"}})
	if rec := callAPIKeys(t, h.PatchAPIKeys, http.MethodPatch, "/", `{"old":"missing","new":"fresh"}`); rec.Code != http.StatusOK {
		t.Fatalf("append status = %d", rec.Code)
	}
	if got := h.cfg.APIKeys; len(got) != 2 || got[1].Key != "fresh" || got[1].Bound() {
		t.Fatalf("APIKeys = %#v", got)
	}
	before := append([]config.APIKeyEntry(nil), h.cfg.APIKeys...)
	if rec := callAPIKeys(t, h.PatchAPIKeys, http.MethodPatch, "/", `{"old":"fresh","new":"personal"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate rename status = %d, want 400", rec.Code)
	}
	if !reflect.DeepEqual(h.cfg.APIKeys, before) {
		t.Fatalf("a rejected patch changed the keys: %#v", h.cfg.APIKeys)
	}
}

func TestPutAPIKeysKeepsBindingsAndRejectsDuplicates(t *testing.T) {
	h := apiKeysHandler(t, config.APIKeyEntry{Key: "personal", Auths: []string{"a.json"}})
	if rec := callAPIKeys(t, h.PutAPIKeys, http.MethodPut, "/", `["personal","other"]`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := []config.APIKeyEntry{{Key: "personal", Auths: []string{"a.json"}}, {Key: "other"}}
	if !reflect.DeepEqual(h.cfg.APIKeys, want) {
		t.Fatalf("APIKeys = %#v, want %#v", h.cfg.APIKeys, want)
	}
	if rec := callAPIKeys(t, h.PutAPIKeys, http.MethodPut, "/", `["dup","dup"]`); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate PUT status = %d, want 400", rec.Code)
	}
	if !reflect.DeepEqual(h.cfg.APIKeys, want) {
		t.Fatalf("a rejected PUT changed the keys: %#v", h.cfg.APIKeys)
	}
}

func TestPutAPIKeysAcceptsWhatGetEmits(t *testing.T) {
	entries := []config.APIKeyEntry{
		{Key: "plain"},
		{Key: "personal", Auths: []string{"a.json", "b.json"}},
		{Key: "work", Auths: []string{"c.json"}},
	}
	h := apiKeysHandler(t, entries...)
	got := callAPIKeys(t, h.GetAPIKeys, http.MethodGet, "/v0/management/api-keys", "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET status = %d", got.Code)
	}
	var emitted struct {
		APIKeys json.RawMessage `json:"api-keys"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &emitted); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"GET body as is":   got.Body.String(),
		"bare array":       string(emitted.APIKeys),
		"items wrapper":    `{"items":` + string(emitted.APIKeys) + `}`,
		"api-keys wrapper": `{"api-keys":` + string(emitted.APIKeys) + `}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			target := apiKeysHandler(t, entries...)
			if rec := callAPIKeys(t, target.PutAPIKeys, http.MethodPut, "/v0/management/api-keys", body); rec.Code != http.StatusOK {
				t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(target.cfg.APIKeys, entries) {
				t.Fatalf("round trip changed the keys: %#v, want %#v", target.cfg.APIKeys, entries)
			}
		})
	}
}

func TestPutAPIKeysTakesExplicitBindingsAndValidatesThem(t *testing.T) {
	h := apiKeysHandler(t, config.APIKeyEntry{Key: "old", Auths: []string{"a.json"}})
	body := `["plain",{"key":"bound","auths":["x.json"]},{"key":"old","auths":["b.json"]}]`
	if rec := callAPIKeys(t, h.PutAPIKeys, http.MethodPut, "/", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := []config.APIKeyEntry{{Key: "plain"}, {Key: "bound", Auths: []string{"x.json"}}, {Key: "old", Auths: []string{"b.json"}}}
	if !reflect.DeepEqual(h.cfg.APIKeys, want) {
		t.Fatalf("APIKeys = %#v, want %#v", h.cfg.APIKeys, want)
	}
	for name, bad := range map[string]string{
		"duplicate across forms": `["k",{"key":"k","auths":["a.json"]}]`,
		"unknown field":          `[{"key":"k","auth":["a.json"]}]`,
		"empty auths":            `[{"key":"k","auths":[]}]`,
		"blank auth item":        `[{"key":"k","auths":["  "]}]`,
	} {
		if rec := callAPIKeys(t, h.PutAPIKeys, http.MethodPut, "/", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 body=%s", name, rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(h.cfg.APIKeys, want) {
			t.Fatalf("%s: a rejected PUT changed the keys: %#v", name, h.cfg.APIKeys)
		}
	}
}
