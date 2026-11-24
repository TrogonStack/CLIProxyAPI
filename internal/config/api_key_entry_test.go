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
