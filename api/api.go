// Package api exposes the instance manager over a JSON REST API.
//
// All endpoints except /healthz require "Authorization: Bearer <key>".
// Two kinds of keys exist:
//
//   - the admin key (WAX_API_KEY) manages tenants and their API keys only;
//   - a tenant key (issued through the admin API) manages that tenant's
//     instances only. Another tenant's instance is reported as 404, so its
//     existence is not revealed.
//
// Errors are returned as {"error": "..."} with a matching status code.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
)

// maxBody bounds request bodies (base64 media included).
const maxBody = 90 << 20

// Large bodies (media sends) are decoded, re-encoded and sealed, holding a
// few copies in memory; at most maxLargeRequests run at once so parallel
// uploads cannot exhaust memory.
const (
	largeBody        = 1 << 20
	maxLargeRequests = 4
)

// Server serves the API.
type Server struct {
	m      *manager.Manager
	store  *store.Store
	apiKey []byte
	log    *slog.Logger
	mux    *http.ServeMux
	large  chan struct{} // semaphore for large request bodies
	// nodeHTTP forwards requests to other nodes; it shares connections.
	nodeHTTP http.RoundTripper
}

// New builds the API handler.
func New(m *manager.Manager, st *store.Store, apiKey string, log *slog.Logger) *Server {
	s := &Server{m: m, store: st, apiKey: []byte(apiKey), log: log, mux: http.NewServeMux(), large: make(chan struct{}, maxLargeRequests),
		nodeHTTP: &http.Transport{ResponseHeaderTimeout: 5 * time.Minute, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 16}}
	s.routes()
	return s
}

// instanceHandler serves a request for one instance, already resolved
// within the caller's tenant.
type instanceHandler func(http.ResponseWriter, *http.Request, *manager.Instance)

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	admin := map[string]http.HandlerFunc{
		"POST /v1/tenants":                       s.createTenant,
		"GET /v1/tenants":                        s.listTenants,
		"DELETE /v1/tenants/{tenant}":            s.deleteTenant,
		"POST /v1/tenants/{tenant}/keys":         s.createKey,
		"GET /v1/tenants/{tenant}/keys":          s.listKeys,
		"DELETE /v1/tenants/{tenant}/keys/{key}": s.deleteKey,
	}
	for pattern, h := range admin {
		s.mux.Handle(pattern, s.authorized(roleAdmin, h))
	}
	tenant := map[string]http.HandlerFunc{
		"POST /v1/instances": s.createInstance,
		"GET /v1/instances":  s.listInstances,
	}
	for pattern, h := range tenant {
		s.mux.Handle(pattern, s.authorized(roleTenant, h))
	}
	scoped := map[string]instanceHandler{
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
	}
	for pattern, h := range scoped {
		s.mux.Handle(pattern, s.authorized(roleTenant, s.scoped(h)))
	}
}

// scoped resolves {id} within the caller's tenant. This is the only way an
// instance reaches a handler, so no handler can act on another tenant's
// instance. A request for an instance that another node runs is forwarded
// to that node (once: a forwarded request is never forwarded again).
func (s *Server) scoped(h instanceHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, err := s.m.Resolve(r.Context(), tenantOf(r), r.PathValue("id"))
		if err != nil {
			s.fail(w, err)
			return
		}
		if !in.Running() && r.Header.Get(forwardedHeader) == "" {
			owner, err := s.m.Owner(r.Context(), in)
			if err == nil && owner.Node != s.m.NodeID() {
				s.forward(w, r, owner)
				return
			}
		}
		h(w, r, in)
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

// role is what a route requires of the caller.
type role int

const (
	roleAdmin role = iota
	roleTenant
)

type tenantKey struct{}

// tenantOf returns the authenticated tenant of a tenant-key request.
func tenantOf(r *http.Request) string {
	t, _ := r.Context().Value(tenantKey{}).(string)
	return t
}

// authorized authenticates the bearer token and enforces the route's role.
// The admin key is compared in constant time; tenant keys are verified
// against the database on every request, so revocation is immediate.
func (s *Server) authorized(need role, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, errors.New("invalid or missing API key"))
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), s.apiKey) == 1 {
			if need != roleAdmin {
				writeError(w, http.StatusForbidden, errors.New("the admin key manages tenants only; use a tenant key"))
				return
			}
			next(w, r)
			return
		}
		tenant, err := s.store.VerifyAPIKey(r.Context(), token)
		switch {
		case errors.Is(err, store.ErrInvalidAPIKey):
			writeError(w, http.StatusUnauthorized, errors.New("invalid or missing API key"))
		case err != nil:
			s.fail(w, err)
		case need != roleTenant:
			writeError(w, http.StatusForbidden, errors.New("this endpoint needs the admin key"))
		default:
			next(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, tenant)))
		}
	})
}

// --- helpers ---------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// upstream reports a failure of WhatsApp or the media servers. Details are
// logged, not returned: they can contain protocol internals and CDN paths.
func (s *Server) upstream(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("upstream request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream request failed"})
}

// acquireLarge limits concurrent large requests (see maxLargeRequests).
func (s *Server) acquireLarge(r *http.Request) (release func(), err error) {
	if r.ContentLength >= 0 && r.ContentLength < largeBody {
		return func() {}, nil
	}
	select {
	case s.large <- struct{}{}:
		return func() { <-s.large }, nil
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// fail maps domain errors to HTTP status codes.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("not found"))
	case errors.Is(err, manager.ErrExists), errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, errors.New("already exists"))
	case errors.Is(err, store.ErrTenantNotEmpty):
		writeError(w, http.StatusConflict, errors.New("delete the tenant's instances first"))
	case errors.Is(err, manager.ErrInvalidID), errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, manager.ErrNotConnected), errors.Is(err, manager.ErrNotRunning):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		s.log.Error("api error", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("internal error"))
	}
}

var (
	errBadRequest      = errors.New("bad request")
	errNodeUnreachable = errors.New("the node running this instance is unreachable; retry shortly")
)

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

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
