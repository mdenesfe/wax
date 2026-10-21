package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNoUI: with NoUI the admin interface is not served, the API still is.
func TestNoUI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		noUI bool
		ui   int
		root int
	}{
		{false, http.StatusOK, http.StatusFound},
		{true, http.StatusNotFound, http.StatusNotFound},
	} {
		s := New(nil, nil, testKey, log, Options{NoUI: tc.noUI})
		get := func(path string) int {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			return w.Code
		}
		if got := get("/ui/"); got != tc.ui {
			t.Errorf("NoUI %v: /ui/ answered %d, want %d", tc.noUI, got, tc.ui)
		}
		if got := get("/"); got != tc.root {
			t.Errorf("NoUI %v: / answered %d, want %d", tc.noUI, got, tc.root)
		}
		if got := get("/healthz"); got != http.StatusOK {
			t.Errorf("NoUI %v: /healthz answered %d", tc.noUI, got)
		}
	}
}
