package natsstore

import (
	"os"
	"testing"
	"time"
)

// editConfig is an operator editing the spool file and the watcher persisting it.
func editConfig(t *testing.T, s *Store, content string) {
	t.Helper()
	if err := os.WriteFile(s.ConfigPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistConfig(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

func TestConfigSyncPropagatesEditsBothWays(t *testing.T) {
	a, b, _ := syncedPair(t)

	editConfig(t, a, "port: 9001\n")
	waitFor(t, "a's edit to reach b", func() bool { return fileEquals(b.ConfigPath(), "port: 9001\n") })

	// b's edit builds on the revision its watch delivered, so it is not a conflict.
	editConfig(t, b, "port: 9002\n")
	waitFor(t, "b's edit to reach a", func() bool { return fileEquals(a.ConfigPath(), "port: 9002\n") })
}

func TestConfigSyncDoesNotEchoBackIntoTheBucket(t *testing.T) {
	a, b, _ := syncedPair(t)
	ctx := testContext(t)

	editConfig(t, a, "port: 9001\n")
	waitFor(t, "a's edit to reach b", func() bool { return fileEquals(b.ConfigPath(), "port: 9001\n") })
	before, err := a.config.kv.Get(ctx, configID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}

	// What the file watcher does after reloading the config the sync wrote.
	if err := b.PersistConfig(ctx); err != nil {
		t.Fatal(err)
	}
	// And a's own write coming back through its watch.
	time.Sleep(300 * time.Millisecond)
	after, err := a.config.kv.Get(ctx, configID)
	if err != nil || after.Revision != before.Revision {
		t.Fatalf("revision moved from %d to %d (%v), want no write back", before.Revision, after.Revision, err)
	}
	if again, err := os.Stat(a.ConfigPath()); err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatalf("writer's own config was rewritten (%v)", err)
	}
}

func TestConfigSyncKeepsAStaleReplicaFromOverwriting(t *testing.T) {
	a, b, _ := syncedPair(t)

	b.stopSync()
	editConfig(t, a, "port: 9001\n")
	// b missed the change, so its edit is checked against the old revision.
	if err := os.WriteFile(b.ConfigPath(), []byte("port: 9002\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.PersistConfig(testContext(t)); err == nil {
		t.Fatal("a stale replica overwrote the newer config")
	}
	stored, err := a.config.kv.Get(testContext(t), configID)
	if err != nil || string(stored.Value) != "port: 9001\n" {
		t.Fatalf("stored = %q, %v", stored.Value, err)
	}
}

func TestConfigObserveOrdersAndIgnoresDeletes(t *testing.T) {
	store := NewConfigStore(NewKV("unit-config", nil, WithOrigin("me")))
	put := func(rev uint64, origin, value string) Change {
		return Change{Entry: Entry{ID: configID, Value: []byte(value), Revision: rev, Origin: origin}}
	}

	if data, ok := store.observe(put(4, "other", "four")); !ok || string(data) != "four" || store.Revision() != 4 {
		t.Fatalf("remote put = %q, %v at %d", data, ok, store.Revision())
	}
	if _, ok := store.observe(put(3, "other", "three")); ok || store.Revision() != 4 {
		t.Fatalf("replayed older revision applied (revision %d)", store.Revision())
	}
	if _, ok := store.observe(put(4, "other", "again")); ok {
		t.Fatal("duplicate delivery applied twice")
	}
	// Own write: nothing for the spool, but later edits build on it.
	if _, ok := store.observe(put(5, "me", "mine")); ok || store.Revision() != 5 {
		t.Fatalf("own write = %v at %d", ok, store.Revision())
	}
	del := put(6, "", "")
	del.Deleted = true
	if _, ok := store.observe(del); ok {
		t.Fatal("a delete was mirrored, which would wipe the shared config")
	}
	if store.Revision() != 0 {
		t.Fatalf("revision after delete = %d, want 0 so the next write recreates it", store.Revision())
	}
}
