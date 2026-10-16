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
	"github.com/mdenesfe/wax/internal/metrics"
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
	large  *largeSlots    // concurrent large request bodies, fair per tenant
	limits *tenantLimiter // per-tenant API rate
	met    apiMetrics
	// nodeHTTP forwards requests to other nodes; it shares connections.
	nodeHTTP http.RoundTripper
}

// Options are optional API settings.
type Options struct {
	// DefaultRequestsPerSecond is the API rate of tenants without their own
	// (0: DefaultRequestsPerSecond).
	DefaultRequestsPerSecond int
	// Metrics receives request metrics (nil: disabled).
	Metrics *metrics.Registry
}

// New builds the API handler.
func New(m *manager.Manager, st *store.Store, apiKey string, log *slog.Logger, opts Options) *Server {
	s := &Server{m: m, store: st, apiKey: []byte(apiKey), log: log, mux: http.NewServeMux(),
		large:    newLargeSlots(maxLargeRequests),
		limits:   newTenantLimiter(opts.DefaultRequestsPerSecond),
		met:      newAPIMetrics(opts.Metrics),
		nodeHTTP: &http.Transport{ResponseHeaderTimeout: 5 * time.Minute, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 16}}
	s.routes()
	return s
}

// instanceHandler serves a request for one instance, already resolved
// within the caller's tenant.
type instanceHandler func(http.ResponseWriter, *http.Request, *manager.Instance)

// route is one API endpoint: its ServeMux pattern and who may call it.
type route struct {
	pattern  string
	role     role
	handler  http.HandlerFunc // admin and tenant routes
	instance instanceHandler  // routes on one instance (tenant role)
}

// routeTable lists every authenticated endpoint. The OpenAPI contract test
// compares it with openapi.json.
func (s *Server) routeTable() []route {
	return []route{
		{pattern: "POST /v1/tenants", role: roleAdmin, handler: s.createTenant},
		{pattern: "GET /v1/tenants", role: roleAdmin, handler: s.listTenants},
		{pattern: "GET /v1/tenants/{tenant}", role: roleAdmin, handler: s.getTenant},
		{pattern: "PATCH /v1/tenants/{tenant}", role: roleAdmin, handler: s.updateTenant},
		{pattern: "DELETE /v1/tenants/{tenant}", role: roleAdmin, handler: s.deleteTenant},
		{pattern: "POST /v1/tenants/{tenant}/keys", role: roleAdmin, handler: s.createKey},
		{pattern: "GET /v1/tenants/{tenant}/keys", role: roleAdmin, handler: s.listKeys},
		{pattern: "DELETE /v1/tenants/{tenant}/keys/{key}", role: roleAdmin, handler: s.deleteKey},

		{pattern: "POST /v1/instances", role: roleTenant, handler: s.createInstance},
		{pattern: "GET /v1/instances", role: roleTenant, handler: s.listInstances},
		{pattern: "GET /v1/instances/{id}", instance: s.getInstance},
		{pattern: "PATCH /v1/instances/{id}", instance: s.updateInstance},
		{pattern: "DELETE /v1/instances/{id}", instance: s.deleteInstance},
		{pattern: "POST /v1/instances/{id}/connect", instance: s.connect},
		{pattern: "GET /v1/instances/{id}/qr", instance: s.qr},
		{pattern: "POST /v1/instances/{id}/messages", instance: s.sendMessage},
		{pattern: "GET /v1/instances/{id}/outbox/{oid}", instance: s.outboxStatus},
		{pattern: "GET /v1/instances/{id}/chats", instance: s.listChats},
		{pattern: "GET /v1/instances/{id}/chats/{chat}/messages", instance: s.listMessages},
		{pattern: "GET /v1/instances/{id}/chats/{chat}/messages/{mid}/media", instance: s.downloadMedia},
		{pattern: "POST /v1/instances/{id}/read", instance: s.markRead},
		{pattern: "POST /v1/instances/{id}/presence", instance: s.presence},
		{pattern: "GET /v1/instances/{id}/groups/{group}", instance: s.groupInfo},
		{pattern: "POST /v1/instances/{id}/calls/{call}/reject", instance: s.rejectCall},
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, healthJSON{Status: "ok"})
	})
	s.mux.HandleFunc("GET /openapi.json", serveOpenAPI)
	for _, rt := range s.routeTable() {
		if rt.instance != nil {
			s.mux.Handle(rt.pattern, s.authorized(roleTenant, s.scoped(rt.instance)))
		} else {
			s.mux.Handle(rt.pattern, s.authorized(rt.role, rt.handler))
		}
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
		if !in.Running() && !isForwarded(r) {
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
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("api handler panicked", "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
			writeError(rec, http.StatusInternalServerError, errors.New("internal error"))
		}
		s.met.observe(r, rec.code, time.Since(start))
	}()
	r.Body = http.MaxBytesReader(rec, r.Body, maxBody)
	s.mux.ServeHTTP(rec, r)
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
		auth, err := s.store.VerifyAPIKey(r.Context(), token)
		switch {
		case errors.Is(err, store.ErrInvalidAPIKey):
			writeError(w, http.StatusUnauthorized, errors.New("invalid or missing API key"))
			return
		case err != nil:
			s.fail(w, err)
			return
		case need != roleTenant:
			writeError(w, http.StatusForbidden, errors.New("this endpoint needs the admin key"))
			return
		}
		// The node that received the request from the client applied the
		// limit already; a signed forwarded request is not counted twice.
		forwarded := s.verifyForward(r)
		if !forwarded {
			if ok, wait := s.limits.allow(auth.TenantID, auth.RequestsPerSecond); !ok {
				s.met.limited.Inc(auth.TenantID)
				tooManyRequests(w, wait)
				return
			}
		}
		r = withForwarded(r, forwarded)
		next(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, auth.TenantID)))
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
	writeJSON(w, http.StatusBadGateway, errorJSON{Error: "upstream request failed"})
}

// acquireLarge limits concurrent large requests (see maxLargeRequests),
// at most half of them per tenant.
func (s *Server) acquireLarge(r *http.Request) (release func(), err error) {
	if r.ContentLength >= 0 && r.ContentLength < largeBody {
		return func() {}, nil
	}
	tenant := tenantOf(r)
	if err := s.large.acquire(r.Context(), tenant); err != nil {
		return nil, err
	}
	return func() { s.large.release(tenant) }, nil
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorJSON{Error: err.Error()})
}

// fail maps domain errors to HTTP status codes.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("not found"))
	case errors.Is(err, manager.ErrExists), errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, errors.New("already exists"))
	case errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, http.StatusForbidden, errors.New("instance quota of this tenant reached"))
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
