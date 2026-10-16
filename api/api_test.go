package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/backoff"
	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/metrics"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
)

const testKey = "test-api-key-0123456789abcdef0123456789"

// hookSink records webhook deliveries.
type hookSink struct {
	mu     sync.Mutex
	events []manager.Envelope
	bad    int // deliveries with a wrong signature
	failN  int // answer 500 to the first failN requests
}

func (h *hookSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failN > 0 {
		h.failN--
		w.WriteHeader(500)
		return
	}
	if r.Header.Get("X-Wax-Signature") != manager.Sign([]byte("hook-secret"), body) {
		h.bad++
	}
	var env manager.Envelope
	_ = json.Unmarshal(body, &env)
	h.events = append(h.events, env)
}

func (h *hookSink) wait(t *testing.T, event string, from int) (manager.Envelope, int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for i := from; i < len(h.events); i++ {
			if h.events[i].Event == event {
				e := h.events[i]
				h.mu.Unlock()
				return e, i + 1
			}
		}
		h.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("webhook %q not received", event)
	return manager.Envelope{}, 0
}

type env struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	srv   *fakewa.Server
	api   *httptest.Server
	hooks *hookSink
	hook  *httptest.Server
	m     *manager.Manager
	reg   *metrics.Registry // metrics of the first node

	tenant string // the test's own tenant
	key    string // its API key
}

func setup(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("WAX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAX_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	st, err := store.Open(ctx, dsn, bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv, err := fakewa.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	e := &env{t: t, ctx: ctx, st: st, srv: srv, hooks: &hookSink{failN: 1}}
	e.hook = httptest.NewServer(e.hooks)
	t.Cleanup(e.hook.Close)
	first := e.startNode("a")
	e.m, e.api, e.reg = first.m, first.api, first.reg
	e.tenant, e.key = e.newTenant("t")
	return e
}

// newTenant creates a tenant through the admin API and issues it a key.
// Its instances and the tenant are removed when the test ends.
// node is one wax process of a test cluster: a manager and its API.
type node struct {
	m    *manager.Manager
	reg  *metrics.Registry
	api  *httptest.Server
	stop context.CancelFunc // cancels the manager's context (a crash: no release)
}

// testLeaseTTL keeps failover in tests short; leases renew every TTL/3.
const testLeaseTTL = 900 * time.Millisecond

// startNode starts a manager and its API server, as one node of a cluster
// sharing the test database and fake server.
func (e *env) startNode(id string) *node {
	e.t.Helper()
	var handler http.Handler
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})).With("node", id)
	reg := metrics.New()
	m := manager.New(manager.Config{Store: e.st, Log: log, Metrics: reg, ClientURL: e.srv.URL, Verifier: e.srv.Verifier, AllowPrivateWebhooks: true,
		NodeID: fmt.Sprintf("%s-%d", id, time.Now().UnixNano()), Address: api.URL,
		Timing: manager.Timing{
			WebhookRetry: backoff.Policy{Min: time.Millisecond, Max: 10 * time.Millisecond},
			OutboxRetry:  backoff.Policy{Min: time.Millisecond, Max: 10 * time.Millisecond},
			Idle:         50 * time.Millisecond,
			LeaseTTL:     testLeaseTTL,
		}})
	handler = New(m, e.st, testKey, log, Options{Metrics: reg})
	runCtx, cancel := context.WithCancel(e.ctx)
	if err := m.Start(runCtx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cancel(); m.Stop(); api.Close() })
	return &node{m: m, reg: reg, api: api, stop: cancel}
}

func (e *env) newTenant(prefix string) (id, key string) {
	e.t.Helper()
	id = fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	e.okAs(testKey, "POST", "/v1/tenants", map[string]any{"id": id, "name": "test"}, http.StatusCreated)
	var k struct {
		Key string `json:"key"`
	}
	mustT(e.t, json.Unmarshal(e.okAs(testKey, "POST", "/v1/tenants/"+id+"/keys", nil, http.StatusCreated), &k))
	e.t.Cleanup(func() {
		views, _ := e.st.InstanceViews(context.Background(), id, "")
		for _, v := range views {
			_ = e.st.DeleteInstance(context.Background(), v.ID)
		}
		_ = e.st.DeleteTenant(context.Background(), id)
	})
	return id, k.Key
}

func (e *env) call(method, path string, body any, key string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(e.ctx, method, e.api.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (e *env) ok(method, path string, body any, want int) []byte {
	e.t.Helper()
	return e.okAs(e.key, method, path, body, want)
}

// okAs is ok with an explicit key.
func (e *env) okAs(key, method, path string, body any, want int) []byte {
	e.t.Helper()
	code, out := e.call(method, path, body, key)
	if code != want {
		e.t.Fatalf("%s %s: status %d, body %s", method, path, code, out)
	}
	return out
}

func mustT(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// pairInstance drives QR pairing and login through the fake server.
func (e *env) pairInstance(id string) *fakewa.Conn {
	t, ctx := e.t, e.ctx
	c1, err := e.srv.Accept(ctx)
	mustT(t, err)
	mustT(t, c1.Send(ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "set", "id": "pd", "xmlns": "md"},
		Content: []wabin.Node{{Tag: "pair-device", Content: []wabin.Node{{Tag: "ref", Content: []byte("2@apiref")}}}}}))
	var qr struct {
		Code      string `json:"code"`
		PNGBase64 string `json:"png_base64"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := e.call("GET", "/v1/instances/"+id+"/qr", nil, e.key)
		if code == 200 {
			mustT(t, json.Unmarshal(body, &qr))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no QR: %d %s", code, body)
		}
		time.Sleep(time.Millisecond)
	}
	if qr.PNGBase64 == "" || !strings.Contains(qr.Code, "2@apiref") {
		t.Fatalf("qr %+v", qr)
	}
	parsed, err := fakewa.ParseQR(qr.Code)
	mustT(t, err)
	phone, err := fakewa.NewPhone("905550000001", 3)
	mustT(t, err)
	ps, err := phone.PairSuccess(parsed, "ps")
	mustT(t, err)
	mustT(t, c1.Send(ctx, ps))
	_, err = c1.ReadUntil(ctx, func(n *wabin.Node) bool { return n.Attrs["id"] == "ps" })
	mustT(t, err)
	mustT(t, c1.Send(ctx, wabin.Node{Tag: "stream:error", Attrs: wabin.Attrs{"code": "515"}}))
	c2, err := e.srv.Accept(ctx)
	mustT(t, err)
	c1.Close()
	mustT(t, c2.Send(ctx, wabin.Node{Tag: "success", Attrs: wabin.Attrs{"t": "1700000000", "lid": wabin.JID{User: "1905550000001", Device: 3, Server: wabin.ServerLID}}}))
	_, err = c2.ServeLogin(ctx, 812)
	mustT(t, err)
	return c2
}

func TestAPIEndToEnd(t *testing.T) {
	e := setup(t)
	id := "main"
	internal := manager.InternalID(e.tenant, id)

	// Auth.
	if code, _ := e.call("GET", "/v1/instances", nil, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad key accepted: %d", code)
	}
	// Validation and creation.
	e.ok("POST", "/v1/instances", map[string]any{"id": "Bad ID!"}, http.StatusBadRequest)
	e.ok("POST", "/v1/instances", map[string]any{"id": id, "webhook_url": e.hook.URL, "webhook_secret": "hook-secret"}, http.StatusCreated)
	e.ok("POST", "/v1/instances", map[string]any{"id": id}, http.StatusConflict)

	c := e.pairInstance(id)
	// The first QR webhook was rejected once (failN) and retried.
	qrEvt, next := e.hooks.wait(t, "qr", 0)
	if qrEvt.Instance != id {
		t.Fatalf("qr event %+v", qrEvt)
	}
	_, next = e.hooks.wait(t, "connected", next)
	var inst instanceJSON
	mustT(t, json.Unmarshal(e.ok("GET", "/v1/instances/"+id, nil, 200), &inst))
	if inst.Status != "connected" || inst.JID != "905550000001@s.whatsapp.net" {
		t.Fatalf("instance %+v", inst)
	}

	// Idempotent enqueue.
	req := map[string]any{"to": "status@broadcast", "text": "merhaba", "client_id": "c-1"}
	var r1, r2 map[string]any
	mustT(t, json.Unmarshal(e.ok("POST", "/v1/instances/"+id+"/messages", req, http.StatusAccepted), &r1))
	mustT(t, json.Unmarshal(e.ok("POST", "/v1/instances/"+id+"/messages", req, http.StatusAccepted), &r2))
	if r1["outbox_id"] != r2["outbox_id"] || r2["duplicate"] != true {
		t.Fatalf("not idempotent: %v %v", r1, r2)
	}
	e.ok("POST", "/v1/instances/"+id+"/messages", map[string]any{"to": "abc"}, http.StatusBadRequest)
	// Broadcast lists are not supported: the worker fails it permanently.
	st, _ := e.hooks.wait(t, "send_status", next)
	if st.Data.(map[string]any)["status"] != "failed" {
		t.Fatalf("send_status %+v", st)
	}
	var ob map[string]any
	mustT(t, json.Unmarshal(e.ok("GET", fmt.Sprintf("/v1/instances/%s/outbox/%v", id, r1["outbox_id"]), nil, 200), &ob))
	if ob["status"] != "failed" {
		t.Fatalf("outbox %v", ob)
	}

	// Chats and messages come from the store.
	chat := "905551111111@s.whatsapp.net"
	content, _ := proto.Marshal(&wae2e.Message{Conversation: proto.String("selam")})
	mustT(t, e.st.PutMessage(e.ctx, internal, &store.StoredMessage{Chat: chat, ID: "M1", Sender: chat, SentAt: time.Unix(1700000000, 0), Type: "text", Content: content, Source: "live"}))
	at := time.Unix(1700000000, 0)
	mustT(t, e.st.UpdateChat(e.ctx, internal, chat, store.ChatUpdate{LastMessageAt: &at}))
	var chats []store.Chat
	mustT(t, json.Unmarshal(e.ok("GET", "/v1/instances/"+id+"/chats", nil, 200), &chats))
	if len(chats) != 1 || chats[0].JID != chat {
		t.Fatalf("chats %+v", chats)
	}
	var msgs []manager.MessageJSON
	mustT(t, json.Unmarshal(e.ok("GET", "/v1/instances/"+id+"/chats/"+chat+"/messages", nil, 200), &msgs))
	if len(msgs) != 1 || msgs[0].Text != "selam" {
		t.Fatalf("messages %+v", msgs)
	}

	// Metrics reflect what happened.
	var mb strings.Builder
	mustT(t, e.reg.Write(&mb))
	for _, want := range []string{
		fmt.Sprintf(`wax_instance_status{tenant="%s",instance="main",status="connected"} 1`, e.tenant),
		`wax_api_requests_total{route="GET /v1/instances/{id}",code="200"}`,
		`wax_api_requests_total{route="POST /v1/instances/{id}/messages",code="202"}`,
		fmt.Sprintf(`wax_messages_sent_total{tenant="%s",instance="main",result="failed"} 1`, e.tenant),
		fmt.Sprintf(`wax_webhook_deliveries_total{tenant="%s",instance="main",result="ok"}`, e.tenant),
		fmt.Sprintf(`wax_webhook_deliveries_total{tenant="%s",instance="main",result="retry"} 1`, e.tenant),
		fmt.Sprintf(`wax_outbox_pending{tenant="%s",instance="main"} 0`, e.tenant),
		`wax_db_connections{state="max"}`,
		`wax_lease_events_total{event="acquired"}`,
		"# TYPE wax_api_request_seconds histogram",
	} {
		if !strings.Contains(mb.String(), want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	e.hooks.mu.Lock()
	bad := e.hooks.bad
	e.hooks.mu.Unlock()
	if bad != 0 {
		t.Fatalf("%d webhooks with bad signatures", bad)
	}

	// Delete: logs out (remove-companion-device) and removes everything.
	go func() {
		q, err := c.ReadIQ(e.ctx, "md")
		if err == nil {
			_ = c.Send(e.ctx, fakewa.Result(q.Attrs["id"].(string)))
		}
	}()
	e.ok("DELETE", "/v1/instances/"+id, nil, http.StatusNoContent)
	e.ok("GET", "/v1/instances/"+id, nil, http.StatusNotFound)
}

// TestTenantIsolation checks that tenants cannot see or touch each other's
// instances and data, and that the admin and tenant roles are separate.
func TestTenantIsolation(t *testing.T) {
	e := setup(t)
	a, keyA := e.tenant, e.key
	b, keyB := e.newTenant("u")

	// Both tenants may use the same instance name.
	e.okAs(keyA, "POST", "/v1/instances", map[string]any{"id": "main"}, http.StatusCreated)
	e.okAs(keyB, "POST", "/v1/instances", map[string]any{"id": "main"}, http.StatusCreated)
	e.okAs(keyA, "POST", "/v1/instances", map[string]any{"id": "only-a"}, http.StatusCreated)

	// Data written for A's instance is invisible through B's instance.
	chat := "905551111111@s.whatsapp.net"
	content, _ := proto.Marshal(&wae2e.Message{Conversation: proto.String("gizli")})
	// Messages reference the instance's device row, which its client
	// creates in the background right after start.
	waitFor(t, "A's device row", 5*time.Second, func() bool {
		return e.st.PutMessage(e.ctx, manager.InternalID(a, "main"), &store.StoredMessage{Chat: chat, ID: "S1", Sender: chat,
			SentAt: time.Unix(1700000000, 0), Type: "text", Content: content, Source: "live"}) == nil
	})
	at := time.Unix(1700000000, 0)
	mustT(t, e.st.UpdateChat(e.ctx, manager.InternalID(a, "main"), chat, store.ChatUpdate{LastMessageAt: &at}))
	var chats []store.Chat
	mustT(t, json.Unmarshal(e.okAs(keyB, "GET", "/v1/instances/main/chats", nil, 200), &chats))
	if len(chats) != 0 {
		t.Fatalf("tenant B sees A's chats: %+v", chats)
	}
	var msgs []manager.MessageJSON
	mustT(t, json.Unmarshal(e.okAs(keyB, "GET", "/v1/instances/main/chats/"+chat+"/messages", nil, 200), &msgs))
	if len(msgs) != 0 {
		t.Fatalf("tenant B sees A's messages: %+v", msgs)
	}
	mustT(t, json.Unmarshal(e.okAs(keyA, "GET", "/v1/instances/main/chats/"+chat+"/messages", nil, 200), &msgs))
	if len(msgs) != 1 || msgs[0].Text != "gizli" {
		t.Fatalf("tenant A lost its message: %+v", msgs)
	}

	// Another tenant's instance does not exist for B (404, not 403), on
	// every kind of route.
	for _, r := range []struct{ method, path string }{
		{"GET", "/v1/instances/only-a"},
		{"PATCH", "/v1/instances/only-a"},
		{"DELETE", "/v1/instances/only-a"},
		{"GET", "/v1/instances/only-a/qr"},
		{"GET", "/v1/instances/only-a/chats"},
		{"POST", "/v1/instances/only-a/messages"},
		{"GET", "/v1/instances/only-a/outbox/1"},
	} {
		if code, _ := e.call(r.method, r.path, map[string]any{}, keyB); code != http.StatusNotFound {
			t.Errorf("tenant B %s %s: status %d, want 404", r.method, r.path, code)
		}
	}
	var list []instanceJSON
	mustT(t, json.Unmarshal(e.okAs(keyB, "GET", "/v1/instances", nil, 200), &list))
	if len(list) != 1 || list[0].ID != "main" {
		t.Fatalf("tenant B lists %+v", list)
	}

	// Roles: the admin key cannot reach instances, tenant keys cannot manage
	// tenants or keys.
	if code, _ := e.call("GET", "/v1/instances", nil, testKey); code != http.StatusForbidden {
		t.Errorf("admin key on instances: %d", code)
	}
	if code, _ := e.call("GET", "/v1/tenants", nil, keyA); code != http.StatusForbidden {
		t.Errorf("tenant key on tenants: %d", code)
	}
	if code, _ := e.call("POST", "/v1/tenants/"+b+"/keys", nil, keyA); code != http.StatusForbidden {
		t.Errorf("tenant A issuing a key for B: %d", code)
	}

	// A tenant with instances cannot be deleted; a revoked key stops at once.
	e.okAs(testKey, "DELETE", "/v1/tenants/"+b, nil, http.StatusConflict)
	var keys []store.APIKey
	mustT(t, json.Unmarshal(e.okAs(testKey, "GET", "/v1/tenants/"+b+"/keys", nil, 200), &keys))
	if len(keys) != 1 {
		t.Fatalf("keys %+v", keys)
	}
	e.okAs(testKey, "DELETE", "/v1/tenants/"+b+"/keys/"+keys[0].ID, nil, http.StatusNoContent)
	if code, _ := e.call("GET", "/v1/instances", nil, keyB); code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d", code)
	}
	// Malformed and forged keys are rejected the same way.
	forged := keyA[:len(keyA)-4] + "AAAA"
	for _, k := range []string{"wax_", "wax_zz_zz", forged} {
		if code, _ := e.call("GET", "/v1/instances", nil, k); code != http.StatusUnauthorized {
			t.Errorf("key %q accepted: %d", k, code)
		}
	}
}
