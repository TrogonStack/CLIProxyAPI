package natsstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// waitFor polls until cond holds, failing the test with msg on timeout.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

func fileEquals(path, want string) bool {
	got, err := os.ReadFile(path)
	return err == nil && string(got) == want
}

func fileMissing(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// syncedPair returns two bootstrapped replicas that follow the buckets.
func syncedPair(t *testing.T) (a, b *Store, f *storeFixture) {
	t.Helper()
	f = newStoreFixture(t)
	a, b = f.open(), f.open()
	for _, s := range []*Store{a, b} {
		if err := s.Bootstrap(testContext(t), f.example); err != nil {
			t.Fatal(err)
		}
		s.StartSync(context.Background())
		t.Cleanup(func() { _ = s.Close() })
	}
	return a, b, f
}

func TestAuthSyncPropagatesSavesAndDeletes(t *testing.T) {
	a, b, _ := syncedPair(t)
	ctx := testContext(t)
	const id = "team/user@example.com.json"
	remote := filepath.Join(b.AuthDir(), "team", "user@example.com.json")
	local := filepath.Join(a.AuthDir(), "team", "user@example.com.json")

	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-1"})); err != nil {
		t.Fatal(err)
	}
	want := readFile(t, local)
	waitFor(t, "create to reach the other spool", func() bool { return fileEquals(remote, want) })

	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-2"})); err != nil {
		t.Fatal(err)
	}
	want = readFile(t, local)
	waitFor(t, "update to reach the other spool", func() bool { return fileEquals(remote, want) })

	if err := a.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delete to reach the other spool", func() bool { return fileMissing(remote) })
}

func TestAuthSyncDoesNotEchoBackIntoTheBucket(t *testing.T) {
	a, b, _ := syncedPair(t)
	ctx := testContext(t)
	const id = "echo.json"
	remote := filepath.Join(b.AuthDir(), id)

	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "create to reach the other spool", func() bool { _, err := os.Stat(remote); return err == nil })
	before, err := a.auth.kv.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// What the file watcher does when it sees the spool file the sync wrote.
	if err := b.PersistAuthFiles(ctx, "echo", remote); err != nil {
		t.Fatal(err)
	}
	// The writer's own change comes back through its watch and must not rewrite the file.
	info, err := os.Stat(filepath.Join(a.AuthDir(), id))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := a.auth.kv.Get(ctx, id)
	if err != nil || after.Revision != before.Revision {
		t.Fatalf("revision moved from %d to %d (%v), want no write back", before.Revision, after.Revision, err)
	}
	again, err := os.Stat(filepath.Join(a.AuthDir(), id))
	if err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatalf("writer's own file was rewritten (%v)", err)
	}

	// A remote delete removes the file; persisting that removal must not delete again.
	if err := a.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delete to reach the other spool", func() bool { return fileMissing(remote) })
	// The credential comes back while b has not applied it yet, and the
	// persister handles the removal of the file the earlier delete left
	// behind: the new credential must survive.
	b.stopSync()
	if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": "tok-new"})); err != nil {
		t.Fatal(err)
	}
	if err := b.PersistAuthFiles(ctx, "stale removal", remote); err != nil {
		t.Fatal(err)
	}
	if _, err := a.auth.kv.Get(ctx, id); err != nil {
		t.Fatalf("recreated credential was deleted: %v", err)
	}
}

func TestAuthSyncCatchesUpOnStart(t *testing.T) {
	f := newStoreFixture(t)
	ctx := testContext(t)
	a, b := f.open(), f.open()
	for _, s := range []*Store{a, b} {
		if err := s.Bootstrap(ctx, f.example); err != nil {
			t.Fatal(err)
		}
	}
	// Changes between b's bootstrap and its watch starting must still arrive.
	if _, err := a.Save(ctx, testAuth("late.json", map[string]any{"type": "test", "access_token": "tok"})); err != nil {
		t.Fatal(err)
	}
	b.StartSync(context.Background())
	t.Cleanup(func() { _ = b.Close() })
	want := readFile(t, filepath.Join(a.AuthDir(), "late.json"))
	waitFor(t, "catch-up", func() bool { return fileEquals(filepath.Join(b.AuthDir(), "late.json"), want) })
}

func TestAuthSyncResumesAfterTheWatchDrops(t *testing.T) {
	old := watchRetryDelay
	watchRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { watchRetryDelay = old })

	f := newStoreFixture(t)
	ctx := testContext(t)
	a, b := f.open(), f.open()
	watches := make(chan jetstream.KeyWatcher, 8)
	b.auth.kv.onWatch = func(w jetstream.KeyWatcher) { watches <- w }
	for _, s := range []*Store{a, b} {
		if err := s.Bootstrap(ctx, f.example); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"keep.json", "gone.json"} {
		if _, err := a.Save(ctx, testAuth(id, map[string]any{"type": "test", "access_token": id})); err != nil {
			t.Fatal(err)
		}
	}
	b.StartSync(context.Background())
	t.Cleanup(func() { _ = b.Close() })
	first := <-watches
	waitFor(t, "initial replay", func() bool {
		return !fileMissing(filepath.Join(b.AuthDir(), "keep.json")) && !fileMissing(filepath.Join(b.AuthDir(), "gone.json"))
	})

	// While b's watch is down: a change, a delete and a new credential.
	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Save(ctx, testAuth("keep.json", map[string]any{"type": "test", "access_token": "changed"})); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, "gone.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Save(ctx, testAuth("fresh.json", map[string]any{"type": "test", "access_token": "fresh"})); err != nil {
		t.Fatal(err)
	}

	select {
	case <-watches:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch was not reopened")
	}
	for _, id := range []string{"keep.json", "fresh.json"} {
		want := readFile(t, filepath.Join(a.AuthDir(), id))
		waitFor(t, id+" to converge", func() bool { return fileEquals(filepath.Join(b.AuthDir(), id), want) })
	}
	waitFor(t, "delete made while down", func() bool { return fileMissing(filepath.Join(b.AuthDir(), "gone.json")) })
}

// unitAuthStore has no bucket; it only exercises how watched changes reach the spool.
func unitAuthStore(t *testing.T) *AuthStore {
	t.Helper()
	store, err := NewAuthStore(NewKV("unit-auth", nil, WithOrigin("me")), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func change(id string, rev uint64, origin, value string) Change {
	return Change{Entry: Entry{ID: id, Value: []byte(value), Revision: rev, Origin: origin}}
}

func TestApplyChangeOrdersByRevision(t *testing.T) {
	store := unitAuthStore(t)
	path := filepath.Join(store.AuthDir(), "a.json")

	for _, c := range []Change{
		change("a.json", 5, "other", "five"),
		change("a.json", 3, "other", "three"), // replayed older revision
		change("a.json", 5, "other", "again"), // duplicate delivery
	} {
		if err := store.applyChange(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := readFile(t, path); got != "five" {
		t.Fatalf("file = %q, want the highest revision only", got)
	}

	// A delete is ordered too, and a stale put cannot resurrect the file.
	del := change("a.json", 7, "", "")
	del.Deleted = true
	if err := store.applyChange(del); err != nil {
		t.Fatal(err)
	}
	if err := store.applyChange(change("a.json", 6, "other", "six")); err != nil {
		t.Fatal(err)
	}
	if !fileMissing(path) {
		t.Fatal("a put older than the delete brought the file back")
	}
	if store.revisions["a.json"] != 0 {
		t.Fatalf("revision kept after delete: %d", store.revisions["a.json"])
	}
}

func TestApplyChangeLeavesOwnWritesAndIdenticalFilesAlone(t *testing.T) {
	store := unitAuthStore(t)
	path := filepath.Join(store.AuthDir(), "a.json")
	if err := os.WriteFile(path, []byte("local edit"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The bucket echoing this replica's write must not replace a newer local edit.
	if err := store.applyChange(change("a.json", 2, "me", "stored")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "local edit" {
		t.Fatalf("own write rewrote the spool: %q", got)
	}
	if store.revisions["a.json"] != 2 {
		t.Fatalf("revision = %d, want it advanced so the next save checks against it", store.revisions["a.json"])
	}

	// A remote value equal to the file produces no write, so no file event.
	if err := os.WriteFile(path, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	if err := store.applyChange(change("a.json", 3, "other", "same")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.ModTime().After(past.Add(time.Minute)) {
		t.Fatalf("identical content was rewritten (%v)", err)
	}
}

func TestApplyChangeRefusesPathsOutsideTheSpool(t *testing.T) {
	store := unitAuthStore(t)
	if err := store.applyChange(change("../escape.json", 1, "other", "x")); err == nil {
		t.Fatal("a credential id escaping the spool was applied")
	}
}

func TestPruneForgetsOnlyCredentialsOlderThanTheReplay(t *testing.T) {
	store := unitAuthStore(t)
	for id, rev := range map[string]uint64{"live.json": 4, "gone.json": 3, "newer.json": 9} {
		if err := store.applyChange(change(id, rev, "other", id)); err != nil {
			t.Fatal(err)
		}
	}
	store.prune(map[string]struct{}{"live.json": {}}, 5)

	if fileMissing(filepath.Join(store.AuthDir(), "live.json")) {
		t.Fatal("a credential in the replay was pruned")
	}
	if !fileMissing(filepath.Join(store.AuthDir(), "gone.json")) {
		t.Fatal("a credential absent from the replay was kept")
	}
	if fileMissing(filepath.Join(store.AuthDir(), "newer.json")) {
		t.Fatal("a credential written after the replay was pruned")
	}
}
