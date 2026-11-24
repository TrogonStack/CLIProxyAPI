package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type allowedAuthsFixture struct {
	manager  *Manager
	executor *afterAuthTestExecutor
	provider string
	model    string
}

// newAllowedAuthsFixture registers cred-a (best priority), cred-b and cred-c, each with a file name.
func newAllowedAuthsFixture(t *testing.T, selector Selector) *allowedAuthsFixture {
	t.Helper()
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	provider := "allowed-auths-" + name
	model := "allowed-auths-model-" + name
	executor := &afterAuthTestExecutor{id: provider, failByAuth: map[string]error{}}
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(3, 0, 10)
	manager.RegisterExecutor(executor)
	for i, id := range []string{"cred-a", "cred-b", "cred-c"} {
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		_, errReg := manager.Register(WithSkipPersist(context.Background()), &Auth{
			ID: id, FileName: id + ".json", Provider: provider, Status: StatusActive,
			Attributes: map[string]string{"priority": fmt.Sprint(10 - i)},
		})
		if errReg != nil {
			t.Fatal(errReg)
		}
	}
	return &allowedAuthsFixture{manager: manager, executor: executor, provider: provider, model: model}
}

func (f *allowedAuthsFixture) run(mode string, allowed any) ([]byte, error) {
	opts := cliproxyexecutor.Options{}
	if allowed != nil {
		opts.Metadata = map[string]any{cliproxyexecutor.AllowedAuthsMetadataKey: allowed}
	}
	return invokeAfterAuthMode(f.manager, f.provider, f.model, mode, opts)
}

var errUpstream500 = &Error{Code: "upstream", Message: "boom", Retryable: true, HTTPStatus: http.StatusInternalServerError}

func TestAllowedAuthsRestrictSelectionAcrossModesAndSelectors(t *testing.T) {
	selectors := map[string]func() Selector{
		"fill-first":  func() Selector { return &FillFirstSelector{} },
		"round-robin": func() Selector { return &RoundRobinSelector{} },
	}
	for selName, newSelector := range selectors {
		for _, mode := range []string{"execute", "count", "stream"} {
			t.Run(selName+"/"+mode, func(t *testing.T) {
				f := newAllowedAuthsFixture(t, newSelector())
				for range 4 {
					payload, err := f.run(mode, []string{"cred-b.json"})
					if err != nil {
						t.Fatal(err)
					}
					if len(payload) == 0 || string(payload[len(payload)-6:]) != "cred-b" {
						t.Fatalf("payload %q was not served by cred-b", payload)
					}
				}
				for _, id := range f.executor.Calls() {
					if id != "cred-b" {
						t.Fatalf("credential %s used outside the allowed set: %v", id, f.executor.Calls())
					}
				}
			})
		}
	}
}

func TestAllowedAuthsMatchByIDAndFileName(t *testing.T) {
	f := newAllowedAuthsFixture(t, &FillFirstSelector{})
	if _, err := f.run("execute", []string{"cred-c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run("execute", []string{"cred-b.json", "unknown"}); err != nil {
		t.Fatal(err)
	}
	if got, want := f.executor.Calls(), []string{"cred-c", "cred-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	// Matching is exact: no case folding and no basename matching.
	for _, ref := range []string{"CRED-B.JSON", "cred-b", "dir/cred-b.json"} {
		if ref == "cred-b" {
			continue // matches the ID, so it is a valid reference.
		}
		if _, err := f.run("execute", []string{ref}); err == nil {
			t.Fatalf("reference %q must not match", ref)
		}
	}
}

func TestAllowedAuthsFailoverNeverLeavesTheAllowedSet(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			f := newAllowedAuthsFixture(t, &FillFirstSelector{})
			f.executor.failByAuth["cred-b"] = errUpstream500
			payload, err := f.run(mode, []string{"cred-b.json", "cred-c.json"})
			if err != nil || string(payload[len(payload)-6:]) != "cred-c" {
				t.Fatalf("payload=%q err=%v, want failover to cred-c", payload, err)
			}

			f = newAllowedAuthsFixture(t, &FillFirstSelector{})
			f.executor.failByAuth["cred-b"] = errUpstream500
			f.executor.failByAuth["cred-c"] = errUpstream500
			if _, err = f.run(mode, []string{"cred-b.json", "cred-c.json"}); err == nil {
				t.Fatal("expected an error once every allowed credential failed")
			}
			for _, id := range f.executor.Calls() {
				if id == "cred-a" {
					t.Fatalf("failover escaped to cred-a: %v", f.executor.Calls())
				}
			}
		})
	}
}

func TestAllowedAuthsEmptyIntersectionFailsInsteadOfFallingBack(t *testing.T) {
	f := newAllowedAuthsFixture(t, &FillFirstSelector{})
	if _, err := f.run("execute", []string{"not-a-credential.json"}); err == nil {
		t.Fatal("expected an error when no credential matches the binding")
	}
	if calls := f.executor.Calls(); len(calls) != 0 {
		t.Fatalf("a credential was used: %v", calls)
	}
}

func TestAllowedAuthsUnparsableRestrictionFailsClosed(t *testing.T) {
	f := newAllowedAuthsFixture(t, &FillFirstSelector{})
	for name, restriction := range map[string]any{"non-string item": []any{1}, "wrong type": 7, "blank items": []string{" "}} {
		if _, err := f.run("execute", restriction); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	if calls := f.executor.Calls(); len(calls) != 0 {
		t.Fatalf("a credential was used: %v", calls)
	}
}

func TestUnboundRequestsAreUnchanged(t *testing.T) {
	f := newAllowedAuthsFixture(t, &FillFirstSelector{})
	for _, restriction := range []any{nil, []string{}, []any{}} {
		if _, err := f.run("execute", restriction); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := f.executor.Calls(), []string{"cred-a", "cred-a", "cred-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestAllowedAuthsPinnedOutsideTheSetIsRejected(t *testing.T) {
	f := newAllowedAuthsFixture(t, &FillFirstSelector{})
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.PinnedAuthMetadataKey:   "cred-a",
		cliproxyexecutor.AllowedAuthsMetadataKey: []string{"cred-b.json"},
	}}
	_, _, err := f.manager.pickNextLegacy(context.Background(), f.provider, f.model, opts, nil)
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("err = %v, want auth_not_found", err)
	}
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = "cred-b"
	if auth, _, errPick := f.manager.pickNextLegacy(context.Background(), f.provider, f.model, opts, nil); errPick != nil || auth.ID != "cred-b" {
		t.Fatalf("pinned allowed credential: auth=%v err=%v", auth, errPick)
	}
}

func TestAllowedAuthsSessionAffinityOwnerOutsideTheSetIsNotUsed(t *testing.T) {
	sticky := NewSessionAffinitySelector(&FillFirstSelector{})
	t.Cleanup(sticky.Stop)
	f := newAllowedAuthsFixture(t, sticky)
	headers := http.Header{"Session_id": {"bound-session"}}
	run := func(allowed []string) string {
		opts := cliproxyexecutor.Options{Headers: headers}
		if allowed != nil {
			opts.Metadata = map[string]any{cliproxyexecutor.AllowedAuthsMetadataKey: allowed}
		}
		payload, err := invokeAfterAuthMode(f.manager, f.provider, f.model, "execute", opts)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload[len(payload)-6:])
	}
	if got := run(nil); got != "cred-a" {
		t.Fatalf("unrestricted session served by %s, want cred-a", got)
	}
	// The same session now arrives with a key bound elsewhere: the sticky owner must not leak.
	for range 3 {
		if got := run([]string{"cred-b.json"}); got != "cred-b" {
			t.Fatalf("bound session served by %s, want cred-b", got)
		}
	}
}

func TestAllowedAuthsFallbackReadsGinContext(t *testing.T) {
	f := newAllowedAuthsFixture(t, &FillFirstSelector{})
	gc, _ := gin.CreateTestContext(nil)
	gc.Set(sdkaccess.AllowedAuthsContextKey, []string{"cred-c.json"})
	ctx := context.WithValue(context.Background(), "gin", gc)
	resp, err := f.manager.Execute(ctx, []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "ok:cred-c" {
		t.Fatalf("payload=%q err=%v, want cred-c", resp.Payload, err)
	}
	gc.Set(sdkaccess.AllowedAuthsContextKey, "unreadable")
	if _, err = f.manager.Execute(ctx, []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("an unreadable gin restriction must fail closed")
	}
}

func TestAuthAllowedByRefs(t *testing.T) {
	auth := &Auth{ID: "id-1", FileName: "team/work.json"}
	for refs, want := range map[string]bool{"": true, "id-1": true, "team/work.json": true, " team/work.json ": true, "work.json": false, "other": false} {
		var list []string
		if refs != "" {
			list = []string{refs}
		}
		if got := AuthAllowedByRefs(auth, list); got != want {
			t.Fatalf("AuthAllowedByRefs(%q) = %t, want %t", refs, got, want)
		}
	}
	if AuthAllowedByRefs(nil, []string{"x"}) {
		t.Fatal("nil auth must not be allowed")
	}
}

func TestHomeModeRefusesBoundKeys(t *testing.T) {
	provider := "home-bound-key"
	model := "home-bound-model"
	executor := &afterAuthTestExecutor{id: provider}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(executor)
	dispatcher := &retryContractHomeDispatcher{authIDs: []string{"cred-a"}}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.AllowedAuthsMetadataKey: []string{"cred-a.json"}}}
	_, err := invokeAfterAuthMode(manager, provider, model, "execute", opts)
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != "api_key_auth_binding_unsupported" || authErr.HTTPStatus != http.StatusForbidden {
		t.Fatalf("err = %v, want api_key_auth_binding_unsupported", err)
	}
	if calls := executor.Calls(); len(calls) != 0 {
		t.Fatalf("a credential was used: %v", calls)
	}
}

// A session binding made by any key, bound or not, is only a preference: a request from
// another key bound to a different set must never follow it out of that set.
func TestAllowedAuthsSessionAffinityBindingFromAnyKeyNeverLeaksIntoABoundSet(t *testing.T) {
	creators := map[string][]string{
		"unrestricted key":      nil,
		"key bound elsewhere":   {"cred-c.json"},
		"key bound to a subset": {"cred-a.json", "cred-c.json"},
	}
	for name, creatorAllowed := range creators {
		for _, mode := range []string{"execute", "count", "stream"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				sticky := NewSessionAffinitySelector(&FillFirstSelector{})
				t.Cleanup(sticky.Stop)
				f := newAllowedAuthsFixture(t, sticky)
				headers := http.Header{"Session_id": {"shared-session"}}
				run := func(allowed []string) (string, error) {
					opts := cliproxyexecutor.Options{Headers: headers}
					if allowed != nil {
						opts.Metadata = map[string]any{cliproxyexecutor.AllowedAuthsMetadataKey: allowed}
					}
					payload, err := invokeAfterAuthMode(f.manager, f.provider, f.model, mode, opts)
					if err != nil {
						return "", err
					}
					return string(payload[len(payload)-6:]), nil
				}
				owner, err := run(creatorAllowed)
				if err != nil {
					t.Fatal(err)
				}
				if owner == "cred-b" {
					t.Fatalf("creator %s was served by cred-b, the fixture needs the owner outside {cred-b}", name)
				}
				for range 3 {
					got, err := run([]string{"cred-b.json"})
					if err != nil || got != "cred-b" {
						t.Fatalf("bound request served by %q (err=%v), want cred-b", got, err)
					}
				}
				// The binding the bound key made must not drag the creator's own traffic out
				// of the creator's set either.
				if creatorAllowed != nil {
					got, err := run(creatorAllowed)
					if err != nil {
						t.Fatal(err)
					}
					if got == "cred-b" {
						t.Fatalf("creator restricted to %v was served by cred-b", creatorAllowed)
					}
				}
			})
		}
	}
}
