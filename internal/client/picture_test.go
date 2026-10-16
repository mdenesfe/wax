package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
)

// A 1x1 PNG.
var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// TestProfilePicture: the picture is looked up once and then cached, and
// so is a missing one; the request carries the target and asks for a URL.
func TestProfilePicture(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	h.client.mediaScheme = "http"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPNG) }))
	t.Cleanup(cdn.Close)

	bob := wabin.JID{User: bobLID.User, Server: wabin.ServerLID}
	got := make(chan error, 1)
	var pic *Picture
	go func() {
		var err error
		pic, err = h.client.ProfilePicture(h.ctx, bob)
		got <- err
	}()
	q, err := c.ReadIQ(h.ctx, "w:profile:picture")
	must(t, err)
	if target, _ := q.AttrJID("target"); target != bob {
		t.Fatalf("target %v", q.Attrs["target"])
	}
	if p, ok := q.Child("picture"); !ok || p.Attrs["type"] != "preview" || p.Attrs["query"] != "url" {
		t.Fatalf("query %s", q.String())
	}
	must(t, c.Send(h.ctx, fakewa.Result(q.Attrs["id"].(string), wabin.Node{Tag: "picture", Attrs: wabin.Attrs{"id": "1", "url": cdn.URL + "/p.jpg"}})))
	must(t, <-got)
	if pic.ContentType != "image/png" || len(pic.Data) != len(tinyPNG) {
		t.Fatalf("picture %s, %d bytes", pic.ContentType, len(pic.Data))
	}
	// Cached: no second IQ (one would block without an answer).
	if again, err := h.client.ProfilePicture(h.ctx, bob); err != nil || again != pic {
		t.Fatalf("not cached: %v", err)
	}

	// No picture (or hidden): 404 or 401, also cached.
	carol := wabin.JID{User: "777000222", Server: wabin.ServerLID}
	go func() {
		_, err := h.client.ProfilePicture(h.ctx, carol)
		got <- err
	}()
	q, err = c.ReadIQ(h.ctx, "w:profile:picture")
	must(t, err)
	must(t, c.Send(h.ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "error", "id": q.Attrs["id"]},
		Content: []wabin.Node{{Tag: "error", Attrs: wabin.Attrs{"code": "401", "text": "not-authorized"}}}}))
	if err := <-got; !errors.Is(err, ErrNoPicture) {
		t.Fatalf("hidden picture: %v", err)
	}
	if _, err := h.client.ProfilePicture(h.ctx, carol); !errors.Is(err, ErrNoPicture) {
		t.Fatalf("absence not cached: %v", err)
	}
}
