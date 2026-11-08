package natsstore

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const e2eProvider = "e2e-oauth"

// e2eGrant is what the stub provider hands back, shaped like a journal receipt value.
type e2eGrant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Expired      string `json:"expired"`
}

// e2eProviderStub stands in for the token endpoint and counts what it is sent.
type e2eProviderStub struct {
	requests atomic.Int32
}

func (p *e2eProviderStub) send() (*e2eGrant, error) {
	p.requests.Add(1)
	return &e2eGrant{AccessToken: "new-access", RefreshToken: "new-refresh", Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, nil
}

// crashingReceipts loses the replica between the provider's answer and the receipt.
type crashingReceipts struct{ codex.RefreshJournal }

func (crashingReceipts) RefreshComplete(context.Context, string, []byte) error {
	return errors.New("replica crashed before saving the receipt")
}

// e2eExecutor refreshes a single-use grant through the journal bound to the
// context, the way the providers that rotate grants do.
type e2eExecutor struct {
	provider *e2eProviderStub
	// crashAfterAnswer makes the first refresh die once the provider answered.
	crashAfterAnswer bool
	calls            atomic.Int32
	unknown          atomic.Int32
	crashed          atomic.Bool
}

func (e *e2eExecutor) Identifier() string { return e2eProvider }

func (e *e2eExecutor) Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *e2eExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *e2eExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *e2eExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *e2eExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	e.calls.Add(1)
	journal, ok := codex.RefreshJournalFromContext(ctx)
	if !ok {
		return nil, errors.New("no refresh journal bound")
	}
	crash := e.crashAfterAnswer && e.crashed.CompareAndSwap(false, true)
	if crash {
		journal = crashingReceipts{journal}
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	grant, err := codex.JournalRefresh(ctx, journal, e2eProvider, refreshToken, e.provider.send, func(value ojson.Result) (*e2eGrant, error) {
		return &e2eGrant{
			AccessToken:  value.Get("access_token").String(),
			RefreshToken: value.Get("refresh_token").String(),
			Expired:      value.Get("expired").String(),
		}, nil
	})
	if crash {
		// The process is gone: nothing answers the request, which ends with this
		// replica's context.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		if codex.IsRefreshOutcomeUnknown(err) {
			e.unknown.Add(1)
		}
		return nil, err
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = grant.AccessToken
	updated.Metadata["refresh_token"] = grant.RefreshToken
	updated.Metadata["expires_at"] = grant.Expired
	return updated, nil
}

// e2eReplica is one process: a store, a manager over it, and the refresh loop.
type e2eReplica struct {
	store   *Store
	manager *cliproxyauth.Manager
	exec    *e2eExecutor
	cancel  context.CancelFunc
}

func (r *e2eReplica) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	r.store.StartSync(ctx)
	r.manager.StartAutoRefresh(ctx, time.Second)
}

func newE2EReplica(t *testing.T, f *storeFixture, provider *e2eProviderStub, crashAfterAnswer bool) *e2eReplica {
	t.Helper()
	store := f.open()
	exec := &e2eExecutor{provider: provider, crashAfterAnswer: crashAfterAnswer}
	manager := cliproxyauth.NewManager(store, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(exec)
	return &e2eReplica{store: store, manager: manager, exec: exec}
}

func accessTokenOf(store *Store, id string) string {
	auths, err := store.List(context.Background())
	if err != nil {
		return ""
	}
	for _, auth := range auths {
		if auth.ID == id {
			token, _ := auth.Metadata["access_token"].(string)
			return token
		}
	}
	return ""
}

func registerExpiredCredential(t *testing.T, r *e2eReplica, id string) {
	t.Helper()
	_, err := r.manager.Register(context.Background(), &cliproxyauth.Auth{
		ID:              id,
		FileName:        id,
		Provider:        e2eProvider,
		Status:          cliproxyauth.StatusActive,
		LastRefreshedAt: time.Now().Add(-time.Hour),
		Metadata: map[string]any{
			"type":                     e2eProvider,
			"access_token":             "expired-access",
			"refresh_token":            "expiring-refresh",
			"expires_at":               time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"refresh_interval_seconds": 600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Both replicas see the credential expire. The provider is asked once, and the
// other replica adopts the result through the bucket it follows.
func TestTwoReplicasRefreshAnExpiringCredentialOnce(t *testing.T) {
	f := newStoreFixture(t)
	provider := &e2eProviderStub{}
	a := newE2EReplica(t, f, provider, false)
	b := newE2EReplica(t, f, provider, false)
	registerExpiredCredential(t, a, "e2e.json")
	b.store.StartSync(context.Background())
	waitFor(t, "replica B to see the credential", func() bool { return accessTokenOf(b.store, "e2e.json") == "expired-access" })
	if err := b.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	a.start(t)
	b.start(t)
	waitFor(t, "replica A to save the refreshed tokens", func() bool { return accessTokenOf(a.store, "e2e.json") == "new-access" })
	waitFor(t, "replica B to adopt the refreshed tokens", func() bool { return accessTokenOf(b.store, "e2e.json") == "new-access" })

	time.Sleep(3 * time.Second)
	if got := provider.requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want exactly 1", got)
	}
	if got := a.exec.calls.Load() + b.exec.calls.Load(); got != 1 {
		t.Fatalf("executor refreshes = %d, want exactly 1", got)
	}
}

// A replica dies after the provider answered and before the receipt was saved.
// The request is redelivered to the other replica, which must find the grant
// consumed with an unknown outcome and never send it again.
func TestRedeliveryAfterACrashNeverReplaysTheGrant(t *testing.T) {
	f := newStoreFixture(t)
	provider := &e2eProviderStub{}
	a := newE2EReplica(t, f, provider, true)
	b := newE2EReplica(t, f, provider, false)
	registerExpiredCredential(t, a, "e2e.json")
	b.store.StartSync(context.Background())
	waitFor(t, "replica B to see the credential", func() bool { return accessTokenOf(b.store, "e2e.json") == "expired-access" })
	if err := b.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Only A runs first, so it is the one that wins the request and dies.
	a.start(t)
	waitFor(t, "the provider to answer replica A", func() bool { return provider.requests.Load() == 1 && a.exec.crashed.Load() })
	b.start(t)
	a.cancel()

	waitFor(t, "the redelivered request to reach replica B", func() bool { return b.exec.unknown.Load() >= 1 })
	time.Sleep(2 * time.Second)
	if got := provider.requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want the grant sent exactly once", got)
	}
	if token := accessTokenOf(b.store, "e2e.json"); token != "expired-access" {
		t.Fatalf("access token = %q, want the credential left as it was", token)
	}
}
