package api

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/mdenesfe/wax/internal/store"
)

// forwardedHeader marks a request one node passed to another; it carries
// the sending node's id.
const forwardedHeader = "X-Wax-Forwarded"

// forward passes a request to the node that runs the instance. The
// Authorization header goes along and is checked again there, so nodes do
// not trust each other beyond the network they share (keep it private, or
// terminate TLS between nodes).
func (s *Server) forward(w http.ResponseWriter, r *http.Request, owner *store.Lease) {
	target, err := url.Parse(owner.Address)
	if err != nil || target.Host == "" {
		s.log.Error("owner node has an invalid address", "node", owner.Node, "address", owner.Address)
		writeError(w, http.StatusServiceUnavailable, errNodeUnreachable)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Set(forwardedHeader, s.signForward(pr.In))
		},
		Transport: s.nodeHTTP,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("forwarding to owner node failed", "node", owner.Node, "path", r.URL.Path, "error", err)
			writeError(w, http.StatusServiceUnavailable, errNodeUnreachable)
		},
	}
	proxy.ServeHTTP(w, r)
}
