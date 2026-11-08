package natsstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// newAuthStores opens n stores over one auth bucket, each with its own spool directory.
func newAuthStores(t *testing.T, n int) (stores []*AuthStore, kv *KV) {
	t.Helper()
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings := autoSettings("authtest")
	boot, err := Bootstrap(testContext(t), nc, settings)
	if err != nil {
		t.Fatal(err)
	}
	kv = NewKV(settings.BucketName(BucketAuth), boot.Bucket(settings, BucketAuth))
	for range n {
		store, err := NewAuthStore(kv, filepath.Join(t.TempDir(), "auths"))
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
	}
	return stores, kv
}

func testAuth(id string, metadata map[string]any) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, FileName: id, Provider: "test", Metadata: metadata}
}

func storedMetadata(t *testing.T, kv *KV, id string) map[string]any {
	t.Helper()
	entry, err := kv.Get(testContext(t), id)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(entry.Value, &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestAuthStoreSaveListDelete(t *testing.T) {
	stores, kv := newAuthStores(t, 2)
	writer, reader := stores[0], stores[1]
	ctx := testContext(t)

	auth := testAuth("team/user@example.com.json", map[string]any{
		"type": "test", "email": "user@example.com", "access_token": "tok-1",
	})
	path, err := writer.Save(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(writer.AuthDir(), "team", "user@example.com.json"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	if auth.Attributes[cliproxyauth.AttributeSourceBackend] != cliproxyauth.AuthSourceNATS {
		t.Fatalf("source backend = %q", auth.Attributes[cliproxyauth.AttributeSourceBackend])
	}
	if got := storedMetadata(t, kv, "team/user@example.com.json"); got["access_token"] != "tok-1" {
		t.Fatalf("stored = %v", got)
	}

	auths, err := reader.List(ctx)
	if err != nil || len(auths) != 1 {
		t.Fatalf("List = %v, %v", auths, err)
	}
	got := auths[0]
	if got.ID != "team/user@example.com.json" || got.Provider != "test" || got.Label != "user@example.com" ||
		got.Metadata["access_token"] != "tok-1" || got.Attributes[cliproxyauth.AttributePath] != filepath.Join(reader.AuthDir(), "team", "user@example.com.json") {
		t.Fatalf("listed auth = %+v", got)
	}

	if err := writer.Delete(ctx, "team/user@example.com.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool file survived delete: %v", err)
	}
	if _, err := kv.Get(ctx, "team/user@example.com.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stored value survived delete: %v", err)
	}
	if err := writer.Delete(ctx, "team/user@example.com.json"); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestAuthStoreSaveMergesConcurrentUserEdit(t *testing.T) {
	stores, kv := newAuthStores(t, 2)
	a, b := stores[0], stores[1]
	ctx := testContext(t)
	const id = "shared.json"

	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := b.List(ctx); err != nil {
		t.Fatal(err)
	}
	// b edits a user field and rotates the token before a writes again.
	if _, err := b.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-b", "note": "pinned"})); err != nil {
		t.Fatal(err)
	}
	// a still holds the older revision and refreshed its token.
	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-a"})); err != nil {
		t.Fatalf("save after losing the race: %v", err)
	}
	got := storedMetadata(t, kv, id)
	if got["access_token"] != "tok-a" || got["note"] != "pinned" {
		t.Fatalf("merged = %v, want a's token and b's note", got)
	}
	onDisk, err := os.ReadFile(filepath.Join(a.AuthDir(), id))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(onDisk, &disk); err != nil || disk["note"] != "pinned" {
		t.Fatalf("spool file = %s, %v", onDisk, err)
	}
}

func TestAuthStoreCreateRaceMerges(t *testing.T) {
	stores, kv := newAuthStores(t, 2)
	a, b := stores[0], stores[1]
	ctx := testContext(t)
	const id = "race.json"

	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-a", "note": "first"})); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-b"})); err != nil {
		t.Fatal(err)
	}
	got := storedMetadata(t, kv, id)
	if got["access_token"] != "tok-b" || got["note"] != "first" {
		t.Fatalf("merged = %v", got)
	}
}

func TestAuthStoreDisabledSaveDoesNotResurrect(t *testing.T) {
	stores, kv := newAuthStores(t, 1)
	store := stores[0]
	ctx := testContext(t)

	auth := testAuth("gone.json", map[string]any{"type": "test"})
	auth.Disabled = true
	if path, err := store.Save(ctx, auth); err != nil || path != "" {
		t.Fatalf("runtime save of a missing disabled auth = %q, %v", path, err)
	}
	if _, err := kv.Get(ctx, "gone.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled auth was recreated: %v", err)
	}
	if _, err := store.Save(cliproxyauth.WithAuthCreationIntent(ctx), auth); err != nil {
		t.Fatal(err)
	}
	if got := storedMetadata(t, kv, "gone.json"); got["disabled"] != true {
		t.Fatalf("stored = %v", got)
	}
}

func TestAuthStoreRemoteDeleteDropsRuntimeUpdate(t *testing.T) {
	stores, kv := newAuthStores(t, 2)
	a, b := stores[0], stores[1]
	ctx := testContext(t)
	const id = "removed.json"

	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-1"})); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if path, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-2"})); err != nil || path != "" {
		t.Fatalf("save of a removed auth = %q, %v", path, err)
	}
	if _, err := kv.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed auth was resurrected: %v", err)
	}
}

func TestAuthStoreRejectsPathsOutsideTheSpool(t *testing.T) {
	stores, kv := newAuthStores(t, 1)
	store := stores[0]
	ctx := testContext(t)

	outside := testAuth("x.json", map[string]any{"type": "test"})
	outside.Attributes = map[string]string{cliproxyauth.AttributePath: filepath.Join(filepath.Dir(store.AuthDir()), "x.json")}
	if _, err := store.Save(ctx, outside); err == nil {
		t.Fatal("saved a path outside the auth directory")
	}
	if err := store.Delete(ctx, "../x.json"); err == nil {
		t.Fatal("deleted a path outside the auth directory")
	}

	if _, err := kv.Create(ctx, "../escape.json", []byte(`{"type":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Create(ctx, "bad.json", []byte(`not json`)); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Create(ctx, "ok.json", []byte(`{"type":"test"}`)); err != nil {
		t.Fatal(err)
	}
	auths, err := store.List(ctx)
	if err != nil || len(auths) != 1 || auths[0].ID != "ok.json" {
		t.Fatalf("List = %v, %v", auths, err)
	}
}

func TestAuthStoreSaveKeepsErrorsFreeOfCredentials(t *testing.T) {
	stores, _ := newAuthStores(t, 1)
	store := stores[0]
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	_, err := store.Save(ctx, testAuth("s.json", map[string]any{"type": "test", "access_token": "tok-secret-value"}))
	if err == nil {
		t.Fatal("save with a canceled context succeeded")
	}
	if msg := err.Error(); len(msg) == 0 || strings.Contains(msg, "tok-secret-value") {
		t.Fatalf("error leaks the credential: %v", err)
	}
}
