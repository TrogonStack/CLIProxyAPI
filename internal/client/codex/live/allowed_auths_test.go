package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func registerBindingCredentials(t *testing.T) *auth.Manager {
	t.Helper()
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	for _, id := range []string{"bound-a", "bound-b"} {
		registerCredential(t, manager, &auth.Auth{
			ID:       id,
			Provider: "codex",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "token-" + id},
		})
	}
	return manager
}

// restrictTo mimics the access middleware: the authenticated key's credential
// restriction lives on the gin context and nowhere else.
func restrictTo(refs ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(sdkaccess.AllowedAuthsContextKey, refs)
		c.Next()
	}
}

func TestHandleDirectWebsocketHonorsTheKeyBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := make(chan string, 32)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		tokens <- request.Header.Get("Authorization")
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
	}))
	defer upstreamServer.Close()

	manager := registerBindingCredentials(t)
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime", restrictTo("bound-a"), handler.HandleRealtimeWebsocket)
	downstream := httptest.NewServer(router)
	defer downstream.Close()

	wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/realtime?model=gpt-realtime"
	const dials = 6
	for i := 0; i < dials; i++ {
		connection, response, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
		if errDial != nil {
			t.Fatalf("dial %d: %v", i, errDial)
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		_, _, _ = connection.ReadMessage()
		_ = connection.Close()
	}
	for i := 0; i < dials; i++ {
		select {
		case got := <-tokens:
			if got != "Bearer token-bound-a" {
				t.Fatalf("websocket %d was served with %q, want only bound-a", i, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("upstream saw %d of %d websockets", i, dials)
		}
	}
}

func TestHandleSidebandHonorsTheKeyBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamDials atomic.Int32
	tokens := make(chan string, 8)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		upstreamDials.Add(1)
		tokens <- request.Header.Get("Authorization")
		_ = connection.WriteMessage(websocket.TextMessage, []byte("hello"))
	}))
	defer upstreamServer.Close()

	manager := registerBindingCredentials(t)
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime/calls/:call_id", restrictTo("bound-a"), handler.HandleSideband)
	downstream := httptest.NewServer(router)
	defer downstream.Close()
	base := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/realtime/calls/"

	// A call pinned to a credential outside the key's set must not be joined.
	handler.sessions.put("call-outside", liveSession{authID: "bound-b", model: defaultLiveModel})
	connection, response, errDial := websocket.DefaultDialer.Dial(base+"call-outside", nil)
	if errDial == nil {
		_ = connection.Close()
		t.Fatal("a bound key joined a call pinned to a credential outside its set")
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if got := upstreamDials.Load(); got != 0 {
		t.Fatalf("upstream was dialed %d times for an out-of-set credential", got)
	}

	// Control: a call pinned to the key's own credential is joined.
	handler.sessions.put("call-inside", liveSession{authID: "bound-a", model: defaultLiveModel})
	connection, response, errDial = websocket.DefaultDialer.Dial(base+"call-inside", nil)
	if errDial != nil {
		t.Fatalf("dial in-set call: %v", errDial)
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	_, _, _ = connection.ReadMessage()
	_ = connection.Close()
	select {
	case got := <-tokens:
		if got != "Bearer token-bound-a" {
			t.Fatalf("sideband was served with %q, want bound-a", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-set sideband never reached upstream")
	}
}
