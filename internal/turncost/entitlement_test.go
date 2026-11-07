package turncost

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func testEntitlements(t *testing.T) (*entitlements, *time.Time, string) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "state", DefaultStateFileName)
	e := newEntitlements(func() time.Time { return now })
	e.configure(path, time.Hour)
	return e, &now, path
}

func TestEntitlementsPersistRoundTrip(t *testing.T) {
	e, now, path := testEntitlements(t)
	e.set("acct-a", statusForbidden)
	e.set("acct-b", statusAllowed)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "acct-a") || strings.Contains(string(data), "acct-b") {
		t.Fatalf("state file holds a raw account id: %s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v", info.Mode().Perm())
	}
	if entries, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	reloaded := newEntitlements(func() time.Time { return *now })
	reloaded.configure(path, time.Hour)
	if got := reloaded.status("acct-a"); got != statusForbidden {
		t.Fatalf("acct-a = %q", got)
	}
	if got := reloaded.status("acct-b"); got != statusAllowed {
		t.Fatalf("acct-b = %q", got)
	}
	if got := reloaded.status("acct-c"); got != "" {
		t.Fatalf("acct-c = %q, want unknown", got)
	}
}

func TestEntitlementsTolerateMissingAndCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	e := newEntitlements(time.Now)
	e.configure(missing, time.Hour)
	if got := e.status("acct"); got != "" {
		t.Fatalf("status = %q", got)
	}

	for name, content := range map[string]string{
		"garbage.json":   "{not json",
		"version.json":   `{"version":99,"accounts":{}}`,
		"badstatus.json": `{"version":1,"accounts":{"` + accountKey("acct") + `":{"status":"weird","checked_at":"2099-01-01T00:00:00Z"}}}`,
		"badkey.json":    `{"version":1,"accounts":{"acct":{"status":"forbidden","checked_at":"2099-01-01T00:00:00Z"}}}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		e := newEntitlements(time.Now)
		e.configure(path, time.Hour)
		if got := e.status("acct"); got != "" {
			t.Errorf("%s: status = %q, want unknown", name, got)
		}
		e.set("acct", statusForbidden)
		if got := e.status("acct"); got != statusForbidden {
			t.Errorf("%s: set after corrupt load = %q", name, got)
		}
	}
}

func TestEntitlementsExpire(t *testing.T) {
	e, now, path := testEntitlements(t)
	e.set("acct", statusForbidden)
	*now = now.Add(time.Hour - time.Second)
	if got := e.status("acct"); got != statusForbidden {
		t.Fatalf("before ttl = %q", got)
	}
	*now = now.Add(2 * time.Second)
	if got := e.status("acct"); got != "" {
		t.Fatalf("after ttl = %q, want unknown", got)
	}

	// Expired entries are not restored from disk either.
	reloaded := newEntitlements(func() time.Time { return *now })
	reloaded.configure(path, time.Hour)
	if got := reloaded.status("acct"); got != "" {
		t.Fatalf("reloaded after ttl = %q", got)
	}
}

func TestEntitlementsAreBounded(t *testing.T) {
	e := newEntitlements(time.Now)
	for i := 0; i < maxEntitlements+10; i++ {
		e.set("acct-"+itoa(i), statusAllowed)
	}
	if len(e.entries) > maxEntitlements {
		t.Fatalf("entries = %d, want at most %d", len(e.entries), maxEntitlements)
	}
}

func TestAllowedAccountFlipsToForbiddenOn403AndIgnores401(t *testing.T) {
	status := http.StatusOK
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, `{"threads":[{"thread_id":"th-1","turns":[{"turn_id":"turn-1","estimated_usage_usd_micros":5,"settled_response_ids":["r1"]}]}]}`)
	})
	s := upstreamService(t, u, codexAuth("auth-a", "tok-a", "acct-a"))
	s.HandleUsage(context.Background(), record("auth-a", "th-1", "turn-1", "r1", 1_000_000))

	if got := s.Query(context.Background(), []string{"turn-1"})[0]; got.TotalUSD != "0.000005" {
		t.Fatalf("Query() = %+v", got)
	}
	if got := s.entitlements.status("acct-a"); got != statusAllowed {
		t.Fatalf("after 200 = %q", got)
	}

	status = http.StatusUnauthorized
	s.Query(context.Background(), []string{"turn-1"})
	if got := s.entitlements.status("acct-a"); got != statusAllowed {
		t.Fatalf("after 401 = %q, want allowed untouched", got)
	}

	status = http.StatusForbidden
	s.Query(context.Background(), []string{"turn-1"})
	if got := s.entitlements.status("acct-a"); got != statusForbidden {
		t.Fatalf("after 403 = %q, want forbidden", got)
	}
	hits := u.hits.Load()
	s.Query(context.Background(), []string{"turn-1"})
	if u.hits.Load() != hits {
		t.Fatal("forbidden account was queried again")
	}
}

func TestForbiddenSurvivesRestartAndReaddedAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultStateFileName)
	cfg := &config.Config{CodexTurnCosts: config.CodexTurnCostsConfig{
		Enabled: true, UpstreamEstimates: true, StateFile: path, ForbiddenTTL: "48h",
	}}
	u := newFakeUpstream(t, func(_ upstreamCall, w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) })

	first := upstreamService(t, u, codexAuth("auth-old", "tok", "acct-a"))
	first.Configure(cfg)
	first.HandleUsage(context.Background(), record("auth-old", "th-1", "turn-1", "r1", 1_000_000))
	first.Query(context.Background(), []string{"turn-1"})
	if hits := u.hits.Load(); hits != 1 {
		t.Fatalf("hits = %d", hits)
	}

	// A new process and the same account under a different auth id: no further probe.
	second := upstreamService(t, u, codexAuth("auth-new", "tok", "acct-a"))
	second.Configure(cfg)
	second.HandleUsage(context.Background(), record("auth-new", "th-1", "turn-2", "r2", 1_000_000))
	if got := second.Query(context.Background(), []string{"turn-2"})[0]; got.Status != statusPriced || len(got.Responses) != 1 {
		t.Fatalf("Query() = %+v, want local price", got)
	}
	if hits := u.hits.Load(); hits != 1 {
		t.Fatalf("hits = %d, want the persisted forbidden to skip the probe", hits)
	}
}

func TestStateFilePathDefaultsNextToAuthDir(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{AuthDir: filepath.Join(base, "auths")}
	if got, want := stateFilePath(cfg), filepath.Join(base, DefaultStateFileName); got != want {
		t.Fatalf("stateFilePath() = %q, want %q", got, want)
	}
	cfg.CodexTurnCosts.StateFile = "/explicit/state.json"
	if got := stateFilePath(cfg); got != "/explicit/state.json" {
		t.Fatalf("explicit = %q", got)
	}
	if got := stateFilePath(&config.Config{}); got != "" {
		t.Fatalf("no auth dir = %q, want persistence off", got)
	}
}

func TestForbiddenTTLDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":      DefaultForbiddenTTL,
		"bogus": DefaultForbiddenTTL,
		"-1h":   DefaultForbiddenTTL,
		"48h":   48 * time.Hour,
	} {
		if got := (config.CodexTurnCostsConfig{ForbiddenTTL: in}).ForbiddenTTLDuration(); got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}
