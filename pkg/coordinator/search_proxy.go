package coordinator

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/giongto35/cloud-game/v3/pkg/logger"
)

// newSemanticSearchProxy returns an http.Handler that reverse-proxies
// /v1/search/semantic to the worker's HTTP server on localhost:9000.
// Used by Phase-3 so the browser can reach the vector-search endpoint
// through the same origin it already talks to for WebSocket + static
// files. The worker's port is fixed at :9000 by worker.conf.Worker.Server.
//
// In a multi-worker deploy this needs to become a router that picks an
// available worker from the hub; for single-container cloudplay that's
// over-engineering. Keep simple until shape demands more.
func newSemanticSearchProxy(log *logger.Logger) http.Handler {
	target, _ := url.Parse("http://localhost:9000")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Debug().Err(err).Msg("[coord] semantic-search proxy error; worker may be down")
		// Soft-fail: the frontend degrades to fuzzy-only on empty result,
		// and a 503 there would bubble up as "search feature broken"
		// even though the fuzzy fallback is working fine.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":[]}`))
	}
	return proxy
}
