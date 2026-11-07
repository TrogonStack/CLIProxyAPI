package natsstore

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func autoSettings(prefix string) StoreSettings {
	settings := DefaultStoreSettings()
	settings.Prefix = prefix
	settings.Create = CreateAuto
	return settings
}

func TestBootstrapIsIdempotentAndConcurrencySafe(t *testing.T) {
	srv := natstest.Start(t)
	settings := autoSettings("idem")
	ctx := testContext(t)

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nc, err := Connect(ctx, SettingsFromEnv(mapEnv(map[string]string{EnvURL: srv.URL})).Resolve(), OSLookup, 5*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer nc.Close()
			_, err = Bootstrap(ctx, nc, settings)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent bootstrap: %v", err)
		}
	}

	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := Bootstrap(ctx, nc, settings)
	if err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	if len(boot.Buckets) != len(settings.Buckets()) {
		t.Fatalf("buckets = %d", len(boot.Buckets))
	}
	entry, err := boot.Bucket(settings, BucketMeta).Get(ctx, SchemaKey)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Value()) != `{"v":1}` {
		t.Fatalf("schema marker = %s", entry.Value())
	}
	if entry.Revision() != 1 {
		t.Fatalf("schema marker was rewritten, revision %d", entry.Revision())
	}
}

func TestBootstrapNeverModeVerifiesOnly(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	verify := DefaultStoreSettings()
	verify.Prefix = "verify"

	_, err = Bootstrap(ctx, nc, verify)
	if !errors.Is(err, ErrBucketMissing) {
		t.Fatalf("err = %v, want ErrBucketMissing", err)
	}
	js, _ := jetstream.New(nc)
	if _, lookupErr := js.KeyValue(ctx, "verify-config"); !errors.Is(lookupErr, jetstream.ErrBucketNotFound) {
		t.Fatalf("never mode created a bucket: %v", lookupErr)
	}

	if _, err := Bootstrap(ctx, nc, autoSettings("verify")); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, nc, verify); err != nil {
		t.Fatalf("verify after provisioning: %v", err)
	}
}

func TestBootstrapReportsDrift(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	js, _ := jetstream.New(nc)
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "drift-config", History: 3}); err != nil {
		t.Fatal(err)
	}

	settings := DefaultStoreSettings()
	settings.Prefix = "drift"
	_, err = Bootstrap(ctx, nc, settings)
	var drift *DriftError
	if !errors.As(err, &drift) {
		t.Fatalf("err = %v, want DriftError", err)
	}
	if drift.Bucket != "drift-config" || len(drift.Fields) != 1 || !strings.HasPrefix(drift.Fields[0], "history") {
		t.Fatalf("drift = %+v", drift)
	}
}

func TestBootstrapRefusesNewerSchema(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	settings := autoSettings("schema")
	boot, err := Bootstrap(ctx, nc, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boot.Bucket(settings, BucketMeta).Put(ctx, SchemaKey, []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, nc, settings); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
}

func TestPerKeyTTLExpiresInBootstrappedBuckets(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	settings := autoSettings("ttl")
	boot, err := Bootstrap(ctx, nc, settings)
	if err != nil {
		t.Fatal(err)
	}

	lease := boot.Bucket(settings, BucketLease)
	if _, err := lease.Create(ctx, "short", []byte("x"), jetstream.KeyTTL(time.Second)); err != nil {
		t.Fatalf("create with key TTL: %v", err)
	}
	if _, err := lease.Create(ctx, "short", []byte("y")); !errors.Is(err, jetstream.ErrKeyExists) {
		t.Fatalf("second create = %v, want ErrKeyExists", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := lease.Get(ctx, "short")
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key still readable after its TTL: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := lease.Create(ctx, "short", []byte("again")); err != nil {
		t.Fatalf("create after expiry: %v", err)
	}

	// Keys in the refresh journal bucket never expire: the bucket has no limit marker TTL.
	journal := boot.Bucket(settings, BucketRefresh)
	if _, err := journal.Create(ctx, "claim", []byte("x"), jetstream.KeyTTL(time.Second)); err == nil {
		t.Fatal("refresh bucket accepted a per-key TTL")
	}
}
