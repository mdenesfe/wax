package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestTextFormat(t *testing.T) {
	r := New()
	c := r.Counter("wax_test_total", "Things counted.", "kind")
	c.Inc("a")
	c.Add(2, "a")
	c.Inc(`b"q`)
	g := r.Gauge("wax_test_gauge", "A level.")
	g.Set(1.5)
	h := r.Histogram("wax_test_seconds", "Durations.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "x")
	h.Observe(0.5, "x")
	h.Observe(3, "x")
	r.OnCollect(func(r *Registry) { r.Gauge("wax_test_collected", "Set at scrape.").Set(7) })

	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP wax_test_collected Set at scrape.
# TYPE wax_test_collected gauge
wax_test_collected 7
# HELP wax_test_gauge A level.
# TYPE wax_test_gauge gauge
wax_test_gauge 1.5
# HELP wax_test_seconds Durations.
# TYPE wax_test_seconds histogram
wax_test_seconds_bucket{route="x",le="0.1"} 1
wax_test_seconds_bucket{route="x",le="1"} 2
wax_test_seconds_bucket{route="x",le="+Inf"} 3
wax_test_seconds_sum{route="x"} 3.55
wax_test_seconds_count{route="x"} 3
# HELP wax_test_total Things counted.
# TYPE wax_test_total counter
wax_test_total{kind="a"} 3
wax_test_total{kind="b\"q"} 1
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestNilRegistryIsNoop(t *testing.T) {
	var r *Registry
	r.Counter("x", "").Inc()
	r.Gauge("y", "").Set(1)
	r.Histogram("z", "", DefaultBuckets).Observe(1)
	r.OnCollect(func(*Registry) {})
}

func TestConcurrentCounters(t *testing.T) {
	r := New()
	c := r.Counter("wax_c_total", "c", "i")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				c.Inc("a")
			}
		}()
	}
	wg.Wait()
	var b strings.Builder
	_ = r.Write(&b)
	if !strings.Contains(b.String(), `wax_c_total{i="a"} 50000`) {
		t.Fatalf("lost increments:\n%s", b.String())
	}
}

func TestGaugeForget(t *testing.T) {
	r := New()
	g := r.Gauge("wax_g", "g", "instance", "status")
	g.Set(1, "a", "connected")
	g.Set(1, "b", "qr")
	g.Forget("a")
	var b strings.Builder
	_ = r.Write(&b)
	if strings.Contains(b.String(), `instance="a"`) || !strings.Contains(b.String(), `instance="b"`) {
		t.Fatalf("forget:\n%s", b.String())
	}
}
