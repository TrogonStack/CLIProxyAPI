package natsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	log "github.com/sirupsen/logrus"
)

const (
	// SchemaVersion is the layout version this binary reads and writes.
	SchemaVersion = 1
	// SchemaKey holds the schema marker in the meta bucket.
	SchemaKey = "schema"
)

// ErrSchemaTooNew is returned when the stored layout was written by a newer binary.
var ErrSchemaTooNew = errors.New("stored schema is newer than this binary supports")

// ErrBucketMissing is returned in CreateNever mode when a bucket has not been provisioned.
var ErrBucketMissing = errors.New("bucket does not exist")

// DriftError reports a bucket whose configuration differs from the expected shape.
type DriftError struct {
	Bucket string
	// Fields lists each mismatch as `name: have X, want Y`.
	Fields []string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("bucket %s differs from the expected configuration (%s); update it to match or set %s=%s to let startup reconcile it",
		e.Bucket, strings.Join(e.Fields, "; "), EnvStoreCreate, CreateAuto)
}

// Bootstrapped is a connected JetStream context with every bucket verified.
type Bootstrapped struct {
	JS       jetstream.JetStream
	Features ServerFeatures
	// Buckets maps full bucket name to its handle.
	Buckets map[string]jetstream.KeyValue
}

// Bucket returns the handle for a purpose such as BucketAuth.
func (b *Bootstrapped) Bucket(settings StoreSettings, purpose string) jetstream.KeyValue {
	return b.Buckets[settings.BucketName(purpose)]
}

// Bootstrap probes the server, ensures every bucket and records the schema
// marker. It is idempotent and safe for many replicas to run concurrently.
func Bootstrap(ctx context.Context, nc *nats.Conn, settings StoreSettings) (*Bootstrapped, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}
	features, err := Probe(ctx, nc, js)
	if err != nil {
		return nil, err
	}
	if !features.KeyTTL {
		return nil, fmt.Errorf("NATS server %s does not support per-key TTL (JetStream API level %d); use a server of version %d.%d or newer",
			features.Version, features.APILevel, 2, 11)
	}
	if !features.AtLeast(TargetMajor, TargetMinor) {
		log.Warnf("NATS server %s is older than the tested %d.%d line", features.Version, TargetMajor, TargetMinor)
	}

	buckets := make(map[string]jetstream.KeyValue)
	for _, spec := range settings.Buckets() {
		kv, err := EnsureBucket(ctx, js, spec, settings.Create)
		if err != nil {
			return nil, err
		}
		buckets[spec.Name] = kv
	}
	meta := buckets[settings.BucketName(BucketMeta)]
	if err := EnsureSchema(ctx, meta); err != nil {
		return nil, err
	}
	return &Bootstrapped{JS: js, Features: features, Buckets: buckets}, nil
}

// EnsureBucket makes the bucket exist with the expected shape. With CreateAuto
// it creates or reconciles; with CreateNever it only looks. Either way the
// final shape is verified, so a drifted bucket is reported instead of used.
func EnsureBucket(ctx context.Context, js jetstream.JetStream, spec BucketSpec, mode CreateMode) (jetstream.KeyValue, error) {
	var (
		kv  jetstream.KeyValue
		err error
	)
	switch mode {
	case CreateAuto:
		kv, err = js.CreateOrUpdateKeyValue(ctx, spec.config())
	case CreateNever:
		kv, err = js.KeyValue(ctx, spec.Name)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, fmt.Errorf("%w: %s; provision it or set %s=%s", ErrBucketMissing, spec.Name, EnvStoreCreate, CreateAuto)
		}
	default:
		return nil, fmt.Errorf("unknown create mode %q", mode)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to ensure bucket %s: %w", spec.Name, err)
	}
	if err := verifyBucket(ctx, kv, spec); err != nil {
		return nil, err
	}
	return kv, nil
}

func verifyBucket(ctx context.Context, kv jetstream.KeyValue, spec BucketSpec) error {
	status, err := kv.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to read bucket %s: %w", spec.Name, err)
	}
	cfg := status.(*jetstream.KeyValueBucketStatus).StreamInfo().Config

	var drift []string
	check := func(name string, have, want any) {
		if have != want {
			drift = append(drift, fmt.Sprintf("%s: have %v, want %v", name, have, want))
		}
	}
	check("storage", cfg.Storage.String(), spec.Storage.String())
	check("replicas", cfg.Replicas, spec.Replicas)
	check("history", cfg.MaxMsgsPerSubject, int64(spec.History))
	check("ttl", cfg.MaxAge, time.Duration(0))
	check("limit marker ttl", cfg.SubjectDeleteMarkerTTL, spec.MarkerTTL)
	if len(drift) > 0 {
		return &DriftError{Bucket: spec.Name, Fields: drift}
	}
	return nil
}

type schemaMarker struct {
	Version int `json:"v"`
}

// EnsureSchema records the schema version once and refuses a newer stored one,
// so an old binary in a rolling upgrade never writes into a layout it does not know.
func EnsureSchema(ctx context.Context, meta jetstream.KeyValue) error {
	payload, err := json.Marshal(schemaMarker{Version: SchemaVersion})
	if err != nil {
		return err
	}
	_, err = meta.Create(ctx, SchemaKey, payload)
	if err == nil {
		return nil
	}
	if !errors.Is(err, jetstream.ErrKeyExists) {
		return fmt.Errorf("failed to record schema marker: %w", err)
	}
	entry, err := meta.Get(ctx, SchemaKey)
	if err != nil {
		return fmt.Errorf("failed to read schema marker: %w", err)
	}
	var stored schemaMarker
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		return fmt.Errorf("schema marker is not valid: %w", err)
	}
	if stored.Version > SchemaVersion {
		return fmt.Errorf("%w: stored v%d, supported v%d", ErrSchemaTooNew, stored.Version, SchemaVersion)
	}
	return nil
}
