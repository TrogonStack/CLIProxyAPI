package main

import (
	"strings"
	"testing"
)

func envLookup(values map[string]string) func(keys ...string) (string, bool) {
	return func(keys ...string) (string, bool) {
		for _, key := range keys {
			if value := strings.TrimSpace(values[key]); value != "" {
				return value, true
			}
		}
		return "", false
	}
}

func TestSelectBackendReturnsNilWithoutSettings(t *testing.T) {
	if open := selectBackend(backendEnv{lookup: envLookup(nil), workDir: t.TempDir()}); open != nil {
		t.Fatal("selected a backend without any settings")
	}
}

func TestSelectBackendPriority(t *testing.T) {
	all := map[string]string{
		"PGSTORE_DSN":           "postgres://example.invalid/db",
		"OBJECTSTORE_ENDPOINT":  "http://127.0.0.1:1",
		"GITSTORE_GIT_URL":      "https://example.invalid/repo.git",
		"gitstore_git_username": "ignored",
	}
	tests := []struct {
		name string
		drop []string
		want string
	}{
		{"postgres first", nil, "postgres"},
		{"object next", []string{"PGSTORE_DSN"}, "object"},
		{"git last", []string{"PGSTORE_DSN", "OBJECTSTORE_ENDPOINT"}, "git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range all {
				env[k] = v
			}
			for _, k := range tt.drop {
				delete(env, k)
			}
			be := backendEnv{lookup: envLookup(env), workDir: t.TempDir()}
			var got string
			for _, backend := range storeBackends {
				if backend.detect(be) != nil {
					got = backend.name
					break
				}
			}
			if got != tt.want {
				t.Fatalf("first selected backend = %q, want %q", got, tt.want)
			}
			if selectBackend(be) == nil {
				t.Fatal("selectBackend returned nil")
			}
		})
	}
}

func TestSelectBackendAcceptsLowercaseKeys(t *testing.T) {
	be := backendEnv{lookup: envLookup(map[string]string{"pgstore_dsn": "postgres://example.invalid/db"}), workDir: t.TempDir()}
	if selectBackend(be) == nil {
		t.Fatal("lowercase key not honored")
	}
}

func TestObjectBackendRejectsBadEndpoints(t *testing.T) {
	tests := map[string]string{
		"ftp://host/bucket": "unsupported object store scheme",
		"http://":           "missing host",
	}
	for endpoint, want := range tests {
		be := backendEnv{
			lookup:  envLookup(map[string]string{"OBJECTSTORE_ENDPOINT": endpoint}),
			workDir: t.TempDir(),
		}
		open := selectBackend(be)
		if open == nil {
			t.Fatalf("%q: backend not selected", endpoint)
		}
		if _, err := open(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err = %v, want %q", endpoint, err, want)
		}
	}
}

func TestSpoolBaseFallbacks(t *testing.T) {
	be := backendEnv{workDir: "/work", writableBase: "/writable"}
	if got := be.spoolBase("/configured"); got != "/configured" {
		t.Fatalf("configured = %q", got)
	}
	if got := be.spoolBase(""); got != "/writable" {
		t.Fatalf("writable = %q", got)
	}
	be.writableBase = ""
	if got := be.spoolBase(""); got != "/work" {
		t.Fatalf("work dir = %q", got)
	}
}
