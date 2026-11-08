package natsstore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func refreshStreamConfig(t *testing.T, boot *Bootstrapped, settings StoreSettings) jetstream.StreamConfig {
	t.Helper()
	stream, err := boot.JS.Stream(testContext(t), kvStreamName(settings.BucketName(BucketRefresh)))
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return info.Config
}

func TestBootstrapDeniesDeleteAndPurgeOnJournal(t *testing.T) {
	srv := natstest.Start(t)
	settings := autoSettings("hardened")
	ctx := testContext(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := Bootstrap(ctx, nc, settings)
	if err != nil {
		t.Fatal(err)
	}
	cfg := refreshStreamConfig(t, boot, settings)
	if !cfg.DenyDelete || !cfg.DenyPurge || cfg.AllowRollup {
		t.Fatalf("journal stream deny delete %v, deny purge %v, allow roll-up %v", cfg.DenyDelete, cfg.DenyPurge, cfg.AllowRollup)
	}

	kv := boot.Bucket(settings, BucketRefresh)
	if _, err := kv.Create(ctx, "claim", []byte("pending")); err != nil {
		t.Fatal(err)
	}
	if err := kv.Purge(ctx, "claim"); err == nil {
		t.Fatal("a KV purge removed a claim")
	}
	stream, err := boot.JS.Stream(ctx, kvStreamName(settings.BucketName(BucketRefresh)))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Purge(ctx); err == nil {
		t.Fatal("a stream purge removed the journal")
	}
	if err := stream.DeleteMsg(ctx, 1); err == nil {
		t.Fatal("a message delete removed a claim")
	}

	// Starting again against the hardened bucket reconciles nothing and succeeds.
	if _, err := Bootstrap(ctx, nc, settings); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if entry, err := kv.Get(ctx, "claim"); err != nil || string(entry.Value()) != "pending" {
		t.Fatalf("claim after second bootstrap = %v, %v", entry, err)
	}
}

func TestEnsureRefreshBucketRefusesMisconfiguredJournal(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	want := autoSettings("probe").Buckets()[2]
	if want.Name != "probe-refresh" {
		t.Fatalf("unexpected bucket order: %s", want.Name)
	}

	// A bucket the plain KV helpers create is missing only the purge denial.
	good := func(name string) jetstream.KeyValueConfig {
		cfg := want.config()
		cfg.Bucket = name
		return cfg
	}
	cases := []struct {
		name   string
		mutate func(*jetstream.KeyValueConfig)
		field  string
	}{
		{"ttl", func(c *jetstream.KeyValueConfig) { c.TTL = time.Hour }, "ttl"},
		{"memory", func(c *jetstream.KeyValueConfig) { c.Storage = jetstream.MemoryStorage }, "storage"},
		{"history", func(c *jetstream.KeyValueConfig) { c.History = 5 }, "history"},
		{"size limit", func(c *jetstream.KeyValueConfig) { c.MaxBytes = 1 << 20 }, "max bytes"},
		{"marker ttl", func(c *jetstream.KeyValueConfig) { c.LimitMarkerTTL = time.Minute }, "limit marker ttl"},
		{"purge", func(*jetstream.KeyValueConfig) {}, "deny purge"},
	}
	for _, tc := range cases {
		for _, mode := range []CreateMode{CreateNever, CreateAuto} {
			name := strings.ReplaceAll(tc.name, " ", "") + string(mode)
			cfg := good(name)
			tc.mutate(&cfg)
			if _, err := js.CreateKeyValue(ctx, cfg); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			spec := want
			spec.Name = name
			_, err := EnsureRefreshBucket(ctx, js, spec, mode)
			var drift *DriftError
			switch {
			case tc.name == "purge" && mode == CreateAuto:
				// Hardening is the one thing auto mode fixes.
				if err != nil {
					t.Errorf("%s/%s: %v", tc.name, mode, err)
				}
			case !errors.As(err, &drift) || !strings.Contains(strings.Join(drift.Fields, ";"), tc.field):
				t.Errorf("%s/%s: err = %v, want drift on %s", tc.name, mode, err, tc.field)
			case strings.Contains(drift.Error(), EnvStoreCreate):
				t.Errorf("%s/%s: advice points at %s, which does not reconcile the journal: %v", tc.name, mode, EnvStoreCreate, err)
			}
		}
	}

	spec := want
	spec.Name = "absent-refresh"
	if _, err := EnsureRefreshBucket(ctx, js, spec, CreateNever); !errors.Is(err, ErrBucketMissing) {
		t.Fatalf("missing bucket in never mode = %v, want ErrBucketMissing", err)
	}
	// The replica count is part of the expected shape.
	if _, err := js.CreateKeyValue(ctx, good("replicas")); err != nil {
		t.Fatal(err)
	}
	spec = want
	spec.Name = "replicas"
	spec.Replicas = 3
	if _, err := EnsureRefreshBucket(ctx, js, spec, CreateNever); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Fatalf("replica mismatch = %v, want drift on replicas", err)
	}
}
