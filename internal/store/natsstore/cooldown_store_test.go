package natsstore

import (
	"context"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func cooldownRecord(authID, model string, updatedAt time.Time) cliproxyauth.CooldownStateRecord {
	return cliproxyauth.CooldownStateRecord{
		Provider:       "example",
		AuthID:         authID,
		Model:          model,
		Status:         "error",
		Reason:         "quota",
		NextRetryAfter: time.Now().Add(10 * time.Minute),
		UpdatedAt:      updatedAt,
	}
}

func loadCooldowns(t *testing.T, s *Store) map[string]cliproxyauth.CooldownStateRecord {
	t.Helper()
	records, err := s.CooldownStateStore().Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := make(map[string]cliproxyauth.CooldownStateRecord, len(records))
	for _, record := range records {
		byKey[record.AuthID+"/"+record.Model] = record
	}
	return byKey
}

func TestCooldownStateIsSharedBetweenReplicas(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)

	if err := a.CooldownStateStore().Save(context.Background(), []cliproxyauth.CooldownStateRecord{
		cooldownRecord("one", "model-a", at),
		cooldownRecord("one", "", at),
	}); err != nil {
		t.Fatal(err)
	}
	got := loadCooldowns(t, b)
	if len(got) != 2 {
		t.Fatalf("loaded %d records, want 2: %+v", len(got), got)
	}
	record := got["one/model-a"]
	if record.Reason != "quota" || !record.UpdatedAt.Equal(at) {
		t.Fatalf("record = %+v, want reason quota updated at %v", record, at)
	}
}

func TestCooldownSaveNeverReplacesANewerRecord(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx := context.Background()
	now := time.Now()

	newer := cooldownRecord("one", "m", now)
	newer.Reason = "newer"
	if err := a.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{newer}); err != nil {
		t.Fatal(err)
	}
	older := cooldownRecord("one", "m", now.Add(-time.Hour))
	older.Reason = "older"
	if err := b.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{older}); err != nil {
		t.Fatal(err)
	}
	if got := loadCooldowns(t, a)["one/m"]; got.Reason != "newer" {
		t.Fatalf("reason = %q, want the newer record to survive", got.Reason)
	}

	newest := cooldownRecord("one", "m", now.Add(time.Hour))
	newest.Reason = "newest"
	if err := b.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{newest}); err != nil {
		t.Fatal(err)
	}
	if got := loadCooldowns(t, a)["one/m"]; got.Reason != "newest" {
		t.Fatalf("reason = %q, want a newer write to replace the record", got.Reason)
	}
}

func TestCooldownSaveClearsRecordsItNoLongerHolds(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx := context.Background()
	at := time.Now().Add(-time.Minute)

	if err := a.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{cooldownRecord("one", "m", at), cooldownRecord("two", "m", at)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CooldownStateStore().Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{cooldownRecord("two", "m", at.Add(time.Second))}); err != nil {
		t.Fatal(err)
	}
	got := loadCooldowns(t, b)
	if _, ok := got["one/m"]; ok || len(got) != 1 {
		t.Fatalf("records = %+v, want only two/m", got)
	}

	// A replica that missed the clearing cannot bring the record back.
	if err := b.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{cooldownRecord("one", "m", at), cooldownRecord("two", "m", at.Add(time.Second))}); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCooldowns(t, a)["one/m"]; ok {
		t.Fatal("an older write resurrected a cleared record")
	}
}

func TestCooldownClearLeavesARecordAnotherReplicaWrote(t *testing.T) {
	f := newStoreFixture(t)
	a, b := f.open(), f.open()
	ctx := context.Background()
	now := time.Now()

	if err := a.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{cooldownRecord("one", "m", now.Add(-time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if err := b.CooldownStateStore().Save(ctx, []cliproxyauth.CooldownStateRecord{cooldownRecord("one", "m", now)}); err != nil {
		t.Fatal(err)
	}
	// a still remembers its own, older version and now drops it.
	if err := a.CooldownStateStore().Save(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCooldowns(t, b)["one/m"]; !ok {
		t.Fatal("clearing an old version removed a newer record")
	}
}

func TestCooldownSaveRejectsAnEmptyAuthID(t *testing.T) {
	f := newStoreFixture(t)
	if err := f.open().CooldownStateStore().Save(context.Background(), []cliproxyauth.CooldownStateRecord{{AuthID: " "}}); err == nil {
		t.Fatal("a record without an auth ID was saved")
	}
}

func TestCooldownTTLFollowsTheRetryTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		next time.Time
		want time.Duration
	}{
		{"future", now.Add(time.Hour), time.Hour + cooldownGrace},
		{"passed", now.Add(-time.Hour), cooldownGrace},
		{"unset", time.Time{}, cooldownIdleTTL},
	}
	for _, tc := range cases {
		got := cooldownTTL(cliproxyauth.CooldownStateRecord{NextRetryAfter: tc.next}, now)
		if got != tc.want {
			t.Errorf("%s: ttl = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCooldownRecordExpiresOnTheServer(t *testing.T) {
	f := newStoreFixture(t)
	s := f.open()
	ctx := context.Background()
	record := cooldownRecord("one", "m", time.Now())
	entry := cooldownEntry{UpdatedAt: record.UpdatedAt.UTC().Truncate(time.Microsecond), Record: &record}
	if err := s.cooldown.put(ctx, cooldownID(record), entry, time.Second, func(cooldownEntry) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCooldowns(t, s)["one/m"]; !ok {
		t.Fatal("record is not there before it expires")
	}
	waitFor(t, "record to expire", func() bool {
		_, ok := loadCooldowns(t, s)["one/m"]
		return !ok
	})
}
