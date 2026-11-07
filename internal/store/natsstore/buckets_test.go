package natsstore

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

func TestStoreSettingsFromEnv(t *testing.T) {
	got, err := StoreSettingsFromEnv(mapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultStoreSettings() {
		t.Fatalf("defaults = %+v", got)
	}

	got, err = StoreSettingsFromEnv(mapEnv(map[string]string{
		EnvStorePrefix:   "prod-eu",
		EnvStoreCreate:   "AUTO",
		EnvStoreReplicas: "3",
		EnvStoreStorage:  "memory",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := StoreSettings{Prefix: "prod-eu", Create: CreateAuto, Replicas: 3, Storage: jetstream.MemoryStorage}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestStoreSettingsRejectsBadValues(t *testing.T) {
	tests := map[string]map[string]string{
		EnvStorePrefix + " dot":   {EnvStorePrefix: "a.b"},
		EnvStorePrefix + " wild":  {EnvStorePrefix: "a>"},
		EnvStorePrefix + " long":  {EnvStorePrefix: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		EnvStoreCreate + " word":  {EnvStoreCreate: "always"},
		EnvStoreReplicas + " nan": {EnvStoreReplicas: "three"},
		EnvStoreReplicas + " big": {EnvStoreReplicas: "6"},
		EnvStoreReplicas + " 0":   {EnvStoreReplicas: "0"},
		EnvStoreStorage + " word": {EnvStoreStorage: "disk"},
	}
	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := StoreSettingsFromEnv(mapEnv(env))
			var setting *SettingError
			if !errors.As(err, &setting) {
				t.Fatalf("err = %v, want SettingError", err)
			}
		})
	}
}

func TestBucketsAreStableAndDurableOnesUseFileStorage(t *testing.T) {
	settings := DefaultStoreSettings()
	settings.Storage = jetstream.MemoryStorage
	buckets := settings.Buckets()

	wantNames := []string{"cliproxy-config", "cliproxy-auth", "cliproxy-refresh", "cliproxy-cooldown", "cliproxy-lease", "cliproxy-meta"}
	for i, name := range wantNames {
		if buckets[i].Name != name {
			t.Fatalf("bucket %d = %q, want %q", i, buckets[i].Name, name)
		}
	}
	for _, bucket := range buckets {
		durable := bucket.Name != "cliproxy-cooldown" && bucket.Name != "cliproxy-lease"
		if durable && bucket.Storage != jetstream.FileStorage {
			t.Errorf("%s uses %s storage", bucket.Name, bucket.Storage)
		}
		if !durable && bucket.Storage != jetstream.MemoryStorage {
			t.Errorf("%s ignores the storage setting", bucket.Name)
		}
	}
	// The refresh journal is never allowed to expire keys.
	if journal := buckets[2]; journal.MarkerTTL != 0 || journal.History != 1 {
		t.Errorf("refresh bucket = %+v", journal)
	}
}
