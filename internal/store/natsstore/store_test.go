package natsstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

// storeFixture is one NATS server that several stores, each with its own spool, share.
type storeFixture struct {
	t        *testing.T
	url      string
	settings StoreSettings
	example  string
}

func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()
	srv := natstest.Start(t)
	example := filepath.Join(t.TempDir(), "config.example.yaml")
	if err := os.WriteFile(example, []byte("port: 8317\nhost: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &storeFixture{t: t, url: srv.URL, settings: autoSettings("spooltest"), example: example}
}

// open connects a new replica with an empty spool.
func (f *storeFixture) open() *Store {
	f.t.Helper()
	nc, err := connect(f.t, map[string]string{EnvURL: f.url}, 5*time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.wrap(nc)
}

func (f *storeFixture) wrap(nc *nats.Conn) *Store {
	f.t.Helper()
	boot, err := Bootstrap(testContext(f.t), nc, f.settings)
	if err != nil {
		f.t.Fatal(err)
	}
	store, err := NewStore(nc, boot, f.settings, f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	return store
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStoreBootstrapSeedsConfigOnce(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	first, second := f.open(), f.open()

	if err := first.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, first.ConfigPath()); got != "port: 8317\nhost: \"\"\n" {
		t.Fatalf("seeded config = %q", got)
	}
	// A different template must not replace what is already stored.
	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte("port: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := second.Bootstrap(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, second.ConfigPath()); got != "port: 8317\nhost: \"\"\n" {
		t.Fatalf("second replica config = %q, want the stored one", got)
	}
	rev := first.config.Revision()
	if err := first.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}
	if first.config.Revision() != rev {
		t.Fatalf("repeat bootstrap rewrote the config: revision %d -> %d", rev, first.config.Revision())
	}
}

func TestStoreBootstrapConcurrentSeedsConverge(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	stores := []*Store{f.open(), f.open(), f.open(), f.open()}

	var wg sync.WaitGroup
	errs := make([]error, len(stores))
	for i, store := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = store.Bootstrap(ctx, f.example)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	for i, store := range stores {
		if got := readFile(t, store.ConfigPath()); got != "port: 8317\nhost: \"\"\n" {
			t.Fatalf("replica %d config = %q", i, got)
		}
	}
}

func TestStoreBootstrapRebuildsAuthSpool(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	writer, reader := f.open(), f.open()
	if err := writer.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Save(ctx, testAuth("team/a.json", map[string]any{"type": "test", "access_token": "tok-a"})); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(reader.AuthDir(), "stale.json")
	if err := os.WriteFile(stale, []byte(`{"type":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reader.Bootstrap(ctx, f.example); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale spool file survived bootstrap: %v", err)
		}
		if got := readFile(t, filepath.Join(reader.AuthDir(), "team", "a.json")); got != readFile(t, filepath.Join(writer.AuthDir(), "team", "a.json")) {
			t.Fatalf("mirrored file = %q", got)
		}
		info, err := os.Stat(filepath.Join(reader.AuthDir(), "team", "a.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mirrored file mode = %v, %v", info, err)
		}
	}
	auths, err := reader.List(ctx)
	if err != nil || len(auths) != 1 || auths[0].ID != "team/a.json" {
		t.Fatalf("List = %v, %v", auths, err)
	}
}

func TestStorePersistAuthFiles(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	store, other := f.open(), f.open()
	if err := store.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(store.AuthDir(), "uploaded.json")
	if err := os.WriteFile(path, []byte(`{"type":"test","access_token":"tok-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistAuthFiles(ctx, "", path); err != nil {
		t.Fatal(err)
	}
	entry, err := store.auth.kv.Get(ctx, "uploaded.json")
	if err != nil || string(entry.Value) != `{"type":"test","access_token":"tok-1"}` {
		t.Fatalf("stored = %+v, %v", entry, err)
	}
	// The watcher echoing the same file must not write a new revision.
	if err := store.PersistAuthFiles(ctx, "", path); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.auth.kv.Get(ctx, "uploaded.json"); again.Revision != entry.Revision {
		t.Fatalf("echo wrote revision %d over %d", again.Revision, entry.Revision)
	}

	if err := os.WriteFile(path, []byte(`{"type":"test","access_token":"tok-2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistAuthFiles(ctx, "", path); err != nil {
		t.Fatal(err)
	}
	if changed, _ := store.auth.kv.Get(ctx, "uploaded.json"); string(changed.Value) != `{"type":"test","access_token":"tok-2"}` {
		t.Fatalf("edit not stored: %s", changed.Value)
	}

	// A replica holding an older revision must not overwrite the newer value.
	if err := other.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistAuthFiles(ctx, "", path); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(other.AuthDir(), "uploaded.json")
	if err := os.WriteFile(stale, []byte(`{"type":"test","access_token":"tok-stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.auth.Delete(ctx, "uploaded.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.auth.Save(ctx, testAuth("uploaded.json", map[string]any{"type": "test", "access_token": "tok-3"})); err != nil {
		t.Fatal(err)
	}
	if err := other.PersistAuthFiles(ctx, "", stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale persist = %v, want conflict", err)
	}
	if kept, _ := store.auth.kv.Get(ctx, "uploaded.json"); string(kept.Value) == `{"type":"test","access_token":"tok-stale"}` {
		t.Fatal("stale file replaced newer credentials")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistAuthFiles(ctx, "", path, filepath.Join(t.TempDir(), "outside.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.auth.kv.Get(ctx, "uploaded.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed file left its credential behind: %v", err)
	}
}

func TestStorePersistConfig(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	store, other := f.open(), f.open()
	if err := store.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}
	if err := other.Bootstrap(ctx, f.example); err != nil {
		t.Fatal(err)
	}
	rev := store.config.Revision()

	// The watcher echoing the bootstrap write stores nothing.
	if err := store.PersistConfig(ctx); err != nil || store.config.Revision() != rev {
		t.Fatalf("echo persist: %v, revision %d -> %d", err, rev, store.config.Revision())
	}
	if err := os.WriteFile(store.ConfigPath(), []byte("port: 9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistConfig(ctx); err != nil {
		t.Fatal(err)
	}
	data, _, err := f.open().config.Read(ctx)
	if err != nil || string(data) != "port: 9000\n" {
		t.Fatalf("stored config = %q, %v", data, err)
	}

	// A stale replica is told it lost.
	if err := os.WriteFile(other.ConfigPath(), []byte("port: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := other.PersistConfig(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale persist = %v, want conflict", err)
	}

	// A missing or empty file never wipes the shared config.
	if err := os.WriteFile(store.ConfigPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	if err := store.PersistConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if data, _, err := f.open().config.Read(ctx); err != nil || string(data) != "port: 9000\n" {
		t.Fatalf("config after empty and missing files = %q, %v", data, err)
	}
}

func TestStoreSeedRefusesPlaintextManagementSecret(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	store := f.open()
	template := filepath.Join(t.TempDir(), "template.yaml")
	body := "remote-management:\n  secret-key: &s plain-secret-value\n  other: *s\n"
	if err := os.WriteFile(template, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, template); err == nil {
		t.Fatal("seeded a config that still holds a plaintext management secret")
	}
	if _, _, err := f.open().config.Read(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused config reached the bucket: %v", err)
	}
}
