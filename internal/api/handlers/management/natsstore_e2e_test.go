package management

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// replica is one process-like participant: its own spool, store and handler.
type replica struct {
	store  *natsstore.Store
	router *gin.Engine
	config jetstream.KeyValue
}

type natsCluster struct {
	t        *testing.T
	url      string
	example  string
	settings natsstore.StoreSettings
}

func newNatsCluster(t *testing.T) *natsCluster {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := natstest.Start(t)
	example, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	settings := natsstore.DefaultStoreSettings()
	settings.Prefix = "e2e"
	settings.Create = natsstore.CreateAuto
	return &natsCluster{t: t, url: srv.URL, example: example, settings: settings}
}

// join starts a replica. A replica that does not sync only sees the buckets
// as they were when it joined, which makes it stale on purpose.
func (n *natsCluster) join(sync bool) *replica {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := func(key string) (string, bool) {
		if key == natsstore.EnvURL {
			return n.url, true
		}
		return "", false
	}
	nc, err := natsstore.Connect(ctx, natsstore.SettingsFromEnv(env).Resolve(), env, 5*time.Second)
	if err != nil {
		n.t.Fatal(err)
	}
	boot, err := natsstore.Bootstrap(ctx, nc, n.settings)
	if err != nil {
		nc.Close()
		n.t.Fatal(err)
	}
	store, err := natsstore.NewStore(nc, boot, n.settings, n.t.TempDir())
	if err != nil {
		nc.Close()
		n.t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, n.example); err != nil {
		_ = store.Close()
		n.t.Fatal(err)
	}
	if sync {
		store.StartSync(context.Background())
	}
	n.t.Cleanup(func() { _ = store.Close() })

	cfg, err := config.LoadConfig(store.ConfigPath())
	if err != nil {
		n.t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: store.ConfigPath(), tokenStore: store}
	router := gin.New()
	router.PUT("/debug", h.PutDebug)
	return &replica{
		store:  store,
		router: router,
		config: boot.Bucket(n.settings, natsstore.BucketConfig),
	}
}

func (r *replica) setDebug(value string) int {
	return putJSON(r.router, "/debug", `{"value":`+value+`}`).Code
}

func (r *replica) configText() string {
	data, err := os.ReadFile(r.store.ConfigPath())
	if err != nil {
		return ""
	}
	return string(data)
}

func (r *replica) debugOn() bool {
	for _, line := range strings.Split(r.configText(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "debug:") {
			return strings.Contains(line, "true")
		}
	}
	return false
}

func (r *replica) configRevision(t *testing.T) uint64 {
	t.Helper()
	key, err := natsstore.EncodeKey("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := r.config.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return entry.Revision()
}

func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// watchDir counts filesystem events under dir, so a test can tell a quiet
// spool from one that keeps being rewritten.
func watchDir(t *testing.T, dir string) func() int {
	t.Helper()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Add(dir); err != nil {
		t.Fatal(err)
	}
	events := make(chan struct{}, 1024)
	go func() {
		for range w.Events {
			events <- struct{}{}
		}
	}()
	return func() int { return len(events) }
}

func TestNatsStoreReplicasStayInSync(t *testing.T) {
	cluster := newNatsCluster(t)
	a, b := cluster.join(true), cluster.join(true)
	ctx := context.Background()

	t.Run("config edits propagate without echoing", func(t *testing.T) {
		if a.debugOn() || b.debugOn() {
			t.Fatal("debug starts enabled")
		}
		if code := a.setDebug("true"); code != http.StatusOK {
			t.Fatalf("edit status = %d", code)
		}
		eventually(t, "the edit to reach the other replica", b.debugOn)

		// Both spools and the bucket then stay still: nothing bounces back.
		rev := a.configRevision(t)
		aEvents := watchDir(t, filepath.Dir(a.store.ConfigPath()))
		bEvents := watchDir(t, filepath.Dir(b.store.ConfigPath()))
		time.Sleep(2 * time.Second)
		if got := a.configRevision(t); got != rev {
			t.Fatalf("config revision moved %d -> %d with no edit", rev, got)
		}
		if n := aEvents() + bEvents(); n != 0 {
			t.Fatalf("spool saw %d events with no edit", n)
		}

		if code := b.setDebug("false"); code != http.StatusOK {
			t.Fatalf("edit status = %d", code)
		}
		eventually(t, "the reverse edit to reach the first replica", func() bool { return !a.debugOn() })
	})

	t.Run("credential changes and deletes propagate", func(t *testing.T) {
		const id = "team/e2e@example.com.json"
		auth := &coreauth.Auth{ID: id, FileName: id, Provider: "test", Metadata: map[string]any{"type": "test", "access_token": "tok-e2e"}}
		if _, err := a.store.Save(ctx, auth); err != nil {
			t.Fatal(err)
		}
		remote := filepath.Join(b.store.AuthDir(), "team", "e2e@example.com.json")
		eventually(t, "the credential to reach the other replica", func() bool {
			_, err := os.Stat(remote)
			return err == nil
		})
		if err := a.store.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the delete to reach the other replica", func() bool {
			_, err := os.Stat(remote)
			return os.IsNotExist(err)
		})
	})

	t.Run("a stale edit is refused with 409 and adopts the winner", func(t *testing.T) {
		stale := cluster.join(false)
		if code := a.setDebug("true"); code != http.StatusOK {
			t.Fatalf("edit status = %d", code)
		}
		eventually(t, "the edit to reach the synced replica", b.debugOn)
		if code := stale.setDebug("true"); code != http.StatusConflict {
			t.Fatalf("stale edit status = %d, want 409", code)
		}
		if !stale.debugOn() {
			t.Fatal("the stale replica did not adopt the winning config")
		}
		if code := stale.setDebug("false"); code != http.StatusOK {
			t.Fatalf("retry status = %d, want 200", code)
		}
		eventually(t, "the retry to reach the synced replicas", func() bool { return !a.debugOn() && !b.debugOn() })
	})
}
