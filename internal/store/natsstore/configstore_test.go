package natsstore

import (
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func newConfigStores(t *testing.T, n int) []*ConfigStore {
	t.Helper()
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings := autoSettings("cfgtest")
	boot, err := Bootstrap(testContext(t), nc, settings)
	if err != nil {
		t.Fatal(err)
	}
	kv := NewKV(settings.BucketName(BucketConfig), boot.Bucket(settings, BucketConfig))
	stores := make([]*ConfigStore, n)
	for i := range stores {
		stores[i] = NewConfigStore(kv)
	}
	return stores
}

func TestConfigStoreReadWrite(t *testing.T) {
	store := newConfigStores(t, 1)[0]
	ctx := testContext(t)

	if _, _, err := store.Read(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read before any write = %v, want ErrNotFound", err)
	}
	rev1, err := store.Write(ctx, []byte("port: 8317\r\nhost: \"\"\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	data, rev, err := store.Read(ctx)
	if err != nil || rev != rev1 || string(data) != "port: 8317\nhost: \"\"\n" {
		t.Fatalf("Read = %q, %d, %v", data, rev, err)
	}
	rev2, err := store.Write(ctx, []byte("port: 9000\n"))
	if err != nil || rev2 <= rev1 || store.Revision() != rev2 {
		t.Fatalf("second Write = %d, %v (revision %d)", rev2, err, store.Revision())
	}
}

func TestConfigStoreConflictsInsteadOfOverwriting(t *testing.T) {
	stores := newConfigStores(t, 2)
	a, b := stores[0], stores[1]
	ctx := testContext(t)

	if _, err := a.Write(ctx, []byte("v: 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(ctx, []byte("v: 2\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, []byte("v: 3\n")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Write = %v, want conflict", err)
	}
	data, _, err := a.Read(ctx)
	if err != nil || string(data) != "v: 2\n" {
		t.Fatalf("Read = %q, %v; the newer config must survive", data, err)
	}
	if _, err := a.Write(ctx, []byte("v: 3\n")); err != nil {
		t.Fatalf("Write after re-reading: %v", err)
	}
}

func TestConfigStoreFirstWriteRaceConflicts(t *testing.T) {
	stores := newConfigStores(t, 2)
	ctx := testContext(t)

	if _, err := stores[0].Write(ctx, []byte("a: 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stores[1].Write(ctx, []byte("b: 1\n")); !errors.Is(err, ErrConflict) {
		t.Fatalf("second creator = %v, want conflict", err)
	}
}

func TestConfigStoreStoresEmptyConfig(t *testing.T) {
	store := newConfigStores(t, 1)[0]
	ctx := testContext(t)

	if _, err := store.Write(ctx, nil); err != nil {
		t.Fatal(err)
	}
	data, _, err := store.Read(ctx)
	if err != nil || len(data) != 0 {
		t.Fatalf("Read = %q, %v", data, err)
	}
}
