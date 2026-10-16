// Package webui serves the admin web interface: static HTML, CSS and
// JavaScript embedded in the binary, calling the REST API of the same
// origin with the key the user signs in with.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// csp allows only the interface's own files: no inline scripts, no third
// party code, no framing. Images and media may be blobs (QR codes,
// pictures and attachments are fetched with the API key).
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; media-src blob:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the interface under the prefix it is mounted at (e.g.
// "/ui/"); the caller strips the prefix.
func Handler() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // embedded at build time
	}
	fileServer := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
