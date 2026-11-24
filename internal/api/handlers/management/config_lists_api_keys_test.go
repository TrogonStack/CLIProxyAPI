package management

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
