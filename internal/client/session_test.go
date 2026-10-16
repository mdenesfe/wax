package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/waadv"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/internal/waversion"
)

// Placeholder test number; no real account is involved.
const testUser = "905550000000"

type harness struct {
	t       testing.TB
	ctx     context.Context
	store   *store.Store
	srv     *fakewa.Server
	client  *Client
	events  chan Event
	runErr  chan error
	inst    string
	backlog []Event
}

func newHarness(t testing.TB, tweak func(*Config)) *harness {
	t.Helper()
	dsn := os.Getenv("WAX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAX_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, err := store.Open(ctx, dsn, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv, err := fakewa.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	h := &harness{
		t: t, ctx: ctx, store: st, srv: srv,
		events: make(chan Event, 1024), runErr: make(chan error, 1),
		inst: fmt.Sprintf("client-%s-%d", t.Name(), time.Now().UnixNano()),
	}
	t.Cleanup(func() { st.DeleteDevice(context.Background(), h.inst) })
	cfg := Config{
		Store: st, InstanceID: h.inst, Device: DefaultDeviceConfig(waversion.Default),
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		URL:      srv.URL,
		Verifier: srv.Verifier,
		OnEvent:  func(e Event) { h.events <- e },
		Backoff:  Backoff{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h.client, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go func() { h.runErr <- h.client.Run(runCtx) }()
	return h
}

func (h *harness) accept() *fakewa.Conn {
	h.t.Helper()
	c, err := h.srv.Accept(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// waitEvent returns the first event of type T. Other events are kept in a
// backlog for later waitEvent calls, so the order of waits does not matter.
func waitEvent[T Event](h *harness) T {
	h.t.Helper()
	for i, e := range h.backlog {
		if v, ok := e.(T); ok {
			h.backlog = append(h.backlog[:i], h.backlog[i+1:]...)
			return v
		}
	}
	for {
		select {
		case e := <-h.events:
			if v, ok := e.(T); ok {
				return v
			}
			h.backlog = append(h.backlog, e)
		case err := <-h.runErr:
			h.t.Fatalf("Run returned early: %v", err)
		case <-h.ctx.Done():
			var zero T
			h.t.Fatalf("timed out waiting for %T", zero)
		}
	}
}

func (h *harness) waitRun() error {
	h.t.Helper()
	select {
	case err := <-h.runErr:
		return err
	case <-h.ctx.Done():
		h.t.Fatal("timed out waiting for Run to return")
		return nil
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// pair drives a full QR pairing and the following login.
func (h *harness) pair() (*fakewa.Phone, *fakewa.Conn) {
	t, ctx := h.t, h.ctx
	c1 := h.accept()
	if c1.Payload.GetDevicePairingData() == nil || c1.Payload.GetUsername() != 0 {
		t.Fatal("first connection should use the registration payload")
	}
	must(t, c1.Send(ctx, wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "set", "id": "pd-1", "xmlns": "md"},
		Content: []wabin.Node{{Tag: "pair-device", Content: []wabin.Node{
			{Tag: "ref", Content: []byte("2@ref-one")}, {Tag: "ref", Content: []byte("2@ref-two")},
		}}},
	}))
	ack, err := c1.ReadUntil(ctx, func(n *wabin.Node) bool { id, _ := n.AttrString("id"); return id == "pd-1" })
	must(t, err)
	if typ, _ := ack.AttrString("type"); typ != "result" {
		t.Fatalf("pair-device answered with %s", ack.String())
	}
	qrEvt := waitEvent[QRCode](h)
	qr, err := fakewa.ParseQR(qrEvt.Content)
	must(t, err)
	if qr.Ref != "2@ref-one" || qr.NoisePub != c1.ClientStatic || qrEvt.Total != 2 {
		t.Fatalf("unexpected QR %+v", qr)
	}

	phone, err := fakewa.NewPhone(testUser, 12)
	must(t, err)
	ps, err := phone.PairSuccess(qr, "ps-1")
	must(t, err)
	must(t, c1.Send(ctx, ps))
	confirm, err := c1.ReadUntil(ctx, func(n *wabin.Node) bool { id, _ := n.AttrString("id"); return id == "ps-1" })
	must(t, err)
	pds, ok := confirm.Child("pair-device-sign")
	if !ok {
		t.Fatalf("expected pair-device-sign, got %s", confirm.String())
	}
	di, _ := pds.Child("device-identity")
	raw, _ := di.Bytes()
	var signed waadv.ADVSignedDeviceIdentity
	must(t, proto.Unmarshal(raw, &signed))
	msg := append(append(append([]byte{6, 1}, signed.Details...), qr.IdentityPub[:]...), phone.Account.Pub[:]...)
	if !keys.VerifySlice(qr.IdentityPub[:], msg, signed.DeviceSignature) {
		t.Fatal("device signature in confirmation does not verify")
	}
	waitEvent[PairSuccess](h)

	// Server restarts the stream; the client must log in with the new identity.
	must(t, c1.Send(ctx, wabin.Node{Tag: "stream:error", Attrs: wabin.Attrs{"code": "515"}}))
	c2 := h.accept()
	c1.Close()
	if c2.Payload.GetUsername() != 905550000000 || c2.Payload.GetDevice() != 12 || !c2.Payload.GetPassive() || c2.Payload.GetDevicePairingData() != nil {
		t.Fatalf("second connection should log in, payload: %v", c2.Payload)
	}
	if c2.ClientStatic != c1.ClientStatic {
		t.Fatal("noise key changed between connections")
	}
	return phone, c2
}

// serveLogin answers the post-login IQs and returns the uploaded prekey list.
func (h *harness) serveLogin(c *fakewa.Conn, serverCount int) []wabin.Node {
	up := h.serveLoginUpload(c, serverCount)
	if up == nil {
		return nil
	}
	list, _ := up.Child("list")
	return list.Children()
}

// serveLoginUpload is serveLogin returning the whole prekey upload IQ.
func (h *harness) serveLoginUpload(c *fakewa.Conn, serverCount int) *wabin.Node {
	t, ctx := h.t, h.ctx
	must(t, c.Send(ctx, wabin.Node{Tag: "success", Attrs: wabin.Attrs{"t": "1700000000", "lid": wabin.JID{User: "1" + testUser, Server: wabin.ServerLID}}}))
	uploaded, err := c.ServeLogin(ctx, serverCount)
	must(t, err)
	if uploaded != nil {
		skey, ok := uploaded.Child("skey")
		if !ok {
			t.Fatal("upload without signed prekey")
		}
		ident, _ := uploaded.Child("identity")
		identPub, _ := ident.Bytes()
		val, _ := skey.Child("value")
		sig, _ := skey.Child("signature")
		v, _ := val.Bytes()
		sg, _ := sig.Bytes()
		if !keys.VerifySlice(identPub, append([]byte{keys.DJBType}, v...), sg) {
			t.Fatal("uploaded signed prekey signature invalid")
		}
	}
	waitEvent[Connected](h)
	return uploaded
}

func TestPairLoginAndLogout(t *testing.T) {
	h := newHarness(t, nil)
	_, c2 := h.pair()
	uploaded := h.serveLogin(c2, 0)
	if len(uploaded) != initialPreKeys {
		t.Fatalf("uploaded %d prekeys, want %d", len(uploaded), initialPreKeys)
	}
	n, err := h.store.UploadedPreKeyCount(h.ctx, h.inst)
	must(t, err)
	if n != initialPreKeys {
		t.Fatalf("db has %d uploaded prekeys", n)
	}
	dev, err := h.store.LoadDevice(h.ctx, h.inst)
	must(t, err)
	if !dev.IsPaired() || dev.JID.User != testUser || dev.LID.Server != wabin.ServerLID || len(dev.Account) == 0 {
		t.Fatalf("pairing not persisted: %+v", dev)
	}

	// Logout from the phone.
	must(t, c2.Send(h.ctx, wabin.Node{Tag: "stream:error", Attrs: wabin.Attrs{"code": "401"},
		Content: []wabin.Node{{Tag: "conflict", Attrs: wabin.Attrs{"type": "device_removed"}}}}))
	if err := h.waitRun(); !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("Run returned %v, want ErrLoggedOut", err)
	}
	if _, err := h.store.LoadDevice(h.ctx, h.inst); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("device should be deleted after logout")
	}
}

func TestRestartLogsInWithoutQR(t *testing.T) {
	h := newHarness(t, nil)
	_, c2 := h.pair()
	h.serveLogin(c2, 0)

	// Simulate a network failure: the client must reconnect and log in.
	c2.Close()
	waitEvent[Disconnected](h)
	c3 := h.accept()
	if c3.Payload.GetUsername() == 0 {
		t.Fatal("reconnect should use the login payload")
	}
	// Prekeys are already on the server: no upload expected.
	if up := h.serveLogin(c3, 812); up != nil {
		t.Fatalf("unexpected prekey upload of %d keys", len(up))
	}
}

func TestReplacedStops(t *testing.T) {
	h := newHarness(t, nil)
	c := h.accept()
	must(t, c.Send(h.ctx, wabin.Node{Tag: "stream:error", Content: []wabin.Node{{Tag: "conflict", Attrs: wabin.Attrs{"type": "replaced"}}}}))
	if err := h.waitRun(); !errors.Is(err, ErrReplaced) {
		t.Fatalf("got %v", err)
	}
}

func TestOutdatedStops(t *testing.T) {
	h := newHarness(t, nil)
	c := h.accept()
	must(t, c.Send(h.ctx, wabin.Node{Tag: "failure", Attrs: wabin.Attrs{"reason": "405"}}))
	if err := h.waitRun(); !errors.Is(err, ErrClientOutdated) {
		t.Fatalf("got %v", err)
	}
}

func TestKeepAliveTimeoutReconnects(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.KeepAliveMin, c.KeepAliveMax = 20*time.Millisecond, 30*time.Millisecond
		c.KeepAliveTimeout = 20 * time.Millisecond
		c.KeepAliveMaxFailure = 100 * time.Millisecond
	})
	h.accept() // never answers pings
	waitEvent[KeepAliveTimeout](h)
	d := waitEvent[Disconnected](h)
	if d.Action != ActionReconnect || !strings.Contains(d.Err.Error(), "keepalive") {
		t.Fatalf("unexpected disconnect %+v", d)
	}
	h.accept()
}

func TestQRTimeout(t *testing.T) {
	h := newHarness(t, nil)
	c := h.accept()
	// Shorten the wait by sending a single ref; the client still waits the
	// first-ref timeout, so only check that the QR is shown and that a
	// broken connection while waiting leads to reconnection, not a hang.
	must(t, c.Send(h.ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "set", "id": "x", "xmlns": "md"},
		Content: []wabin.Node{{Tag: "pair-device", Content: []wabin.Node{{Tag: "ref", Content: []byte("2@only")}}}}}))
	waitEvent[QRCode](h)
	c.Close()
	waitEvent[Disconnected](h)
	h.accept()
}
