package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"gopkg.in/yaml.v3"
)

func TestAPIKeyEntryYAMLAcceptsBothForms(t *testing.T) {
	var cfg SDKConfig
	data := "api-keys:\n  - legacy-plain-key\n  - key: personal-key\n    auths: [codex-personal.json, claude-personal.json]\n  - key: mapping-without-auths\n"
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatal(err)
	}
	want := []APIKeyEntry{
		{Key: "legacy-plain-key"},
		{Key: "personal-key", Auths: []string{"codex-personal.json", "claude-personal.json"}},
		{Key: "mapping-without-auths"},
	}
	if !reflect.DeepEqual(cfg.APIKeys, want) {
		t.Fatalf("APIKeys = %#v, want %#v", cfg.APIKeys, want)
	}
	if cfg.APIKeys[0].Bound() || !cfg.APIKeys[1].Bound() || cfg.APIKeys[2].Bound() {
		t.Fatal("Bound() mismatch")
	}
}

func TestAPIKeyEntryYAMLRejectsInvalid(t *testing.T) {
	for name, data := range map[string]string{
		"empty auths": "api-keys:\n  - key: k\n    auths: []\n",
		"missing key": "api-keys:\n  - auths: [a.json]\n",
		"blank key":   "api-keys:\n  - key: \"  \"\n    auths: [a.json]\n",
		"blank auth":  "api-keys:\n  - key: k\n    auths: [\"\"]\n",
		"sequence":    "api-keys:\n  - [a, b]\n",
	} {
		t.Run(name, func(t *testing.T) {
			var cfg SDKConfig
			if err := yaml.Unmarshal([]byte(data), &cfg); err == nil {
				t.Fatalf("expected an error, got %#v", cfg.APIKeys)
			}
		})
	}
}

func TestAPIKeyEntryYAMLMarshalKeepsUnboundAsScalar(t *testing.T) {
	out, err := yaml.Marshal(SDKConfig{APIKeys: []APIKeyEntry{{Key: "plain"}, {Key: "bound", Auths: []string{"a.json"}}}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "- plain\n") || !strings.Contains(text, "key: bound") || !strings.Contains(text, "- a.json") {
		t.Fatalf("unexpected YAML:\n%s", text)
	}
}

func TestAPIKeyEntryJSONBothForms(t *testing.T) {
	var entries []APIKeyEntry
	if err := json.Unmarshal([]byte(`["plain",{"key":"bound","auths":["a.json"]}]`), &entries); err != nil {
		t.Fatal(err)
	}
	want := []APIKeyEntry{{Key: "plain"}, {Key: "bound", Auths: []string{"a.json"}}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
	out, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `["plain",{"key":"bound","auths":["a.json"]}]` {
		t.Fatalf("marshal = %s", out)
	}
	if err := json.Unmarshal([]byte(`[{"auths":["a.json"]}]`), &entries); err == nil {
		t.Fatal("missing key must be rejected")
	}
}

func TestSetAPIKeyStringsPreservesBindingsByKey(t *testing.T) {
	cfg := SDKConfig{APIKeys: []APIKeyEntry{{Key: "plain"}, {Key: "bound", Auths: []string{"a.json"}}}}
	cfg.SetAPIKeyStrings([]string{"bound", "new"})
	want := []APIKeyEntry{{Key: "bound", Auths: []string{"a.json"}}, {Key: "new"}}
	if !reflect.DeepEqual(cfg.APIKeys, want) {
		t.Fatalf("APIKeys = %#v, want %#v", cfg.APIKeys, want)
	}
	if got := cfg.APIKeyStrings(); !reflect.DeepEqual(got, []string{"bound", "new"}) {
		t.Fatalf("APIKeyStrings = %v", got)
	}
}

func TestAPIKeyBindingSurvivesLoadAndSaveRoundTrip(t *testing.T) {
	for name, data := range map[string]string{
		"legacy": "api-keys:\n  - legacy-plain-key\n  - key: personal-key\n    auths: [codex-personal.json]\n",
		"v8":     "access:\n  api-keys:\n    - legacy-plain-key\n    - key: personal-key\n      auths: [codex-personal.json]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			want := []APIKeyEntry{{Key: "legacy-plain-key"}, {Key: "personal-key", Auths: []string{"codex-personal.json"}}}
			if !reflect.DeepEqual(cfg.APIKeys, want) {
				t.Fatalf("loaded %#v, want %#v", cfg.APIKeys, want)
			}
			cfg.APIKeys = append(cfg.APIKeys, APIKeyEntry{Key: "work-key", Auths: []string{"claude-work.json"}})
			if err = SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			reloaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, APIKeyEntry{Key: "work-key", Auths: []string{"claude-work.json"}})
			if !reflect.DeepEqual(reloaded.APIKeys, want) {
				t.Fatalf("reloaded %#v, want %#v", reloaded.APIKeys, want)
			}
		})
	}
}

func TestAPIKeyEntryRejectsUnknownFieldsInYAMLAndJSON(t *testing.T) {
	for name, data := range map[string]string{
		"auth typo":       "{\"key\":\"k\",\"auth\":[\"a.json\"]}",
		"auths suffix":    "{\"key\":\"k\",\"auths_\":[\"a.json\"]}",
		"extra beside ok": "{\"key\":\"k\",\"auths\":[\"a.json\"],\"note\":\"x\"}",
		"wrong case":      "{\"Key\":\"k\"}",
	} {
		t.Run("json "+name, func(t *testing.T) {
			var e APIKeyEntry
			if err := json.Unmarshal([]byte(data), &e); err == nil {
				t.Fatalf("accepted %s as %#v", data, e)
			}
		})
	}
	for name, data := range map[string]string{
		"auth typo":    "api-keys:\n  - key: k\n    auth: [a.json]\n",
		"auths suffix": "api-keys:\n  - key: k\n    auths_: [a.json]\n",
		"extra":        "api-keys:\n  - key: k\n    auths: [a.json]\n    note: x\n",
	} {
		t.Run("yaml "+name, func(t *testing.T) {
			var cfg SDKConfig
			err := yaml.Unmarshal([]byte(data), &cfg)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("err = %v, want an unknown field error", err)
			}
		})
	}
}

func TestAPIKeyEntryJSONRejectsEmptyOrBlankAuths(t *testing.T) {
	for name, data := range map[string]string{
		"empty list": `{"key":"k","auths":[]}`,
		"null":       `{"key":"k","auths":null}`,
		"blank item": `{"key":"k","auths":["  "]}`,
		"mixed":      `{"key":"k","auths":["a.json",""]}`,
		"blank key":  `{"key":" ","auths":["a.json"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var e APIKeyEntry
			if err := json.Unmarshal([]byte(data), &e); err == nil {
				t.Fatalf("accepted %s as %#v", data, e)
			}
		})
	}
	var ok APIKeyEntry
	if err := json.Unmarshal([]byte(`{"key":"k"}`), &ok); err != nil || ok.Bound() {
		t.Fatalf("mapping without auths: %#v, %v", ok, err)
	}
}

func TestValidateAPIKeysRejectsDuplicates(t *testing.T) {
	for name, keys := range map[string][]APIKeyEntry{
		"plain then bound": {{Key: "k"}, {Key: "k", Auths: []string{"a.json"}}},
		"bound then plain": {{Key: "k", Auths: []string{"a.json"}}, {Key: "k"}},
		"padded copy":      {{Key: "k"}, {Key: " k "}},
	} {
		cfg := SDKConfig{APIKeys: keys}
		if err := cfg.ValidateAPIKeys(); err == nil {
			t.Fatalf("%s: duplicate accepted", name)
		}
	}
	ok := SDKConfig{APIKeys: []APIKeyEntry{{Key: "a"}, {Key: "b", Auths: []string{"x.json"}}}}
	if err := ok.ValidateAPIKeys(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAPIKeysRejectsEmptyAuthsThatWouldBecomeUnrestricted(t *testing.T) {
	for name, entry := range map[string]APIKeyEntry{
		"empty non-nil list": {Key: "k", Auths: []string{}},
		"blank item":         {Key: "k", Auths: []string{"a.json", "  "}},
	} {
		cfg := SDKConfig{APIKeys: []APIKeyEntry{entry}}
		if err := cfg.ValidateAPIKeys(); err == nil {
			t.Fatalf("%s: accepted, but it serializes as an unrestricted key", name)
		}
	}
	// Nil auths is the plain unrestricted key and stays valid.
	plain := SDKConfig{APIKeys: []APIKeyEntry{{Key: "k"}}}
	if err := plain.ValidateAPIKeys(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRejectsDuplicateAPIKeys(t *testing.T) {
	for name, data := range map[string]string{
		"legacy": "api-keys:\n  - k\n  - key: k\n    auths: [a.json]\n",
		"v8":     "access:\n  api-keys:\n    - key: k\n      auths: [a.json]\n    - k\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("duplicate api-keys loaded")
			}
			if _, err := ParseConfigBytes([]byte(data)); err == nil {
				t.Fatal("duplicate api-keys parsed")
			}
		})
	}
}
