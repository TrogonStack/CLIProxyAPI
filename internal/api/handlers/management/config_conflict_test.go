package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// committingStore is a shared token store whose config commit result is scripted.
type committingStore struct {
	memoryAuthStore
	commitErr error
	commits   int
}

func (s *committingStore) CommitConfig(context.Context) error {
	s.commits++
	return s.commitErr
}

func newCommitHandler(t *testing.T, store coreauth.Store) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\nserver: {port: 8317}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path, tokenStore: store}
	router := gin.New()
	router.PUT("/debug", h.PutDebug)
	router.PUT("/config.yaml", h.PutConfigYAML)
	return h, router
}

func putJSON(router *gin.Engine, url, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, url, strings.NewReader(body)))
	return rec
}

func TestConfigWriteAnswers409OnSharedStoreConflict(t *testing.T) {
	conflict := fmt.Errorf("commit: %w", coreauth.ErrStoreConflict)
	store := &committingStore{commitErr: conflict}
	_, router := newCommitHandler(t, store)

	if rec := putJSON(router, "/debug", `{"value":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("scalar edit status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if rec := putJSON(router, "/config.yaml", "port: 8317\n"); rec.Code != http.StatusConflict {
		t.Fatalf("yaml put status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if store.commits != 2 {
		t.Fatalf("commits = %d, want 2", store.commits)
	}
}

func TestConfigWriteSucceedsWhenSharedStoreCommits(t *testing.T) {
	store := &committingStore{}
	_, router := newCommitHandler(t, store)
	if rec := putJSON(router, "/debug", `{"value":true}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if store.commits != 1 {
		t.Fatalf("commits = %d, want 1", store.commits)
	}
}

func TestConfigWriteReportsOtherCommitFailuresAs500(t *testing.T) {
	_, router := newCommitHandler(t, &committingStore{commitErr: errors.New("bucket unavailable")})
	if rec := putJSON(router, "/debug", `{"value":true}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
}

func TestConfigWriteIgnoresStoresThatAreNotShared(t *testing.T) {
	_, router := newCommitHandler(t, &memoryAuthStore{})
	if rec := putJSON(router, "/debug", `{"value":true}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}
