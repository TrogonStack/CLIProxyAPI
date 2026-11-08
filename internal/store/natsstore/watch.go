package natsstore

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	log "github.com/sirupsen/logrus"
)

// watchRetryDelay is how long Follow waits before reopening a watch that ended.
var watchRetryDelay = time.Second

// Change is one revision observed on a key.
type Change struct {
	Entry
	// Deleted is set for a delete or purge marker, whose Value is empty.
	Deleted bool
}

// Event is what Follow hands to its callback.
type Event struct {
	// Change is the observed revision, nil for the synced event.
	Change *Change
	// Synced marks the end of the initial replay: every key that was live when
	// the watch opened has been delivered. Updates after it are live changes.
	Synced bool
	// Initial lists the keys delivered by the replay and InitialMax is the
	// highest revision among them, so the caller can tell a key the bucket no
	// longer holds from one written after the replay. Both are set on Synced.
	Initial    map[string]struct{}
	InitialMax uint64
}

// Follow replays the bucket and then streams its changes to apply until ctx
// ends. A watch that ends, or fails to open, is reopened after a delay and
// replays from the start, which is safe because every revision carries its
// own ordering. apply is called from one goroutine at a time.
func (k *KV) Follow(ctx context.Context, apply func(Event)) {
	for ctx.Err() == nil {
		watcher, err := k.kv.WatchAll(ctx)
		if err != nil {
			log.WithError(err).Warnf("nats store: watching %s failed, retrying", k.bucket)
		} else {
			if k.onWatch != nil {
				k.onWatch(watcher)
			}
			k.consume(ctx, watcher, apply)
			_ = watcher.Stop()
		}
		select {
		case <-ctx.Done():
		case <-time.After(watchRetryDelay):
		}
	}
}

func (k *KV) consume(ctx context.Context, watcher jetstream.KeyWatcher, apply func(Event)) {
	var (
		synced     bool
		initial    = make(map[string]struct{})
		initialMax uint64
	)
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-watcher.Updates():
			if !ok {
				return
			}
			if entry == nil {
				synced = true
				apply(Event{Synced: true, Initial: initial, InitialMax: initialMax})
				continue
			}
			id, err := DecodeKey(entry.Key())
			if err != nil {
				// Not written by this backend; leave it alone.
				continue
			}
			change := Change{Entry: entryOf(id, entry), Deleted: entry.Operation() != jetstream.KeyValuePut}
			if !synced {
				initial[id] = struct{}{}
				if change.Revision > initialMax {
					initialMax = change.Revision
				}
			}
			apply(Event{Change: &change})
		}
	}
}
