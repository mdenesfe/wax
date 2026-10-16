package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/manager"
	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
)

// callNode is call against a specific node's API.
func (e *env) callNode(n *node, method, path string, body any) (int, []byte) {
	e.t.Helper()
	saved := e.api
	e.api = n.api
	defer func() { e.api = saved }()
	return e.call(method, path, body, e.key)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serveRelogin accepts the next connection from an already paired instance
// and logs it in, as the real server does after a takeover.
func (e *env) serveRelogin() *fakewa.Conn {
	e.t.Helper()
	c, err := e.srv.Accept(e.ctx)
	mustT(e.t, err)
	mustT(e.t, c.Send(e.ctx, wabin.Node{Tag: "success", Attrs: wabin.Attrs{"t": "1700000000",
		"lid": wabin.JID{User: "1905550000001", Device: 3, Server: wabin.ServerLID}}}))
	_, err = c.ServeLogin(e.ctx, 812)
	mustT(e.t, err)
	return c
}

func (e *env) statusVia(n *node, name string) string {
	e.t.Helper()
	code, body := e.callNode(n, "GET", "/v1/instances/"+name, nil)
	if code != http.StatusOK {
		e.t.Fatalf("GET %s via node: %d %s", name, code, body)
	}
	var v instanceJSON
	mustT(e.t, json.Unmarshal(body, &v))
	return v.Status
}

func runs(n *node, id string) bool { return slices.Contains(n.m.RunningIDs(), id) }

// TestClusterForwardingAndFailover runs one paired instance on a two-node
// cluster: requests to the other node are forwarded to the owner; when the
// owner shuts down the other node takes over at once, and when a node
// crashes (no release) the takeover waits for the lease to expire.
func TestClusterForwardingAndFailover(t *testing.T) {
	e := setup(t)
	a := &node{m: e.m, api: e.api}
	e.ok("POST", "/v1/instances", map[string]any{"id": "main"}, http.StatusCreated)
	id := manager.InternalID(e.tenant, "main")
	if !runs(a, id) {
		t.Fatal("the creating node should run the new instance")
	}
	e.pairInstance("main")
	waitFor(t, "connected on node a", 5*time.Second, func() bool { return e.statusVia(a, "main") == "connected" })

	b := e.startNode("b")
	if runs(b, id) {
		t.Fatal("instance runs on two nodes")
	}
	// Forwarded: node b does not run it, but answers through node a.
	if st := e.statusVia(b, "main"); st != "connected" {
		t.Fatalf("status via node b: %q", st)
	}
	var list []instanceJSON
	code, body := e.callNode(b, "GET", "/v1/instances", nil)
	mustT(t, json.Unmarshal(body, &list))
	if code != 200 || len(list) != 1 || list[0].Status != "connected" || list[0].JID == "" {
		t.Fatalf("listing via node b: %d %s", code, body)
	}

	// Graceful shutdown of a: b takes over within one lease round, without
	// waiting for the lease to expire, and logs in without a QR code.
	start := time.Now()
	a.m.Stop()
	e.serveRelogin()
	waitFor(t, "node b to run the instance", 5*time.Second, func() bool { return runs(b, id) })
	// Released leases are claimed between rounds: far sooner than an expiry.
	if took := time.Since(start); took > testLeaseTTL/2 {
		t.Errorf("graceful takeover took %v: it waited for the lease to expire", took)
	}
	waitFor(t, "connected on node b", 5*time.Second, func() bool { return e.statusVia(b, "main") == "connected" })

	// Crash of b (context gone, leases not released): node c may only take
	// over after the lease expired, never while b could still run it.
	c := e.startNode("c")
	crashed := time.Now()
	b.stop()
	e.serveRelogin()
	waitFor(t, "node c to run the instance", 5*time.Second, func() bool { return runs(c, id) })
	if took := time.Since(crashed); took < testLeaseTTL/2 {
		t.Errorf("takeover after a crash after %v: before the lease could expire", took)
	}
	waitFor(t, "connected on node c", 5*time.Second, func() bool { return e.statusVia(c, "main") == "connected" })
}

// TestClusterBalancing checks that a joining node takes its fair share.
func TestClusterBalancing(t *testing.T) {
	e := setup(t)
	a := &node{m: e.m, api: e.api}
	var ids []string
	for i := range 4 {
		name := fmt.Sprintf("bal-%d", i)
		e.ok("POST", "/v1/instances", map[string]any{"id": name}, http.StatusCreated)
		ids = append(ids, manager.InternalID(e.tenant, name))
	}
	waitFor(t, "node a to run all four", 5*time.Second, func() bool { return len(a.m.RunningIDs()) == 4 })

	b := e.startNode("b")
	waitFor(t, "an even split", 5*time.Second, func() bool {
		return len(a.m.RunningIDs()) == 2 && len(b.m.RunningIDs()) == 2
	})
	// Every instance runs on exactly one node.
	for _, id := range ids {
		if runs(a, id) == runs(b, id) {
			t.Fatalf("instance %s: on a=%v, on b=%v", id, runs(a, id), runs(b, id))
		}
	}
}
