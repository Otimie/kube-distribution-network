package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/kube-distribution-network/internal/cache"
	"github.com/example/kube-distribution-network/internal/config"
	"github.com/example/kube-distribution-network/internal/upstream"
)

// fakeAPIServer stands in for the Kubernetes API server and records what it was
// asked for, so tests can assert on what the proxy did and did not forward.
type fakeAPIServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	hits     atomic.Int64

	delay  time.Duration
	status int
	body   string
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{
		status: http.StatusOK,
		body:   `{"kind":"PodList","metadata":{"resourceVersion":"4242"},"items":[]}`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(r.Context()))
		delay, status, body := f.delay, f.status, f.body
		f.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		// Headers a real API server sends that must not reach a client.
		w.Header().Set("Audit-Id", "secret-audit-id")
		w.Header().Set("Cache-Control", "no-cache, private")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPIServer) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

func newTestHandler(t *testing.T, f *fakeAPIServer, mutate func(*config.Config)) *Handler {
	t.Helper()

	cfg := config.Default()
	cfg.Upstream.Server = f.srv.URL
	cfg.Upstream.CAFile = ""
	cfg.Upstream.TokenFile = ""
	cfg.Upstream.Timeout = config.Duration(5 * time.Second)
	cfg.Rules = []config.Rule{
		{Version: "v1", Resource: "pods", TTL: config.Duration(30 * time.Second)},
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	up, err := upstream.New(cfg.Upstream.Server, "", "", false, cfg.Upstream.Timeout.D(), cfg.UpstreamConcurrency)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	return New(cfg, cache.New(cfg.CacheMaxBytes), up, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(h *Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServesAndCaches(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	first := do(h, http.MethodGet, "/api/v1/namespaces/default/pods", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("got %d, body %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Kdn-Cache"); got != "miss" {
		t.Errorf("X-Kdn-Cache = %q, want miss", got)
	}
	if cc := first.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=30") {
		t.Errorf("Cache-Control = %q, want max-age=30", cc)
	}
	if got := first.Header().Get("X-Kdn-Resource-Version"); got != "4242" {
		t.Errorf("X-Kdn-Resource-Version = %q, want 4242", got)
	}

	second := do(h, http.MethodGet, "/api/v1/namespaces/default/pods", nil)
	if got := second.Header().Get("X-Kdn-Cache"); got != "hit" {
		t.Errorf("second request X-Kdn-Cache = %q, want hit", got)
	}
	if f.hits.Load() != 1 {
		t.Errorf("API server saw %d requests, want 1", f.hits.Load())
	}
}

// The core protection: a stampede of identical misses reaches the API server as
// a single request.
func TestConcurrentMissesCoalesce(t *testing.T) {
	f := newFakeAPIServer(t)
	f.delay = 150 * time.Millisecond
	h := newTestHandler(t, f, nil)

	const callers = 100
	var wg sync.WaitGroup
	codes := make([]int, callers)
	wg.Add(callers)
	for i := range callers {
		go func(i int) {
			defer wg.Done()
			codes[i] = do(h, http.MethodGet, "/api/v1/pods", nil).Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("caller %d got %d", i, c)
		}
	}
	if hits := f.hits.Load(); hits != 1 {
		t.Errorf("API server saw %d requests from %d simultaneous callers, want 1", hits, callers)
	}
}

func TestWatchIsRefused(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	for _, q := range []string{"watch=true", "watch=1", "watch=TRUE"} {
		rec := do(h, http.MethodGet, "/api/v1/pods?"+q, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: got %d, want 501", q, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"kind":"Status"`) {
			t.Errorf("%s: response is not a Status object: %s", q, rec.Body.String())
		}
	}
	if f.hits.Load() != 0 {
		t.Error("a watch must be refused at the proxy, never opened against the API server")
	}
}

func TestWritesAreRefused(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if rec := do(h, m, "/api/v1/pods", nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", m, rec.Code)
		}
	}
	if f.hits.Load() != 0 {
		t.Error("a mutating request must never be forwarded")
	}
}

// Resources outside the allowlist are refused by the proxy itself, without the
// request reaching the API server.
func TestDeniedResourceNeverReachesUpstream(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	for _, path := range []string{
		"/api/v1/secrets",
		"/api/v1/namespaces/default/secrets/db-password",
		"/apis/rbac.authorization.k8s.io/v1/clusterroles",
		// A subresource of an allowlisted resource is still not served.
		"/api/v1/namespaces/default/pods/web-0/log",
	} {
		if rec := do(h, http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s: got %d, want 403", path, rec.Code)
		}
	}
	if f.hits.Load() != 0 {
		t.Errorf("API server saw %d requests for denied resources, want 0", f.hits.Load())
	}
}

// A namespace-restricted rule must not be satisfiable by a cluster-wide read,
// which would return objects from every namespace.
func TestNamespaceRestriction(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, func(c *config.Config) {
		c.Rules[0].Namespaces = []string{"public", "team-*"}
	})

	for _, path := range []string{
		"/api/v1/namespaces/public/pods",
		"/api/v1/namespaces/team-blue/pods",
	} {
		if rec := do(h, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d, want 200", path, rec.Code)
		}
	}
	for _, path := range []string{
		"/api/v1/namespaces/private/pods",
		"/api/v1/pods", // cluster-wide
	} {
		if rec := do(h, http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s: got %d, want 403", path, rec.Code)
		}
	}
}

// Client credentials are never forwarded: the API server only sees the proxy's
// own identity, so a caller cannot escalate past the allowlist with their own
// token or an impersonation header.
func TestClientCredentialsAreNotForwarded(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	rec := do(h, http.MethodGet, "/api/v1/pods", map[string]string{
		"Authorization":    "Bearer attacker-supplied-token",
		"Cookie":           "session=abc",
		"Impersonate-User": "system:admin",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := f.lastRequest()
	if got == nil {
		t.Fatal("upstream received no request")
	}
	if strings.Contains(got.Header.Get("Authorization"), "attacker-supplied-token") {
		t.Error("the client's token was forwarded upstream")
	}
	for _, h := range []string{"Cookie", "Impersonate-User"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("%s was forwarded upstream: %q", h, v)
		}
	}
}

// Upstream headers are not copied through, so audit IDs and the API server's
// own no-cache directives do not leak or confuse the CDN.
func TestUpstreamHeadersAreNotForwarded(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	rec := do(h, http.MethodGet, "/api/v1/pods", nil)
	if v := rec.Header().Get("Audit-Id"); v != "" {
		t.Errorf("Audit-Id leaked to the client: %q", v)
	}
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "private") {
		t.Errorf("upstream Cache-Control leaked through: %q", cc)
	}
}

func TestConditionalRequestReturns304(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	etag := do(h, http.MethodGet, "/api/v1/pods", nil).Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag was issued")
	}
	rec := do(h, http.MethodGet, "/api/v1/pods", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Errorf("got %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 must carry no body, got %d bytes", rec.Body.Len())
	}
}

// A read pinned to an exact resourceVersion is one immutable state of the
// cluster and can be cached indefinitely.
func TestPinnedResourceVersionIsImmutable(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	if cc := do(h, http.MethodGet, "/api/v1/pods?resourceVersion=4242", nil).Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	// resourceVersion=0 means "any version the server has handy", so it is not
	// pinned and gets the ordinary lifetime.
	if cc := do(h, http.MethodGet, "/api/v1/pods?resourceVersion=0", nil).Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("resourceVersion=0 must not be immutable, got %q", cc)
	}
}

// Enumerating names that do not exist is the cheapest way to defeat a
// read-through cache, so upstream refusals are cached briefly too.
func TestNotFoundIsCached(t *testing.T) {
	f := newFakeAPIServer(t)
	f.status = http.StatusNotFound
	f.body = `{"kind":"Status","code":404}`
	h := newTestHandler(t, f, nil)

	for i := range 5 {
		if rec := do(h, http.MethodGet, "/api/v1/namespaces/default/pods/nope", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("request %d: got %d, want 404", i, rec.Code)
		}
	}
	if f.hits.Load() != 1 {
		t.Errorf("API server saw %d requests for a nonexistent object, want 1", f.hits.Load())
	}
}

// When the API server is failing, stale data beats propagating the outage.
func TestStaleIsServedWhenUpstreamFails(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	if rec := do(h, http.MethodGet, "/api/v1/pods", nil); rec.Code != http.StatusOK {
		t.Fatalf("warming request got %d", rec.Code)
	}
	f.srv.Close() // break upstream entirely

	// Still fresh, so this is an ordinary hit.
	if rec := do(h, http.MethodGet, "/api/v1/pods", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d while still fresh", rec.Code)
	}

	// Expire the entry and confirm the stale copy is served rather than an error.
	entry, ok := h.cache.Get(cacheKey("/api/v1/pods", nil))
	if !ok {
		t.Fatal("entry was not cached")
	}
	entry.StoredAt = time.Now().Add(-2 * (entry.FreshFor + entry.StaleFor))

	rec := do(h, http.MethodGet, "/api/v1/pods", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 served from stale cache", rec.Code)
	}
	if got := rec.Header().Get("X-Kdn-Cache"); got != "stale-error" {
		t.Errorf("X-Kdn-Cache = %q, want stale-error", got)
	}
}

func TestDiscoveryIsServed(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	for _, path := range []string{"/api", "/api/v1", "/apis", "/version", "/openapi/v2"} {
		if rec := do(h, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d, want 200", path, rec.Code)
		}
	}

	// Paths off the resource surface are forwarded, and the API server decides
	// them: the service account is granted no nonResourceURLs, so /metrics and
	// /debug/pprof come back as a 403 that the proxy then caches. This test
	// pins that delegation, so a reader does not mistake it for an oversight.
	f.status = http.StatusForbidden
	f.body = `{"kind":"Status","code":403}`
	for _, path := range []string{"/metrics", "/logs", "/debug/pprof/heap"} {
		if rec := do(h, http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s: got %d, want the API server's 403 passed through", path, rec.Code)
		}
	}
}

func TestCacheKeyIsOrderIndependent(t *testing.T) {
	a := mustQuery("limit=500&resourceVersion=0")
	b := mustQuery("resourceVersion=0&limit=500")
	if cacheKey("/api/v1/pods", a) != cacheKey("/api/v1/pods", b) {
		t.Error("parameter ordering alone must not fork the cache key")
	}
	if cacheKey("/api/v1/pods", a) == cacheKey("/api/v1/pods", mustQuery("limit=100")) {
		t.Error("different parameter values must produce different cache keys")
	}
}

func TestHeadReturnsHeadersWithoutBody(t *testing.T) {
	f := newFakeAPIServer(t)
	h := newTestHandler(t, f, nil)

	rec := do(h, http.MethodHead, "/api/v1/pods", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must carry no body, got %d bytes", rec.Body.Len())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("HEAD should still carry the validator")
	}
}
