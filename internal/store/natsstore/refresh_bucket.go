package natsstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// EnsureRefreshBucket makes the refresh journal bucket exist and verifies it is
// safe to claim grants in. Unlike the other buckets it is never reconciled once
// it exists: a journal that drifted may already have lost claims, so the
// replica refuses to start refresh workers and an operator decides.
//
// jetstream.KeyValueConfig has no field for DenyDelete, DenyPurge or
// AllowRollup, so the KV helpers cannot create the bucket the journal wants.
// CreateKeyValue already sets DenyDelete. In CreateAuto mode this updates the
// underlying KV_ stream to also deny purge and roll-ups, which is what stops
// a KV purge from wiping a claim, and the server refuses to cancel it later.
// In CreateNever mode the operator provisions the stream with those settings.
//
// Known gap: a KV delete is an ordinary publish carrying a delete marker header,
// so no stream setting prevents it. Only publish permissions on the bucket
// subjects can. The journal never deletes, claims treat a marker as an existing
// write, and reads fail closed, so a marker blocks the grant instead of
// reopening it.
func EnsureRefreshBucket(ctx context.Context, js jetstream.JetStream, spec BucketSpec, mode CreateMode) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, spec.Name)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		if mode != CreateAuto {
			return nil, fmt.Errorf("%w: %s; provision it or set %s=%s", ErrBucketMissing, spec.Name, EnvStoreCreate, CreateAuto)
		}
		kv, err = js.CreateOrUpdateKeyValue(ctx, spec.config())
		if err != nil {
			// Another replica may have created and hardened it first, which
			// makes the reconciling update fail.
			var errOpen error
			if kv, errOpen = js.KeyValue(ctx, spec.Name); errOpen != nil {
				return nil, fmt.Errorf("failed to ensure bucket %s: %w", spec.Name, err)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("failed to ensure bucket %s: %w", spec.Name, err)
	}

	stream, err := js.Stream(ctx, kvStreamName(spec.Name))
	if err != nil {
		return nil, fmt.Errorf("failed to read stream of bucket %s: %w", spec.Name, err)
	}
	cfg := stream.CachedInfo().Config
	if mode == CreateAuto && (!cfg.DenyDelete || !cfg.DenyPurge || cfg.AllowRollup) {
		cfg.DenyDelete, cfg.DenyPurge, cfg.AllowRollup = true, true, false
		if _, err := js.UpdateStream(ctx, cfg); err != nil {
			return nil, fmt.Errorf("failed to deny delete and purge on bucket %s: %w", spec.Name, err)
		}
	}
	if err := verifyRefreshBucket(ctx, js, kv, spec); err != nil {
		return nil, err
	}
	return kv, nil
}

const journalRemedy = "refresh workers will not start; recreate the bucket as documented, since a journal that drifted may have lost claims"

// kvStreamName is the stream JetStream keeps a bucket in.
func kvStreamName(bucket string) string { return "KV_" + bucket }

// verifyRefreshBucket checks the shape every bucket has plus what only the
// journal needs: no limit that can discard a claim and no way to delete or purge one.
func verifyRefreshBucket(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue, spec BucketSpec) error {
	if err := verifyBucket(ctx, kv, spec); err != nil {
		var drift *DriftError
		if errors.As(err, &drift) {
			drift.Remedy = journalRemedy
		}
		return err
	}
	stream, err := js.Stream(ctx, kvStreamName(spec.Name))
	if err != nil {
		return fmt.Errorf("failed to read stream of bucket %s: %w", spec.Name, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("failed to read stream of bucket %s: %w", spec.Name, err)
	}
	cfg := info.Config

	var drift []string
	check := func(name string, have, want any) {
		if have != want {
			drift = append(drift, fmt.Sprintf("%s: have %v, want %v", name, have, want))
		}
	}
	check("max messages", cfg.MaxMsgs, int64(-1))
	check("max bytes", cfg.MaxBytes, int64(-1))
	check("per-message ttl", cfg.AllowMsgTTL, false)
	check("deny delete", cfg.DenyDelete, true)
	check("deny purge", cfg.DenyPurge, true)
	check("allow roll-up", cfg.AllowRollup, false)
	if len(drift) > 0 {
		return &DriftError{Bucket: spec.Name, Fields: drift, Remedy: journalRemedy}
	}
	return nil
}
