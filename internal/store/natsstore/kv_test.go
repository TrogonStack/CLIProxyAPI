package natsstore

import (
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func openAuthKV(t *testing.T) *KV {
	t.Helper()
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings := autoSettings("kvtest")
	boot, err := Bootstrap(testContext(t), nc, settings)
	if err != nil {
		t.Fatal(err)
	}
	return NewKV(settings.BucketName(BucketAuth), boot.Bucket(settings, BucketAuth))
}

func TestKVCompareAndSwap(t *testing.T) {
	kv := openAuthKV(t)
	ctx := testContext(t)
	const id = "sub/dir/a@b.com.json"

	if _, err := kv.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	rev1, err := kv.Create(ctx, id, []byte(`{"v":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Create(ctx, id, []byte(`{"v":2}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second Create = %v, want conflict", err)
	}
	rev2, err := kv.Update(ctx, id, []byte(`{"v":2}`), rev1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = kv.Update(ctx, id, []byte(`{"v":3}`), rev1)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.ID != id || conflict.Bucket != "kvtest-auth" {
		t.Fatalf("stale Update = %v, want ConflictError for %s", err, id)
	}
	if err := kv.Delete(ctx, id, rev1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Delete = %v, want conflict", err)
	}
	got, err := kv.Get(ctx, id)
	if err != nil || string(got.Value) != `{"v":2}` || got.Revision != rev2 || got.ID != id {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if err := kv.Delete(ctx, id, rev2); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if _, err := kv.Update(ctx, id, []byte(`{}`), rev2); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update after delete = %v, want conflict", err)
	}
	if _, err := kv.Create(ctx, id, []byte(`{"v":4}`)); err != nil {
		t.Fatalf("Create over a delete marker: %v", err)
	}
}

func TestKVListSkipsDeletedAndForeignKeys(t *testing.T) {
	kv := openAuthKV(t)
	ctx := testContext(t)

	empty, err := kv.List(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("List empty = %v, %v", empty, err)
	}
	ids := []string{"b.json", "a/c.json", "gone.json"}
	revs := map[string]uint64{}
	for _, id := range ids {
		rev, err := kv.Create(ctx, id, []byte(id))
		if err != nil {
			t.Fatal(err)
		}
		revs[id] = rev
	}
	if err := kv.Delete(ctx, "gone.json", revs["gone.json"]); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.kv.Create(ctx, "plain", []byte("foreign")); err != nil {
		t.Fatal(err)
	}
	entries, err := kv.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, entry := range entries {
		got[entry.ID] = string(entry.Value)
	}
	if len(got) != 2 || got["b.json"] != "b.json" || got["a/c.json"] != "a/c.json" {
		t.Fatalf("List = %v", got)
	}
}
