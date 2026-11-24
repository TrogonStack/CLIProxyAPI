package tui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/json"
)

// bindingServer serves a mixed api-keys list and records the last PUT body.
func bindingServer(t *testing.T, initial string, put *string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v8/management/config/access/api-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			*put = string(body)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(initial))
	})
	mux.HandleFunc("/v8/management/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access":{"api-keys":` + initial + `}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClientWithBaseURL(srv.URL, "secret")
}

const mixedAPIKeys = `["plain",{"key":"personal","auths":["codex-personal.json"]}]`

func TestClientGetAPIKeysReadsBoundEntries(t *testing.T) {
	var put string
	keys, err := bindingServer(t, mixedAPIKeys, &put).GetAPIKeys()
	if err != nil || len(keys) != 2 || keys[0] != "plain" || keys[1] != "personal" {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
}

func TestClientAPIKeyMutationsKeepBindings(t *testing.T) {
	assertPut := func(t *testing.T, put, want string) {
		t.Helper()
		var got, expect any
		if err := json.Unmarshal([]byte(put), &got); err != nil {
			t.Fatalf("PUT body %q: %v", put, err)
		}
		_ = json.Unmarshal([]byte(want), &expect)
		gotJSON, _ := json.Marshal(got)
		expectJSON, _ := json.Marshal(expect)
		if string(gotJSON) != string(expectJSON) {
			t.Fatalf("PUT body = %s, want %s", gotJSON, expectJSON)
		}
	}

	t.Run("add", func(t *testing.T) {
		var put string
		if err := bindingServer(t, mixedAPIKeys, &put).AddAPIKey("new"); err != nil {
			t.Fatal(err)
		}
		assertPut(t, put, `["plain",{"key":"personal","auths":["codex-personal.json"]},"new"]`)
	})
	t.Run("edit moves the binding to the new key", func(t *testing.T) {
		var put string
		if err := bindingServer(t, mixedAPIKeys, &put).EditAPIKey(1, "renamed", "personal"); err != nil {
			t.Fatal(err)
		}
		assertPut(t, put, `["plain",{"key":"renamed","auths":["codex-personal.json"]}]`)
	})
	t.Run("delete another key keeps the binding", func(t *testing.T) {
		var put string
		if err := bindingServer(t, mixedAPIKeys, &put).DeleteAPIKey(0, "plain"); err != nil {
			t.Fatal(err)
		}
		assertPut(t, put, `[{"key":"personal","auths":["codex-personal.json"]}]`)
	})
}
