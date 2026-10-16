package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mdenesfe/wax/internal/metrics"
)

// apiMetrics are the request metrics. The route label is the ServeMux
// pattern (e.g. "GET /v1/instances/{id}"), so ids never become labels.
type apiMetrics struct {
	requests *metrics.CounterVec   // route, code
	duration *metrics.HistogramVec // route
	limited  *metrics.CounterVec   // tenant
}

func newAPIMetrics(r *metrics.Registry) apiMetrics {
	return apiMetrics{
		requests: r.Counter("wax_api_requests_total", "API requests by route and status code.", "route", "code"),
		duration: r.Histogram("wax_api_request_seconds", "API request duration by route.", metrics.DefaultBuckets, "route"),
		limited:  r.Counter("wax_api_rate_limited_total", "API requests refused by the tenant rate limit.", "tenant"),
	}
}

func (m apiMetrics) observe(r *http.Request, code int, d time.Duration) {
	route := r.Pattern // set by ServeMux on the request it routed
	if route == "" {
		route = "unmatched"
	}
	m.requests.Inc(route, strconv.Itoa(code))
	m.duration.Observe(d.Seconds(), route)
}

// statusRecorder remembers the response status. Unwrap lets
// http.ResponseController (used by the forwarding proxy) reach the
// underlying writer's Flush and deadlines.
type statusRecorder struct {
	http.ResponseWriter
	code    int
	written bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.written {
		w.code, w.written = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
