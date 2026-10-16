package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesInterfaceWithCSP(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("/ui", Handler()))
	defer srv.Close()
	for _, c := range []struct{ path, contentType, contains string }{
		{"/ui/", "text/html", `<script src="app.js"`},
		{"/ui/app.js", "javascript", "textContent"},
		{"/ui/app.css", "text/css", "--background"},
	} {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		buf := make([]byte, 1<<16)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), c.contentType) || !strings.Contains(b.String(), c.contains) {
			t.Errorf("%s: %d %s", c.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP %q", c.path, csp)
		}
	}
}

// The interface must never put server data into HTML: no innerHTML,
// insertAdjacentHTML, document.write or eval, and no inline styles (which
// the CSP would block).
func TestNoUnsafeDOM(t *testing.T) {
	js, err := files.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "style:"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
}
