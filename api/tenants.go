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
		ID                string `json:"id"`
		Name              string `json:"name"`
		MaxInstances      int    `json:"max_instances"`
		RequestsPerSecond int    `json:"requests_per_second"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	t := store.Tenant{ID: req.ID, Name: req.Name, MaxInstances: req.MaxInstances, RequestsPerSecond: req.RequestsPerSecond}
	if !tenantIDPattern.MatchString(t.ID) {
		s.fail(w, badRequest("id must be 1-64 characters of a-z, 0-9, '-' or '_'"))
		return
	}
	if err := validLimits(t); err != nil {
		s.fail(w, err)
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

// validLimits checks tenant limits (0 = no quota / server default).
func validLimits(t store.Tenant) error {
	if t.MaxInstances < 0 || t.RequestsPerSecond < 0 || t.RequestsPerSecond > 10000 {
		return badRequest("max_instances must be >= 0 and requests_per_second between 0 and 10000")
	}
	return nil
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTenant(r.Context(), r.PathValue("tenant"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// updateTenant changes the name and limits; absent fields keep their value.
func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Name              *string `json:"name"`
		MaxInstances      *int    `json:"max_instances"`
		RequestsPerSecond *int    `json:"requests_per_second"`
	}
	if err := decode(r, &patch); err != nil {
		s.fail(w, err)
		return
	}
	t, err := s.store.GetTenant(r.Context(), r.PathValue("tenant"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if patch.Name != nil {
		t.Name = *patch.Name
	}
	if patch.MaxInstances != nil {
		t.MaxInstances = *patch.MaxInstances
	}
	if patch.RequestsPerSecond != nil {
		t.RequestsPerSecond = *patch.RequestsPerSecond
	}
	if err := validLimits(*t); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateTenant(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
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
