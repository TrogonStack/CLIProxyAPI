package handlers

import (
	"context"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestPublishAllowedAuthIDsResolvesRefsAndFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	for id, file := range map[string]string{"publish-a": "publish-a.json", "publish-b": "publish-b.json"} {
		if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, FileName: file, Provider: "codex", Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

	newCtx := func(refs []string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if refs != nil {
			c.Set(sdkaccess.AllowedAuthsContextKey, refs)
		}
		return c
	}

	c := newCtx([]string{"publish-a.json", "publish-b"})
	h.PublishAllowedAuthIDs(c)
	ids, restricted := sdkaccess.AllowedAuthIDsFromGin(c)
	sort.Strings(ids)
	if !restricted || len(ids) != 2 || ids[0] != "publish-a" || ids[1] != "publish-b" {
		t.Fatalf("ids = %v restricted = %v", ids, restricted)
	}

	c = newCtx([]string{"missing.json"})
	h.PublishAllowedAuthIDs(c)
	if ids, restricted = sdkaccess.AllowedAuthIDsFromGin(c); !restricted || len(ids) != 0 {
		t.Fatalf("dangling ref: ids = %v restricted = %v, want restricted and empty", ids, restricted)
	}

	// A restricted request whose IDs were never published must read as "nothing allowed".
	if ids, restricted = sdkaccess.AllowedAuthIDsFromGin(newCtx([]string{"publish-a.json"})); !restricted || len(ids) != 0 {
		t.Fatalf("unpublished: ids = %v restricted = %v", ids, restricted)
	}

	// A later unrestricted turn on the same connection clears the earlier IDs.
	c = newCtx([]string{"publish-a.json"})
	h.PublishAllowedAuthIDs(c)
	c.Set(sdkaccess.AllowedAuthsContextKey, nil)
	h.PublishAllowedAuthIDs(c)
	if _, restricted = sdkaccess.AllowedAuthIDsFromGin(c); restricted {
		t.Fatal("an unrestricted turn must not stay restricted")
	}
}
