package natsstore

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Store environment variables.
const (
	EnvStorePrefix   = "NATS_STORE_PREFIX"
	EnvStoreCreate   = "NATS_STORE_CREATE"
	EnvStoreReplicas = "NATS_STORE_REPLICAS"
	EnvStoreStorage  = "NATS_STORE_STORAGE"

	// DefaultPrefix names the buckets when NATS_STORE_PREFIX is unset.
	DefaultPrefix = "cliproxy"

	maxPrefixLength = 48
	maxReplicas     = 5

	// limitMarkerTTL is how long buckets with per-key TTL keep expiry markers.
	limitMarkerTTL = time.Minute
)

// CreateMode decides whether bootstrap may create or update buckets.
type CreateMode string

const (
	// CreateNever only verifies that buckets exist with the expected shape, for
	// deployments where an operator provisions them with tighter permissions.
	CreateNever CreateMode = "never"
	// CreateAuto creates missing buckets and updates drifted ones.
	CreateAuto CreateMode = "auto"
)

// StoreSettings is the non-secret store layout configuration.
type StoreSettings struct {
	// Prefix names every bucket as <prefix>-<purpose>, so deployments can share a cluster.
	Prefix string
	Create CreateMode
	// Replicas is the replication factor, 1 to 5.
	Replicas int
	// Storage applies to buckets that tolerate loss. Buckets holding credentials
	// or the refresh journal always use file storage.
	Storage jetstream.StorageType
}

// DefaultStoreSettings verifies only, with one replica and file storage.
func DefaultStoreSettings() StoreSettings {
	return StoreSettings{Prefix: DefaultPrefix, Create: CreateNever, Replicas: 1, Storage: jetstream.FileStorage}
}

// StoreSettingsFromEnv reads the store layout from the environment, applying defaults for unset values.
func StoreSettingsFromEnv(env Lookup) (StoreSettings, error) {
	settings := DefaultStoreSettings()
	if value, ok := env(EnvStorePrefix); ok && value != "" {
		settings.Prefix = value
	}
	if value, ok := env(EnvStoreCreate); ok && value != "" {
		settings.Create = CreateMode(strings.ToLower(strings.TrimSpace(value)))
	}
	if value, ok := env(EnvStoreReplicas); ok && value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return StoreSettings{}, &SettingError{Setting: EnvStoreReplicas, Err: fmt.Errorf("%q is not a number", value)}
		}
		settings.Replicas = n
	}
	if value, ok := env(EnvStoreStorage); ok && value != "" {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "file":
			settings.Storage = jetstream.FileStorage
		case "memory":
			settings.Storage = jetstream.MemoryStorage
		default:
			return StoreSettings{}, &SettingError{Setting: EnvStoreStorage, Err: fmt.Errorf("%q is not file or memory", value)}
		}
	}
	if err := settings.Validate(); err != nil {
		return StoreSettings{}, err
	}
	return settings, nil
}

// Validate rejects settings that cannot name valid buckets or streams.
func (s StoreSettings) Validate() error {
	if s.Prefix == "" || len(s.Prefix) > maxPrefixLength {
		return &SettingError{Setting: EnvStorePrefix, Err: fmt.Errorf("must be 1 to %d characters", maxPrefixLength)}
	}
	for _, r := range s.Prefix {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
			return &SettingError{Setting: EnvStorePrefix, Err: fmt.Errorf("allows only letters, digits, - and _")}
		}
	}
	switch s.Create {
	case CreateNever, CreateAuto:
	default:
		return &SettingError{Setting: EnvStoreCreate, Err: fmt.Errorf("%q is not auto or never", s.Create)}
	}
	if s.Replicas < 1 || s.Replicas > maxReplicas {
		return &SettingError{Setting: EnvStoreReplicas, Err: fmt.Errorf("must be 1 to %d", maxReplicas)}
	}
	return nil
}

// BucketSpec is the shape one KV bucket must have.
type BucketSpec struct {
	// Name is the full bucket name, including the prefix.
	Name        string
	Description string
	History     uint8
	// MarkerTTL enables per-key TTL on the bucket, zero for none. It is never
	// combined with a bucket-wide TTL, so keys only expire when written with one.
	MarkerTTL time.Duration
	Storage   jetstream.StorageType
	Replicas  int
}

// Bucket purposes, combined with the prefix to form bucket names.
const (
	BucketConfig   = "config"
	BucketAuth     = "auth"
	BucketRefresh  = "refresh"
	BucketCooldown = "cooldown"
	BucketLease    = "lease"
	BucketMeta     = "meta"
)

// BucketName returns the bucket name for a purpose.
func (s StoreSettings) BucketName(purpose string) string { return s.Prefix + "-" + purpose }

// Buckets lists every bucket the backend uses, in a stable order. Config,
// credentials, the refresh journal and metadata never expire and always use
// file storage; cooldown and lease keys expire per key and follow Storage.
func (s StoreSettings) Buckets() []BucketSpec {
	spec := func(purpose, description string, history uint8, markerTTL time.Duration, durable bool) BucketSpec {
		storage := jetstream.FileStorage
		if !durable {
			storage = s.Storage
		}
		return BucketSpec{
			Name:        s.BucketName(purpose),
			Description: description,
			History:     history,
			MarkerTTL:   markerTTL,
			Storage:     storage,
			Replicas:    s.Replicas,
		}
	}
	return []BucketSpec{
		spec(BucketConfig, "CLIProxyAPI configuration", 20, 0, true),
		spec(BucketAuth, "CLIProxyAPI credentials", 5, 0, true),
		spec(BucketRefresh, "CLIProxyAPI refresh grant journal", 1, 0, true),
		spec(BucketCooldown, "CLIProxyAPI cooldown state", 1, limitMarkerTTL, false),
		spec(BucketLease, "CLIProxyAPI refresh leases", 1, limitMarkerTTL, false),
		spec(BucketMeta, "CLIProxyAPI schema and migration markers", 1, limitMarkerTTL, true),
	}
}

func (b BucketSpec) config() jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		Bucket:         b.Name,
		Description:    b.Description,
		History:        b.History,
		Storage:        b.Storage,
		Replicas:       b.Replicas,
		LimitMarkerTTL: b.MarkerTTL,
	}
}
