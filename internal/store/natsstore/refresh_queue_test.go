package natsstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

// openRefreshQueues bootstraps one connection per replica against one server.
func openRefreshQueues(t *testing.T, replicas int, tune func(*StoreSettings, *RefreshQueueSpec)) (*natstest.Server, StoreSettings, []*RefreshQueue) {
	t.Helper()
	srv := natstest.Start(t)
	settings := autoSettings("queue")
	spec := settings.RefreshQueue()
	if tune != nil {
		tune(&settings, &spec)
	}
	var queues []*RefreshQueue
	for range replicas {
		nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		js, err := jetstream.New(nc)
		if err != nil {
			t.Fatal(err)
		}
		queue, err := EnsureRefreshQueue(testContext(t), js, spec, CreateAuto)
		if err != nil {
			t.Fatal(err)
		}
		queues = append(queues, queue)
	}
	return srv, settings, queues
}

// consume runs the queue's consumer until the test ends.
func consume(t *testing.T, queue *RefreshQueue, handle RefreshHandler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := queue.Consume(ctx, handle); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// consumeWithoutAnswering receives requests and never acknowledges them until
// ctx ends, the way a process that died mid-refresh leaves its request.
func consumeWithoutAnswering(ctx context.Context, queue *RefreshQueue, received func()) {
	cc, err := queue.consumer.Consume(func(jetstream.Msg) { received() })
	if err != nil {
		return
	}
	<-ctx.Done()
	cc.Stop()
}

func TestRefreshQueueDeliversOneRequestToOneConsumer(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 2, nil)
	var handled atomic.Int32
	seen := make(chan RefreshRequest, 4)
	handle := func(_ context.Context, req RefreshRequest) error {
		handled.Add(1)
		seen <- req
		return nil
	}
	consume(t, queues[0], handle)
	consume(t, queues[1], handle)

	queued, err := queues[0].Publish(testContext(t), RefreshRequest{CredentialID: "team/a@example.com.json", Provider: "codex", Version: 3, RequestedBy: "r1"})
	if err != nil || !queued {
		t.Fatalf("publish = %v, %v", queued, err)
	}
	select {
	case req := <-seen:
		if req.CredentialID != "team/a@example.com.json" || req.Provider != "codex" || req.Version != 3 {
			t.Fatalf("request = %+v", req)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no consumer received the request")
	}
	// Give a wrongly shared delivery time to show up.
	time.Sleep(time.Second)
	if n := handled.Load(); n != 1 {
		t.Fatalf("handlers ran %d times, want exactly 1", n)
	}
}

func TestRefreshQueueRefusesASecondRequestWhileOneIsQueued(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 1, nil)
	queue, ctx := queues[0], testContext(t)

	if queued, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil || !queued {
		t.Fatalf("first publish = %v, %v", queued, err)
	}
	// Same version: dedupe by message id.
	if queued, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil || queued {
		t.Fatalf("same version publish = %v, %v; want not queued", queued, err)
	}
	// New version while the first waits: refused by the per-credential limit.
	if queued, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 2}); err != nil || queued {
		t.Fatalf("second version publish = %v, %v; want not queued", queued, err)
	}
	// Another credential is independent.
	if queued, err := queue.Publish(ctx, RefreshRequest{CredentialID: "b", Version: 1}); err != nil || !queued {
		t.Fatalf("other credential publish = %v, %v", queued, err)
	}
	info, err := queue.consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.NumPending; got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
}

func TestRefreshQueueAcceptsANewVersionAfterTheRequestIsHandled(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 1, nil)
	queue, ctx := queues[0], testContext(t)
	var handled atomic.Int32
	consume(t, queue, func(context.Context, RefreshRequest) error {
		handled.Add(1)
		return nil
	})

	if _, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first request", func() bool { return handled.Load() == 1 })
	var queued bool
	waitFor(t, "the slot to free", func() bool {
		var err error
		queued, err = queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 2})
		return err == nil && queued
	})
	waitFor(t, "the second request", func() bool { return handled.Load() == 2 })
}

func TestRefreshQueueRedeliversAnUnacknowledgedRequest(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 2, func(_ *StoreSettings, spec *RefreshQueueSpec) {
		spec.AckWait = 3 * time.Second
	})
	ctx := testContext(t)
	crashed := make(chan struct{})
	var once sync.Once
	var winner atomic.Int32

	// The first replica takes the request and never answers, like a crashed process.
	stuck, cancelStuck := context.WithCancel(context.Background())
	go func() {
		consumeWithoutAnswering(stuck, queues[0], func() { once.Do(func() { close(crashed) }) })
	}()
	t.Cleanup(cancelStuck)

	if _, err := queues[0].Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-crashed:
	case <-time.After(20 * time.Second):
		t.Fatal("the first replica never received the request")
	}
	cancelStuck()

	consume(t, queues[1], func(context.Context, RefreshRequest) error {
		winner.Add(1)
		return nil
	})
	waitFor(t, "redelivery to the second replica", func() bool { return winner.Load() == 1 })
}

func TestRefreshQueueStopsAtMaxDeliver(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 1, func(_ *StoreSettings, spec *RefreshQueueSpec) {
		spec.MaxDeliver = 2
	})
	queue, ctx := queues[0], testContext(t)
	var attempts atomic.Int32
	consume(t, queue, func(context.Context, RefreshRequest) error {
		attempts.Add(1)
		return RetryRefresh(errors.New("provider unavailable"), 100*time.Millisecond)
	})
	if _, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both deliveries", func() bool { return attempts.Load() == 2 })
	time.Sleep(time.Second)
	if n := attempts.Load(); n != 2 {
		t.Fatalf("delivered %d times, want max deliver 2", n)
	}
}

func TestRefreshQueueTerminalFailureIsNotRedelivered(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 1, nil)
	queue, ctx := queues[0], testContext(t)
	var attempts atomic.Int32
	consume(t, queue, func(context.Context, RefreshRequest) error {
		attempts.Add(1)
		return errors.New("refresh rejected")
	})
	if _, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the delivery", func() bool { return attempts.Load() == 1 })
	time.Sleep(time.Second)
	if n := attempts.Load(); n != 1 {
		t.Fatalf("delivered %d times, want 1", n)
	}
	// The request left the queue, so the credential can be asked for again.
	waitFor(t, "the slot to free", func() bool {
		queued, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 2})
		return err == nil && queued
	})
}

func TestRefreshQueueKeepsASlowHandlerAlive(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 2, func(_ *StoreSettings, spec *RefreshQueueSpec) {
		spec.AckWait = 2 * time.Second
	})
	var handled atomic.Int32
	slow := func(context.Context, RefreshRequest) error {
		handled.Add(1)
		time.Sleep(5 * time.Second)
		return nil
	}
	consume(t, queues[0], slow)
	consume(t, queues[1], slow)
	if _, err := queues[0].Publish(testContext(t), RefreshRequest{CredentialID: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the delivery", func() bool { return handled.Load() >= 1 })
	time.Sleep(7 * time.Second)
	if n := handled.Load(); n != 1 {
		t.Fatalf("a handler outlasting the ack wait ran %d times, want 1", n)
	}
}

func TestRefreshQueueMaxAgeFreesAStuckRequest(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 1, func(_ *StoreSettings, spec *RefreshQueueSpec) {
		spec.MaxAge, spec.DuplicateWindow = 2*time.Second, 2*time.Second
	})
	queue, ctx := queues[0], testContext(t)
	if _, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the request to age out", func() bool {
		queued, err := queue.Publish(ctx, RefreshRequest{CredentialID: "a", Version: 2})
		return err == nil && queued
	})
}

func TestRefreshQueueDropsMalformedRequests(t *testing.T) {
	_, _, queues := openRefreshQueues(t, 1, nil)
	queue, ctx := queues[0], testContext(t)
	var handled atomic.Int32
	consume(t, queue, func(context.Context, RefreshRequest) error {
		handled.Add(1)
		return nil
	})
	subject, err := queue.spec.Subject("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: []byte("not json")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the malformed request to leave", func() bool {
		info, err := queue.consumer.Info(ctx)
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})
	if handled.Load() != 0 {
		t.Fatal("handler ran for a malformed request")
	}
}

func TestEnsureRefreshQueueDriftAndNeverMode(t *testing.T) {
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
	spec := autoSettings("drift").RefreshQueue()

	if _, err := EnsureRefreshQueue(ctx, js, spec, CreateNever); !errors.Is(err, ErrBucketMissing) {
		t.Fatalf("never mode on a missing stream = %v", err)
	}
	if _, err := EnsureRefreshQueue(ctx, js, spec, CreateAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRefreshQueue(ctx, js, spec, CreateNever); err != nil {
		t.Fatalf("never mode on a provisioned queue: %v", err)
	}

	tighter := spec
	tighter.MaxDeliver = 9
	var drift *DriftError
	if _, err := EnsureRefreshQueue(ctx, js, tighter, CreateNever); !errors.As(err, &drift) {
		t.Fatalf("drifted consumer = %v, want DriftError", err)
	}
	if _, err := EnsureRefreshQueue(ctx, js, tighter, CreateAuto); err != nil {
		t.Fatalf("auto mode reconciles a consumer: %v", err)
	}
}

func TestRefreshQueueIsBootstrapped(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := Bootstrap(testContext(t), nc, autoSettings("boot"))
	if err != nil {
		t.Fatal(err)
	}
	if boot.Refresh == nil || boot.Refresh.Spec().Name != "BOOT_REFRESH" {
		t.Fatalf("refresh queue = %+v", boot.Refresh)
	}
}
