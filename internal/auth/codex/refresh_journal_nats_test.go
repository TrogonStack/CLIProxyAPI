package codex_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type stubTransport func(*http.Request) (*http.Response, error)

func (f stubTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// provider stands in for the token endpoint and counts what it is sent.
type provider struct {
	requests atomic.Int32
}

func (p *provider) transport() http.RoundTripper {
	return stubTransport(func(req *http.Request) (*http.Response, error) {
		p.requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
}

// crashingJournal loses the process between the claim and the receipt.
type crashingJournal struct {
	codex.RefreshJournal
}

func (crashingJournal) RefreshComplete(context.Context, string, []byte) error {
	return errors.New("replica crashed before saving the receipt")
}

// replicaStore joins the shared NATS server as its own replica.
func replicaStore(t *testing.T, url string, settings natsstore.StoreSettings) *natsstore.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := func(key string) (string, bool) {
		if key == natsstore.EnvURL {
			return url, true
		}
		return "", false
	}
	nc, err := natsstore.Connect(ctx, natsstore.SettingsFromEnv(env).Resolve(), env, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := natsstore.Bootstrap(ctx, nc, settings)
	if err != nil {
		nc.Close()
		t.Fatal(err)
	}
	store, err := natsstore.NewStore(nc, boot, settings, t.TempDir())
	if err != nil {
		nc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func journalSettings() natsstore.StoreSettings {
	settings := natsstore.DefaultStoreSettings()
	settings.Prefix = "journal"
	settings.Create = natsstore.CreateAuto
	return settings
}

// Two replicas see the same expiring credential. The provider must be asked
// once, and the replica that lost the claim must adopt the saved tokens.
func TestTwoReplicasRefreshTheSameCredentialOnce(t *testing.T) {
	srv := natstest.Start(t)
	settings := journalSettings()
	replicaA := replicaStore(t, srv.URL, settings)
	replicaB := replicaStore(t, srv.URL, settings)
	if !coreauthSharesJournal(replicaA) {
		t.Fatal("the NATS store does not declare a shared refresh journal")
	}

	stub := &provider{}
	auth := codex.NewCodexAuthWithTransport(stub.transport())
	first, err := auth.RefreshTokens(codex.WithRefreshJournal(context.Background(), replicaA), "expiring-refresh")
	if err != nil || first.AccessToken != "new-access" {
		t.Fatalf("replica A refresh = %+v, %v", first, err)
	}
	adopted, err := auth.RefreshTokens(codex.WithRefreshJournal(context.Background(), replicaB), "expiring-refresh")
	if err != nil {
		t.Fatalf("replica B refresh = %v", err)
	}
	if adopted.AccessToken != first.AccessToken || adopted.RefreshToken != first.RefreshToken || adopted.Expire != first.Expire {
		t.Fatalf("replica B did not adopt the receipt: %+v vs %+v", adopted, first)
	}
	if got := stub.requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want exactly 1", got)
	}
}

// A replica that dies after claiming leaves a grant nobody may send again, because
// the provider may already have rotated it.
func TestCrashBetweenClaimAndReceiptNeverReplaysTheGrant(t *testing.T) {
	srv := natstest.Start(t)
	settings := journalSettings()
	replicaA := replicaStore(t, srv.URL, settings)
	replicaB := replicaStore(t, srv.URL, settings)

	stub := &provider{}
	auth := codex.NewCodexAuthWithTransport(stub.transport())
	crashed := codex.WithRefreshJournal(context.Background(), crashingJournal{replicaA})
	// The provider answered, but the receipt was never saved.
	if _, err := auth.RefreshTokens(crashed, "crashing-refresh"); err == nil {
		t.Fatal("replica A reported success without saving a receipt")
	}
	for name, store := range map[string]*natsstore.Store{"replica A after restart": replicaA, "replica B": replicaB} {
		_, err := auth.RefreshTokens(codex.WithRefreshJournal(context.Background(), store), "crashing-refresh")
		if !codex.IsRefreshOutcomeUnknown(err) {
			t.Fatalf("%s = %v, want refresh_outcome_unknown", name, err)
		}
	}
	if got := stub.requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want exactly the first", got)
	}
}

func coreauthSharesJournal(store *natsstore.Store) bool {
	shared, ok := any(store).(coreauth.SharedRefreshJournal)
	return ok && shared.SharesRefreshJournal()
}
