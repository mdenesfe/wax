package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/manager"
)

// TestStopAndResume: stop keeps the device but runs the instance nowhere,
// also on a node that joins later; connect resumes it without a QR code.
func TestStopAndResume(t *testing.T) {
	e := setup(t)
	a := &node{m: e.m, api: e.api}
	e.ok("POST", "/v1/instances", map[string]any{"id": "main"}, http.StatusCreated)
	id := manager.InternalID(e.tenant, "main")
	e.pairInstance("main")
	waitFor(t, "connected", 5*time.Second, func() bool { return e.statusVia(a, "main") == "connected" })

	e.ok("POST", "/v1/instances/main/stop", nil, http.StatusNoContent)
	if runs(a, id) {
		t.Fatal("still running after stop")
	}
	if st := e.statusVia(a, "main"); st != "stopped" {
		t.Fatalf("status after stop: %q", st)
	}
	// Another node does not pick it up, not even after a lease round.
	b := e.startNode("b")
	time.Sleep(testLeaseTTL / 2)
	if runs(a, id) || runs(b, id) {
		t.Fatal("a node runs the stopped instance")
	}
	if st := e.statusVia(b, "main"); st != "stopped" {
		t.Fatalf("status via node b: %q", st)
	}
	// Stopping again is harmless.
	e.ok("POST", "/v1/instances/main/stop", nil, http.StatusNoContent)

	// Connect resumes it with the stored session: no pairing.
	e.ok("POST", "/v1/instances/main/connect", nil, http.StatusAccepted)
	e.serveRelogin()
	waitFor(t, "connected again", 5*time.Second, func() bool { return e.statusVia(a, "main") == "connected" })
	if runs(a, id) == runs(b, id) {
		t.Fatalf("after resume: on a=%v, on b=%v", runs(a, id), runs(b, id))
	}
}

// TestCheckPhonesRequest: the request is validated before WhatsApp is
// asked, and needs a connected account.
func TestCheckPhonesRequest(t *testing.T) {
	e := setup(t)
	e.ok("POST", "/v1/instances", map[string]any{"id": "chk"}, http.StatusCreated)
	many := make([]string, maxPhoneChecks+1)
	for i := range many {
		many[i] = "905550000030"
	}
	for _, body := range []map[string]any{
		{"phones": []string{}},
		{"phones": many},
		{"phones": []string{"120363000000000001@g.us"}},
		{"phones": []string{"not a number"}},
		{"numbers": []string{"905550000030"}},
	} {
		e.ok("POST", "/v1/instances/chk/contacts/check", body, http.StatusBadRequest)
	}
	e.ok("POST", "/v1/instances/chk/contacts/check", map[string]any{"phones": []string{"+905550000030"}}, http.StatusServiceUnavailable)
}
