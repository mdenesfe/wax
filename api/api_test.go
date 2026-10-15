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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := manager.New(manager.Config{Store: st, Log: log, ClientURL: srv.URL, Verifier: srv.Verifier, Timing: manager.Timing{
		WebhookRetry: backoff.Policy{Min: time.Millisecond, Max: 10 * time.Millisecond},
		OutboxRetry:  backoff.Policy{Min: time.Millisecond, Max: 10 * time.Millisecond},
		Idle:         50 * time.Millisecond,
	}})
	runCtx, stopRun := context.WithCancel(ctx)
	if err := m.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRun(); m.Stop() })
	e := &env{t: t, ctx: ctx, st: st, srv: srv, hooks: &hookSink{failN: 1}}
	e.api = httptest.NewServer(New(m, st, testKey, log))
	e.hook = httptest.NewServer(e.hooks)
	t.Cleanup(e.api.Close)
	t.Cleanup(e.hook.Close)
	return e
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
	code, out := e.call(method, path, body, testKey)
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
		code, body := e.call("GET", "/v1/instances/"+id+"/qr", nil, testKey)
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
	id := fmt.Sprintf("api-%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = e.st.DeleteInstance(context.Background(), id) })

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
	mustT(t, e.st.PutMessage(e.ctx, id, &store.StoredMessage{Chat: chat, ID: "M1", Sender: chat, SentAt: time.Unix(1700000000, 0), Type: "text", Content: content, Source: "live"}))
	at := time.Unix(1700000000, 0)
	mustT(t, e.st.UpdateChat(e.ctx, id, chat, store.ChatUpdate{LastMessageAt: &at}))
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
