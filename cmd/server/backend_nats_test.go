package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// storePersister mirrors what the watcher requires of a remote store.
type storePersister interface {
	PersistConfig(ctx context.Context) error
	PersistAuthFiles(ctx context.Context, message string, paths ...string) error
}

func TestNatsBackendIsSelectedByURL(t *testing.T) {
	if selectBackend(backendEnv{lookup: envLookup(map[string]string{"NATS_CREDS": "x"}), workDir: t.TempDir()}) != nil {
		t.Fatal("selected nats without NATS_URL")
	}
	if selectBackend(backendEnv{lookup: envLookup(map[string]string{"NATS_URL": "nats://127.0.0.1:1"}), workDir: t.TempDir()}) == nil {
		t.Fatal("NATS_URL did not select a backend")
	}
	both := envLookup(map[string]string{"PGSTORE_DSN": "postgres://example.invalid/db", "NATS_URL": "nats://127.0.0.1:1"})
	var got string
	for _, backend := range storeBackends {
		if backend.detect(backendEnv{lookup: both, workDir: t.TempDir()}) != nil {
			got = backend.name
			break
		}
	}
	if got != "postgres" {
		t.Fatalf("first selected backend = %q, want postgres to keep priority", got)
	}
}

func TestNatsBackendSharesCredentialsAcrossReplicas(t *testing.T) {
	srv := natstest.Start(t)
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "config.example.yaml"), []byte("port: 8317\nhost: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	openReplica := func() *openedBackend {
		t.Helper()
		env := backendEnv{
			workDir: workDir,
			lookup: envLookup(map[string]string{
				"NATS_URL":              srv.URL,
				"NATS_STORE_PREFIX":     "e2e",
				"NATS_STORE_CREATE":     "auto",
				"NATS_STORE_LOCAL_PATH": t.TempDir(),
			}),
		}
		open := selectBackend(env)
		if open == nil {
			t.Fatal("nats backend was not selected")
		}
		opened, err := open()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = opened.tokenStore.(interface{ Close() error }).Close() })
		return opened
	}

	first := openReplica()
	cfg, err := loadBackendConfig(first, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8317 || cfg.AuthDir != first.authDir {
		t.Fatalf("config = port %d, auth dir %q", cfg.Port, cfg.AuthDir)
	}
	if _, ok := first.tokenStore.(storePersister); !ok {
		t.Fatal("token store does not implement the watcher persister")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	auth := &coreauth.Auth{
		ID: "team/user@example.com.json", FileName: "team/user@example.com.json", Provider: "test",
		Metadata: map[string]any{"type": "test", "email": "user@example.com", "access_token": "tok-e2e"},
	}
	if _, err := first.tokenStore.Save(ctx, auth); err != nil {
		t.Fatal(err)
	}

	second := openReplica()
	if second.authDir == first.authDir {
		t.Fatal("replicas share a spool")
	}
	auths, err := second.tokenStore.List(ctx)
	if err != nil || len(auths) != 1 || auths[0].ID != "team/user@example.com.json" || auths[0].Metadata["access_token"] != "tok-e2e" {
		t.Fatalf("second replica List = %v, %v", auths, err)
	}
	if _, err := os.Stat(filepath.Join(second.authDir, "team", "user@example.com.json")); err != nil {
		t.Fatalf("credential was not mirrored into the second spool: %v", err)
	}

	// An edit that reaches the first spool, as the management API would make it, goes back to the buckets.
	edited := filepath.Join(first.authDir, "team", "user@example.com.json")
	if err := os.WriteFile(edited, []byte(`{"type":"test","email":"user@example.com","access_token":"tok-e2e","note":"edited"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.tokenStore.(storePersister).PersistAuthFiles(ctx, "", edited); err != nil {
		t.Fatal(err)
	}
	third := openReplica()
	got, err := os.ReadFile(filepath.Join(third.authDir, "team", "user@example.com.json"))
	if err != nil || !strings.Contains(string(got), `"note":"edited"`) {
		t.Fatalf("third replica spool = %s, %v", got, err)
	}
}
