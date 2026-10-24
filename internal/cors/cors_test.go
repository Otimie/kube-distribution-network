package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// next stands in for the proxy: read-only, like the real one.
var next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	_, _ = w.Write([]byte("ok"))
})

func serve(h http.Handler, method, origin string, extra map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/nodes", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for k, v := range extra {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOffByDefault(t *testing.T) {
	w := serve(Wrap(next, "", false), http.MethodGet, "https://a.example", nil)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q with CORS off", got)
	}
	pre := serve(Wrap(next, "", false), http.MethodOptions, "https://a.example",
		map[string]string{"Access-Control-Request-Method": "GET"})
	if pre.Code != http.StatusMethodNotAllowed {
		t.Errorf("preflight with CORS off: status %d, want 405 from the proxy", pre.Code)
	}
}

func TestGetCarriesHeaders(t *testing.T) {
	h := Wrap(next, "https://dash.example", false)
	w := serve(h, http.MethodGet, "https://dash.example", nil)
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("GET: %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://dash.example" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if w.Header().Get("Access-Control-Expose-Headers") == "" {
		t.Error("no Access-Control-Expose-Headers: pages could not read ETag or X-Kdn-*")
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=30" {
		t.Errorf("Cache-Control = %q; the proxy's must pass through", got)
	}
}

// The CDN caches one copy per URL, so the headers must not depend on who asked.
func TestHeadersIndependentOfOrigin(t *testing.T) {
	h := Wrap(next, "https://dash.example", false)
	for _, origin := range []string{"", "https://dash.example", "https://evil.example"} {
		w := serve(h, http.MethodGet, origin, nil)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://dash.example" {
			t.Errorf("Origin %q: Access-Control-Allow-Origin = %q", origin, got)
		}
		if got := w.Header().Get("Vary"); got != "" {
			t.Errorf("Origin %q: Vary = %q; the CDN cache ignores it", origin, got)
		}
	}
}

func TestPreflight(t *testing.T) {
	w := serve(Wrap(next, "*", false), http.MethodOptions, "https://dash.example",
		map[string]string{"Access-Control-Request-Method": "GET"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status %d, want 204", w.Code)
	}
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, HEAD",
		"Cache-Control":                "no-store",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// A bare OPTIONS is not a preflight; the read-only proxy still refuses it.
func TestBareOptionsReachesProxy(t *testing.T) {
	w := serve(Wrap(next, "*", false), http.MethodOptions, "", nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("bare OPTIONS: status %d, want 405", w.Code)
	}
}

func TestCredentials(t *testing.T) {
	for _, allow := range []bool{false, true} {
		h := Wrap(next, "https://dash.example", allow)
		want := ""
		if allow {
			want = "true"
		}
		get := serve(h, http.MethodGet, "https://dash.example", nil)
		pre := serve(h, http.MethodOptions, "https://dash.example",
			map[string]string{"Access-Control-Request-Method": "GET"})
		for name, w := range map[string]*httptest.ResponseRecorder{"GET": get, "preflight": pre} {
			if got := w.Header().Get("Access-Control-Allow-Credentials"); got != want {
				t.Errorf("allowCredentials %v, %s: Access-Control-Allow-Credentials = %q, want %q", allow, name, got, want)
			}
		}
	}
}
