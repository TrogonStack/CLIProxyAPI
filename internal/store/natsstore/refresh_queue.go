package natsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	log "github.com/sirupsen/logrus"
)

const (
	// refreshRequestMaxAge bounds how long a request waits. A request that
	// reached max deliveries is never removed by the server, and with one
	// message allowed per credential it would block every later request for
	// that credential, so an unhandled request must age out.
	refreshRequestMaxAge = 10 * time.Minute
	// refreshDuplicateWindow is how long a request id stays remembered, so a
	// replica asking again for the same credential version is not queued twice.
	refreshDuplicateWindow = 2 * time.Minute
	// refreshAckWait is how long a delivered request may go without an ack or
	// a progress signal before another replica gets it. Handlers signal
	// progress, so this only has to outlast a missed heartbeat, not a provider call.
	refreshAckWait = time.Minute
	// refreshMaxDeliver caps deliveries of one request. A redelivery after a
	// crash is safe, but a request that keeps failing should not circulate.
	refreshMaxDeliver = 3
	// refreshMaxInFlight caps unacknowledged requests per replica.
	refreshMaxInFlight = 16

	refreshConsumerName = "refresh-workers"
	refreshNakDelay     = 5 * time.Second
	refreshAckTimeout   = 5 * time.Second

	// subjectFullText is what the server answers when a subject already holds
	// the one message the queue allows it.
	subjectFullText = "maximum messages per subject exceeded"
)

// RefreshStreamName is the stream holding refresh requests.
func (s StoreSettings) RefreshStreamName() string { return strings.ToUpper(s.Prefix) + "_REFRESH" }

// RefreshQueue describes the refresh work queue for these settings.
func (s StoreSettings) RefreshQueue() RefreshQueueSpec {
	return RefreshQueueSpec{
		Name:            s.RefreshStreamName(),
		SubjectPrefix:   s.Prefix + ".refresh",
		Replicas:        s.Replicas,
		MaxAge:          refreshRequestMaxAge,
		DuplicateWindow: refreshDuplicateWindow,
		AckWait:         refreshAckWait,
		MaxDeliver:      refreshMaxDeliver,
		MaxInFlight:     refreshMaxInFlight,
		ConsumerName:    refreshConsumerName,
		Description:     "CLIProxyAPI credential refresh requests",
	}
}

// RefreshQueueSpec is the shape of the refresh work queue: one stream holding
// at most one request per credential and one shared consumer, so exactly one
// replica receives each request.
type RefreshQueueSpec struct {
	// Name is the stream name.
	Name string
	// SubjectPrefix is followed by the encoded credential id to form a subject.
	SubjectPrefix   string
	Description     string
	Replicas        int
	MaxAge          time.Duration
	DuplicateWindow time.Duration

	ConsumerName string
	AckWait      time.Duration
	MaxDeliver   int
	MaxInFlight  int
}

// Subject returns the subject carrying requests for the credential.
func (q RefreshQueueSpec) Subject(credentialID string) (string, error) {
	token, err := EncodeKey(credentialID)
	if err != nil {
		return "", err
	}
	return q.SubjectPrefix + "." + token, nil
}

func (q RefreshQueueSpec) streamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:        q.Name,
		Description: q.Description,
		Subjects:    []string{q.SubjectPrefix + ".>"},
		// A request leaves the stream once it is acknowledged.
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		Replicas:  q.Replicas,
		// One queued request per credential. With discard-new a second request
		// is refused instead of replacing the first, which may be in flight.
		MaxMsgsPerSubject:    1,
		Discard:              jetstream.DiscardNew,
		DiscardNewPerSubject: true,
		MaxAge:               q.MaxAge,
		Duplicates:           q.DuplicateWindow,
	}
}

func (q RefreshQueueSpec) consumerConfig() jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       q.ConsumerName,
		Description:   "CLIProxyAPI refresh workers",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       q.AckWait,
		MaxDeliver:    q.MaxDeliver,
		MaxAckPending: q.MaxInFlight,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		FilterSubject: q.SubjectPrefix + ".>",
	}
}

// EnsureRefreshQueue makes the stream and its shared consumer exist with the
// expected shape. With CreateAuto it creates or reconciles; with CreateNever it
// only looks. Either way the final shape is verified. A stream's retention
// cannot change once created, so a drifted one is recreated by an operator.
func EnsureRefreshQueue(ctx context.Context, js jetstream.JetStream, spec RefreshQueueSpec, mode CreateMode) (*RefreshQueue, error) {
	var (
		stream   jetstream.Stream
		consumer jetstream.Consumer
		err      error
	)
	switch mode {
	case CreateAuto:
		if stream, err = js.CreateOrUpdateStream(ctx, spec.streamConfig()); err != nil {
			return nil, fmt.Errorf("failed to ensure stream %s: %w", spec.Name, err)
		}
		if consumer, err = js.CreateOrUpdateConsumer(ctx, spec.Name, spec.consumerConfig()); err != nil {
			return nil, fmt.Errorf("failed to ensure consumer %s: %w", spec.ConsumerName, err)
		}
	case CreateNever:
		if stream, err = js.Stream(ctx, spec.Name); err != nil {
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				return nil, fmt.Errorf("%w: %s; provision it or set %s=%s", ErrBucketMissing, spec.Name, EnvStoreCreate, CreateAuto)
			}
			return nil, fmt.Errorf("failed to read stream %s: %w", spec.Name, err)
		}
		if consumer, err = stream.Consumer(ctx, spec.ConsumerName); err != nil {
			return nil, fmt.Errorf("failed to read consumer %s: %w; provision it or set %s=%s", spec.ConsumerName, err, EnvStoreCreate, CreateAuto)
		}
	default:
		return nil, fmt.Errorf("unknown create mode %q", mode)
	}
	if err := verifyRefreshQueue(ctx, stream, consumer, spec); err != nil {
		return nil, err
	}
	return &RefreshQueue{js: js, spec: spec, consumer: consumer}, nil
}

func verifyRefreshQueue(ctx context.Context, stream jetstream.Stream, consumer jetstream.Consumer, spec RefreshQueueSpec) error {
	info, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("failed to read stream %s: %w", spec.Name, err)
	}
	want := spec.streamConfig()
	have := info.Config

	var drift []string
	check := func(name string, have, want any) {
		if have != want {
			drift = append(drift, fmt.Sprintf("%s: have %v, want %v", name, have, want))
		}
	}
	check("retention", have.Retention.String(), want.Retention.String())
	check("storage", have.Storage.String(), want.Storage.String())
	check("replicas", have.Replicas, want.Replicas)
	check("discard", have.Discard.String(), want.Discard.String())
	check("discard new per subject", have.DiscardNewPerSubject, want.DiscardNewPerSubject)
	check("max messages per subject", have.MaxMsgsPerSubject, want.MaxMsgsPerSubject)
	check("max age", have.MaxAge, want.MaxAge)
	check("duplicate window", have.Duplicates, want.Duplicates)
	if !slices.Equal(have.Subjects, want.Subjects) {
		drift = append(drift, fmt.Sprintf("subjects: have %v, want %v", have.Subjects, want.Subjects))
	}

	consumerInfo, err := consumer.Info(ctx)
	if err != nil {
		return fmt.Errorf("failed to read consumer %s: %w", spec.ConsumerName, err)
	}
	wantConsumer := spec.consumerConfig()
	haveConsumer := consumerInfo.Config
	check("consumer ack policy", haveConsumer.AckPolicy.String(), wantConsumer.AckPolicy.String())
	check("consumer ack wait", haveConsumer.AckWait, wantConsumer.AckWait)
	check("consumer max deliver", haveConsumer.MaxDeliver, wantConsumer.MaxDeliver)
	check("consumer max ack pending", haveConsumer.MaxAckPending, wantConsumer.MaxAckPending)
	check("consumer filter", haveConsumer.FilterSubject, wantConsumer.FilterSubject)
	if len(drift) > 0 {
		return &DriftError{Bucket: spec.Name, Fields: drift}
	}
	return nil
}

// RefreshRequest asks for one credential to be refreshed. It carries no secret:
// the worker reads the credential itself.
type RefreshRequest struct {
	CredentialID string `json:"credential_id"`
	// Provider is informational, for logs and for handlers that route by it.
	Provider string `json:"provider,omitempty"`
	// Version is the credential version the requester saw expiring. A request
	// for a version that was already replaced is skipped by the worker.
	Version uint64 `json:"version"`
	// RequestedBy is the replica that published the request.
	RequestedBy string `json:"requested_by,omitempty"`
}

// RefreshQueue publishes refresh requests and delivers each to exactly one worker.
type RefreshQueue struct {
	js       jetstream.JetStream
	spec     RefreshQueueSpec
	consumer jetstream.Consumer
}

// Spec returns the queue shape.
func (q *RefreshQueue) Spec() RefreshQueueSpec { return q.spec }

// Publish queues a request. It reports false when an equivalent request is
// already known, either queued for the credential or published for the same
// credential version inside the duplicate window; nothing new is queued then,
// and the caller has nothing to retry.
func (q *RefreshQueue) Publish(ctx context.Context, req RefreshRequest) (bool, error) {
	subject, err := q.spec.Subject(req.CredentialID)
	if err != nil {
		return false, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("failed to encode refresh request: %w", err)
	}
	token, _ := EncodeKey(req.CredentialID)
	msgID := token + ":" + strconv.FormatUint(req.Version, 10)
	ack, err := q.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: body}, jetstream.WithMsgID(msgID))
	if err != nil {
		if isSubjectFull(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to queue refresh of %s: %w", req.CredentialID, err)
	}
	return !ack.Duplicate, nil
}

// isSubjectFull reports the refusal of a request for a credential that already has one queued.
func isSubjectFull(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && strings.Contains(apiErr.Description, subjectFullText)
}

// RefreshHandler performs one refresh request. Returning nil acknowledges it,
// RetryRefresh asks for a later redelivery, and any other error ends the
// request without redelivery, since the request was understood and failed.
type RefreshHandler func(ctx context.Context, req RefreshRequest) error

type retryError struct {
	err   error
	delay time.Duration
}

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

// RetryRefresh marks a handler failure as worth another delivery after delay,
// or after a default delay when delay is not positive.
func RetryRefresh(err error, delay time.Duration) error {
	if err == nil {
		return nil
	}
	return &retryError{err: err, delay: delay}
}

// Consume delivers requests to handle until ctx ends, then waits for running
// handlers. Every replica that consumes shares one consumer, so each request
// goes to one of them. A handler is kept alive with progress signals while it
// runs, so its request is not redelivered to a second replica mid-refresh.
func (q *RefreshQueue) Consume(ctx context.Context, handle RefreshHandler) error {
	var running sync.WaitGroup
	consumeCtx, err := q.consumer.Consume(func(msg jetstream.Msg) {
		running.Add(1)
		go func() {
			defer running.Done()
			q.handle(ctx, msg, handle)
		}()
	}, jetstream.PullMaxMessages(q.spec.MaxInFlight), jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		log.WithError(err).Warn("nats store: refresh consumer error")
	}))
	if err != nil {
		return fmt.Errorf("failed to consume refresh requests: %w", err)
	}
	<-ctx.Done()
	consumeCtx.Stop()
	running.Wait()
	return nil
}

func (q *RefreshQueue) handle(ctx context.Context, msg jetstream.Msg, handle RefreshHandler) {
	var req RefreshRequest
	if err := json.Unmarshal(msg.Data(), &req); err != nil || req.CredentialID == "" {
		log.Warn("nats store: dropping a malformed refresh request")
		_ = msg.Term()
		return
	}

	stopBeat := make(chan struct{})
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		ticker := time.NewTicker(q.spec.AckWait / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-ticker.C:
				_ = msg.InProgress()
			}
		}
	}()
	err := handle(ctx, req)
	close(stopBeat)
	<-beatDone

	// The acknowledgement must not depend on the consumer context, which
	// has ended when the replica is shutting down.
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshAckTimeout)
	defer cancel()
	var retry *retryError
	switch {
	case err == nil:
		if err := msg.DoubleAck(ackCtx); err != nil && !errors.Is(err, jetstream.ErrMsgAlreadyAckd) {
			log.WithError(err).Warn("nats store: refresh acknowledgement was not confirmed")
		}
	case errors.As(err, &retry):
		delay := retry.delay
		if delay <= 0 {
			delay = refreshNakDelay
		}
		_ = msg.NakWithDelay(delay)
	case ctx.Err() != nil:
		// Shutting down mid-request: hand it to another replica now instead of
		// after the ack wait. It meets the journal's claim if this one sent a grant.
		_ = msg.Nak()
	default:
		log.WithError(err).Warnf("nats store: refresh request for %s failed", req.CredentialID)
		_ = msg.Term()
	}
}
