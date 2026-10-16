package api

import (
	"net/http"
	"regexp"

	"github.com/mdenesfe/wax/internal/store"
)

// tenantIDPattern matches instance names: ':' is excluded so internal
// instance ids ("tenant:name") stay unambiguous.
var tenantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	t := store.Tenant{ID: req.ID, Name: req.Name}
	if !tenantIDPattern.MatchString(t.ID) {
		s.fail(w, badRequest("id must be 1-64 characters of a-z, 0-9, '-' or '_'"))
		return
	}
	if err := s.store.CreateTenant(r.Context(), &t); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListTenants(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteTenant(r.Context(), r.PathValue("tenant")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createKey issues a tenant key. The response is the only place the key
// appears; it cannot be retrieved later.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	token, key, err := s.store.CreateAPIKey(r.Context(), r.PathValue("tenant"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": key.ID, "key": token, "created_at": key.CreatedAt})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context(), r.PathValue("tenant"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAPIKey(r.Context(), r.PathValue("tenant"), r.PathValue("key")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
