// Package cors lets browser pages on another origin read the proxy's
// responses.
//
// It wraps the proxy handler: it adds the CORS headers to every response and
// answers preflight requests itself, so the read-only proxy never sees an
// OPTIONS request it would refuse.
package cors

import (
	"net/http"
	"strings"
)

// Headers a browser page may read beyond the CORS-safelisted ones.
var exposedHeaders = strings.Join([]string{"ETag", "Age", "X-Kdn-Cache", "X-Kdn-Resource-Version"}, ", ")

// Request headers a page may send. Edge clients are anonymous, so there is no
// Authorization here.
var allowedHeaders = strings.Join([]string{"Accept", "If-None-Match"}, ", ")

// Wrap returns next with CORS applied for allowedOrigin, an origin like
// "https://dashboard.example.com" or "*". An empty allowedOrigin returns next
// unchanged, so CORS stays off by default.
//
// The headers are the same on every response, whatever the request's Origin.
// That is deliberate: the CDN caches one copy per URL and ignores Vary: Origin,
// so a response that depended on Origin could be served to the wrong page.
// Browsers on other origins still cannot read it, since the header names one
// origin.
func Wrap(next http.Handler, allowedOrigin string) http.Handler {
	if allowedOrigin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Access-Control-Allow-Origin", allowedOrigin)

		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			hdr.Set("Access-Control-Allow-Methods", "GET, HEAD")
			hdr.Set("Access-Control-Allow-Headers", allowedHeaders)
			hdr.Set("Access-Control-Max-Age", "86400")
			hdr.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		hdr.Set("Access-Control-Expose-Headers", exposedHeaders)
		next.ServeHTTP(w, r)
	})
}
