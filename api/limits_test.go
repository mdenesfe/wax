package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
)

// TestTenantRateLimit: a tenant over its rate gets 429 with Retry-After,
// other tenants are unaffected, and a client cannot escape the limit with a
// forged forwarding header.
func TestTenantRateLimit(t *testing.T) {
	e := setup(t)
	_, keyB := e.newTenant("u")
	e.okAs(testKey, "PATCH", "/v1/tenants/"+e.tenant, map[string]any{"requests_per_second": 1}, http.StatusOK)

	// Burst of two seconds' worth, then 429.
	codes := []int{}
	var retry string
	for range 4 {
		req, _ := http.NewRequestWithContext(e.ctx, "GET", e.api.URL+"/v1/instances", nil)
		req.Header.Set("Authorization", "Bearer "+e.key)
		req.Header.Set(forwardedHeader, "evil;"+strconv.FormatInt(time.Now().Unix(), 10)+";00") // forged
		resp, err := http.DefaultClient.Do(req)
		mustT(t, err)
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests {
			retry = resp.Header.Get("Retry-After")
		}
	}
	if fmt.Sprint(codes) != "[200 200 429 429]" || retry == "" {
		t.Fatalf("codes %v, Retry-After %q", codes, retry)
	}
	// Another tenant has its own budget.
	if code, _ := e.call("GET", "/v1/instances", nil, keyB); code != http.StatusOK {
		t.Fatalf("tenant B limited by A's traffic: %d", code)
	}
	// The limit refills.
	time.Sleep(1100 * time.Millisecond)
	if code, _ := e.call("GET", "/v1/instances", nil, e.key); code != http.StatusOK {
		t.Fatalf("after waiting: %d", code)
	}
}

// TestInstanceQuota: creation stops at the quota, also under concurrency.
func TestInstanceQuota(t *testing.T) {
	e := setup(t)
	var tenant store.Tenant
	mustT(t, json.Unmarshal(e.okAs(testKey, "PATCH", "/v1/tenants/"+e.tenant, map[string]any{"max_instances": 2}, http.StatusOK), &tenant))
	if tenant.MaxInstances != 2 {
		t.Fatalf("tenant %+v", tenant)
	}
	var created, refused atomic.Int32
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := e.call("POST", "/v1/instances", map[string]any{"id": fmt.Sprintf("q-%d", i)}, e.key)
			switch code {
			case http.StatusCreated:
				created.Add(1)
			case http.StatusForbidden:
				refused.Add(1)
			default:
				t.Errorf("unexpected status %d", code)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 2 || refused.Load() != 4 {
		t.Fatalf("created %d, refused %d; want 2 and 4", created.Load(), refused.Load())
	}
	e.okAs(testKey, "PATCH", "/v1/tenants/"+e.tenant, map[string]any{"max_instances": -1}, http.StatusBadRequest)
}

// TestLargeSlotsFairness: one tenant gets at most half of the slots.
func TestLargeSlotsFairness(t *testing.T) {
	l := newLargeSlots(4)
	take := func(tenant string, n int) int {
		got := 0
		for range n {
			if l.tryAcquire(tenant) {
				got++
			}
		}
		return got
	}
	if got := take("a", 3); got != 2 {
		t.Fatalf("tenant a got %d slots, want 2 (half)", got)
	}
	if got := take("b", 2); got != 2 {
		t.Fatalf("tenant b got %d slots: starved by a", got)
	}
	l.release("a")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.acquire(ctx, "a"); err != nil {
		t.Fatalf("released slot not reusable: %v", err)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := l.acquire(short, "c"); err == nil {
		t.Fatal("acquired a slot while all were in use")
	}
}

// TestWebhookRetarget: pending events follow a webhook URL change at once,
// without waiting for their backoff, and are dropped when it is removed.
func TestWebhookRetarget(t *testing.T) {
	e := setup(t)
	failing := &hookSink{failN: 1 << 30}
	failSrv := httptest.NewServer(failing)
	defer failSrv.Close()
	e.ok("POST", "/v1/instances", map[string]any{"id": "hooks", "webhook_url": failSrv.URL, "webhook_secret": "s"}, http.StatusCreated)
	id := manager.InternalID(e.tenant, "hooks")
	pending := func() int {
		d, err := e.st.QueueDepths(e.ctx, []string{id})
		mustT(t, err)
		return d[id].Webhooks
	}
	// Events pile up while the receiver fails; force a long backoff.
	c := e.pairInstance("hooks")
	waitFor(t, "pending events", 5*time.Second, func() bool { return pending() > 0 })

	// A new URL: the backlog is delivered there promptly.
	good := &hookSink{}
	goodSrv := httptest.NewServer(good)
	defer goodSrv.Close()
	e.ok("PATCH", "/v1/instances/hooks", map[string]any{"webhook_url": goodSrv.URL}, http.StatusOK)
	waitFor(t, "backlog delivered to the new URL", 2*time.Second, func() bool {
		good.mu.Lock()
		defer good.mu.Unlock()
		return len(good.events) > 0 && pending() == 0
	})

	// Removing the URL drops what is pending right away.
	failing2 := &hookSink{failN: 1 << 30}
	fail2 := httptest.NewServer(failing2)
	defer fail2.Close()
	e.ok("PATCH", "/v1/instances/hooks", map[string]any{"webhook_url": fail2.URL}, http.StatusOK)
	// The server reports a contact's presence: one more webhook event.
	mustT(t, c.Send(e.ctx, wabin.Node{Tag: "presence", Attrs: wabin.Attrs{"from": wabin.JID{User: "905551111111", Server: wabin.ServerUser}}}))
	waitFor(t, "a new pending event", 5*time.Second, func() bool { return pending() > 0 })
	e.ok("PATCH", "/v1/instances/hooks", map[string]any{"webhook_url": ""}, http.StatusOK)
	if n := pending(); n != 0 {
		t.Fatalf("%d events still pending after removing the URL", n)
	}
}
