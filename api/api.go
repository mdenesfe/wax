// Package api exposes the instance manager over a JSON REST API.
//
// All endpoints except /healthz require "Authorization: Bearer <key>".
// Errors are returned as {"error": "..."} with a matching status code.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
)

// maxBody bounds request bodies (base64 media included).
const maxBody = 90 << 20

// Server serves the API.
type Server struct {
	m      *manager.Manager
	store  *store.Store
	apiKey []byte
	log    *slog.Logger
	mux    *http.ServeMux
}

// New builds the API handler.
func New(m *manager.Manager, st *store.Store, apiKey string, log *slog.Logger) *Server {
	s := &Server{m: m, store: st, apiKey: []byte(apiKey), log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /v1/instances":                                       s.createInstance,
		"GET /v1/instances":                                        s.listInstances,
		"GET /v1/instances/{id}":                                   s.getInstance,
		"PATCH /v1/instances/{id}":                                 s.updateInstance,
		"DELETE /v1/instances/{id}":                                s.deleteInstance,
		"POST /v1/instances/{id}/connect":                          s.connect,
		"GET /v1/instances/{id}/qr":                                s.qr,
		"POST /v1/instances/{id}/messages":                         s.sendMessage,
		"GET /v1/instances/{id}/outbox/{oid}":                      s.outboxStatus,
		"GET /v1/instances/{id}/chats":                             s.listChats,
		"GET /v1/instances/{id}/chats/{chat}/messages":             s.listMessages,
		"GET /v1/instances/{id}/chats/{chat}/messages/{mid}/media": s.downloadMedia,
		"POST /v1/instances/{id}/read":                             s.markRead,
		"POST /v1/instances/{id}/presence":                         s.presence,
		"GET /v1/instances/{id}/groups/{group}":                    s.groupInfo,
		"POST /v1/instances/{id}/calls/{call}/reject":              s.rejectCall,
	} {
		s.mux.Handle(pattern, s.authorized(h))
	}
}

// ServeHTTP implements http.Handler with panic isolation per request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("api handler panicked", "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
			writeError(w, http.StatusInternalServerError, errors.New("internal error"))
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	s.mux.ServeHTTP(w, r)
}

func (s *Server) authorized(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), s.apiKey) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("invalid or missing API key"))
			return
		}
		next(w, r)
	})
}

// --- helpers ---------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// fail maps domain errors to HTTP status codes.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, manager.ErrExists), errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, manager.ErrInvalidID), errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, manager.ErrNotConnected):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		s.log.Error("api error", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("internal error"))
	}
}

var errBadRequest = errors.New("bad request")

func badRequest(msg string) error { return errorf(errBadRequest, msg) }

type wrapped struct {
	base error
	msg  string
}

func (e wrapped) Error() string { return e.msg }
func (e wrapped) Unwrap() error { return e.base }

func errorf(base error, msg string) error { return wrapped{base, msg} }

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: " + err.Error())
	}
	return nil
}

func intQuery(r *http.Request, key string, def, maxV int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || v <= 0 {
		return def
	}
	return min(v, maxV)
}

func (s *Server) instance(r *http.Request) (*manager.Instance, error) {
	return s.m.Get(r.PathValue("id"))
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
