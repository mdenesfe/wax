// Package metrics records counters, gauges and histograms and writes them
// in the Prometheus text exposition format (version 0.0.4), using only the
// standard library.
//
// A nil *Registry, and every metric obtained from it, is a valid no-op, so
// components can record unconditionally whether metrics are enabled or not.
package metrics

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metric families.
type Registry struct {
	mu         sync.Mutex
	families   map[string]*family
	collectors []func(*Registry)
}

// New returns an empty registry.
func New() *Registry { return &Registry{families: map[string]*family{}} }

type kind string

const (
	counterKind   kind = "counter"
	gaugeKind     kind = "gauge"
	histogramKind kind = "histogram"
)

// family is one metric name with its label names and series.
type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64 // histograms only
	mu         sync.Mutex
	series     map[string]*series // by joined label values
}

type series struct {
	values []string
	value  atomic.Uint64 // float64 bits (counter, gauge)
	// histogram
	mu     sync.Mutex
	counts []uint64 // per bucket (non-cumulative)
	sum    float64
	count  uint64
}

func (r *Registry) family(name, help string, k kind, labels []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		if f.kind != k || !slices.Equal(f.labels, labels) {
			panic(fmt.Sprintf("metrics: %s registered twice with different shapes", name))
		}
		return f
	}
	f := &family{name: name, help: help, kind: k, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.families[name] = f
	return f
}

func (f *family) with(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d labels, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[key]
	if !ok {
		s = &series{values: slices.Clone(values)}
		if f.kind == histogramKind {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// forget drops the series whose label values match, e.g. an instance's
// series when it stops running on this node.
func (f *family) forget(match func(values []string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, s := range f.series {
		if match(s.values) {
			delete(f.series, k)
		}
	}
}

func addFloat(v *atomic.Uint64, d float64) {
	for {
		old := v.Load()
		if v.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// --- counters -------------------------------------------------------------------

// CounterVec is a counter family with labels.
type CounterVec struct{ f *family }

// Counter registers (or returns) a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	if r == nil {
		return nil
	}
	return &CounterVec{r.family(name, help, counterKind, labels, nil)}
}

// Add increases the series with the given label values by d (d >= 0).
func (c *CounterVec) Add(d float64, values ...string) {
	if c == nil || d < 0 {
		return
	}
	addFloat(&c.f.with(values).value, d)
}

// Inc adds one.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// --- gauges ---------------------------------------------------------------------

// GaugeVec is a gauge family with labels.
type GaugeVec struct{ f *family }

// Gauge registers (or returns) a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	if r == nil {
		return nil
	}
	return &GaugeVec{r.family(name, help, gaugeKind, labels, nil)}
}

// Set sets the series with the given label values.
func (g *GaugeVec) Set(v float64, values ...string) {
	if g == nil {
		return
	}
	g.f.with(values).value.Store(math.Float64bits(v))
}

// Add changes the series by d.
func (g *GaugeVec) Add(d float64, values ...string) {
	if g == nil {
		return
	}
	addFloat(&g.f.with(values).value, d)
}

// Reset drops all series (for gauges recomputed on every scrape).
func (g *GaugeVec) Reset() {
	if g == nil {
		return
	}
	g.f.forget(func([]string) bool { return true })
}

// Forget drops the series whose leading label values equal prefix (e.g.
// tenant and instance, to drop all statuses of one instance).
func (g *GaugeVec) Forget(prefix ...string) {
	if g == nil {
		return
	}
	g.f.forget(func(v []string) bool { return len(v) >= len(prefix) && slices.Equal(v[:len(prefix)], prefix) })
}

// --- histograms -----------------------------------------------------------------

// DefaultBuckets suit latencies in seconds, from 5 ms to 30 s.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// HistogramVec is a histogram family with labels.
type HistogramVec struct{ f *family }

// Histogram registers (or returns) a histogram family with the given
// upper bounds (sorted; +Inf is implied).
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	if r == nil {
		return nil
	}
	return &HistogramVec{r.family(name, help, histogramKind, labels, buckets)}
}

// Observe records one value. Finding the bucket is a binary search,
// O(log buckets).
func (h *HistogramVec) Observe(v float64, values ...string) {
	if h == nil {
		return
	}
	s := h.f.with(values)
	i, _ := slices.BinarySearch(h.f.buckets, v)
	s.mu.Lock()
	if i < len(s.counts) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
	s.mu.Unlock()
}

// --- collection -------------------------------------------------------------------

// OnCollect registers fn to run before every scrape, to refresh gauges
// computed from other state (queue lengths, pool statistics).
func (r *Registry) OnCollect(fn func(*Registry)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, fn)
	r.mu.Unlock()
}

// Write runs the collectors and writes all families, sorted by name, in
// the Prometheus text format.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	collectors := slices.Clone(r.collectors)
	r.mu.Unlock()
	for _, fn := range collectors {
		fn(r)
	}
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	r.mu.Unlock()
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		r.mu.Lock()
		f := r.families[n]
		r.mu.Unlock()
		f.write(&b)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func (f *family) write(b *strings.Builder) {
	f.mu.Lock()
	list := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		list = append(list, s)
	}
	f.mu.Unlock()
	if len(list) == 0 {
		return
	}
	slices.SortFunc(list, func(a, c *series) int { return slices.Compare(a.values, c.values) })
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	for _, s := range list {
		labels := f.labelText(s.values, "", "")
		if f.kind != histogramKind {
			fmt.Fprintf(b, "%s%s %s\n", f.name, labels, num(math.Float64frombits(s.value.Load())))
			continue
		}
		s.mu.Lock()
		var cum uint64
		for i, ub := range f.buckets {
			cum += s.counts[i]
			fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelText(s.values, "le", num(ub)), cum)
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelText(s.values, "le", "+Inf"), s.count)
		fmt.Fprintf(b, "%s_sum%s %s\n%s_count%s %d\n", f.name, labels, num(s.sum), f.name, labels, s.count)
		s.mu.Unlock()
	}
}

// labelText renders {a="x",b="y"}, with an optional extra label.
func (f *family) labelText(values []string, extraName, extraValue string) string {
	if len(values) == 0 && extraName == "" {
		return ""
	}
	parts := make([]string, 0, len(values)+1)
	for i, v := range values {
		parts = append(parts, f.labels[i]+`="`+escapeLabel(v)+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+extraValue+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
