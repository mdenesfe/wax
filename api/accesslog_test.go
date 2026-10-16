package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAccessLog: changes are logged at info level with the caller, route
// and instance; reads only at debug level; paths (which may hold chat
// addresses) never.
func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{log: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	mux := http.NewServeMux()
	var routed *http.Request
	handle := func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		c.who, c.tenant = "key 0a1b2c3d4e5f6071", "acme"
		routed = r
	}
	mux.HandleFunc("DELETE /v1/instances/{id}", handle)
	mux.HandleFunc("GET /v1/instances/{id}/chats/{chat}/messages", handle)
	call := func(method, path string) {
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, &caller{}))
		mux.ServeHTTP(httptest.NewRecorder(), req)
		s.logRequest(routed, callerOf(routed), http.StatusNoContent, 3*time.Millisecond)
	}

	call("GET", "/v1/instances/support/chats/905550000001@s.whatsapp.net/messages")
	if buf.Len() != 0 {
		t.Fatalf("a read was logged at info level: %s", buf.String())
	}
	call("DELETE", "/v1/instances/support")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	want := map[string]any{"msg": "api request", "level": "INFO", "route": "DELETE /v1/instances/{id}", "status": float64(204),
		"caller": "key 0a1b2c3d4e5f6071", "tenant": "acme", "path_id": "support"}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %v", k, rec[k], v)
		}
	}
	if strings.Contains(buf.String(), "905550000001") {
		t.Error("the log holds a chat address")
	}
}
