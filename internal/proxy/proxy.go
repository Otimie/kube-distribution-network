// Package proxy is the read-only caching reverse proxy for the Kubernetes API.
//
// Request handling, in order: refuse anything that is not a read, refuse
// streaming reads, check the allowlist, then serve from cache or fetch upstream
// with concurrent identical misses coalesced into one API server call.
//
// Client credentials are never forwarded. The proxy reads the cluster with its
// own service account, which makes the allowlist the authorization boundary.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/example/kube-distribution-network/internal/cache"
	"github.com/example/kube-distribution-network/internal/config"
	"github.com/example/kube-distribution-network/internal/upstream"
)

const (
	// pinnedTTL is the max-age for a read at an exact resourceVersion. Such a
	// response describes one immutable state of the cluster and can never
	// change, so it is cacheable indefinitely. This is the biggest win here: a
	// client that pins a resourceVersion gets permanent edge hits.
	pinnedTTL = 365 * 24 * time.Hour

	// discoveryTTL applies to /api, /apis, /version and /openapi, which clients
	// need to bootstrap and which rarely change.
	discoveryTTL = 5 * time.Minute

	// negativeTTL briefly caches 403 and 404. Without it, a caller enumerating
	// names that do not exist reaches the API server every time, which is the
	// cheapest way to defeat a read-through cache.
	negativeTTL = 5 * time.Second

	// staleFor is how long past freshness an entry may still be served: behind a
	// background refresh, or immediately if the API server is failing.
	staleFor = 10 * time.Minute

	// paginatedTTL caps pages fetched with a continue token, which is opaque and
	// expires upstream.
	paginatedTTL = 60 * time.Second
)

// Handler serves proxied API requests.
type Handler struct {
	cfg      *config.Config
	cache    *cache.Cache
	upstream *upstream.Client
	log      *slog.Logger

	// rules is indexed by "group/resource".
	rules map[string][]*config.Rule
}

// New builds a Handler.
func New(cfg *config.Config, c *cache.Cache, up *upstream.Client, log *slog.Logger) *Handler {
	h := &Handler{cfg: cfg, cache: c, upstream: up, log: log, rules: map[string][]*config.Rule{}}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		h.rules[r.Group+"/"+r.Resource] = append(h.rules[r.Group+"/"+r.Resource], r)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read-only. A write cannot be cached, and forwarding one would let an
	// anonymous caller change cluster state through the proxy's service account.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed",
			"this proxy serves read-only requests; "+r.Method+" is not supported")
		return
	}

	t, err := parsePath(r.URL.Path)
	if err != nil {
		writeStatus(w, http.StatusNotFound, "NotFound",
			"the path "+r.URL.Path+" is not a recognized Kubernetes API path")
		return
	}

	// Watch is refused. It is an open-ended stream no cache can serve, and
	// accepting one would hold a long-lived connection against the very API
	// server this exists to protect.
	if truthy(r.URL.Query().Get("watch")) {
		writeStatus(w, http.StatusNotImplemented, "NotImplemented",
			"watch is not supported by this proxy: a long-lived watch stream cannot be served "+
				"from a cache. Use repeated list requests, which are cached, instead.")
		return
	}

	ttl, labels, err := h.authorize(t)
	if err != nil {
		writeStatus(w, http.StatusForbidden, "Forbidden", err.Error())
		return
	}

	query := r.URL.Query()
	var gate *labelGate
	if len(labels) > 0 {
		if t.name == "" {
			// A list: the API server filters it, so pagination and
			// remainingItemCount stay correct for the published set.
			requireLabels(query, labels)
		} else {
			gate = &labelGate{labels: labels, notFound: statusBody(http.StatusNotFound, "NotFound",
				t.resource+" \""+t.name+"\" not found")}
		}
	}
	// The key covers the query as sent upstream, required labels included.
	key := cacheKey(r.URL.Path, query)

	entry, status := h.resolve(r.Context(), key, r.URL.Path, query, ttl, gate)
	if entry == nil {
		h.log.Error("upstream request failed", "path", r.URL.Path, "route", t.route())
		writeStatus(w, http.StatusBadGateway, "InternalError",
			"the proxy could not reach the Kubernetes API server")
		return
	}
	h.write(w, r, entry, query, ttl, status)
}

// authorize checks the allowlist and returns the resource's TTL and the labels
// an object must carry to be served.
func (h *Handler) authorize(t *target) (time.Duration, map[string]string, error) {
	// Non-resource paths (/api, /apis, /version, /openapi, and anything else
	// off the resource surface) are forwarded, and the API server's own RBAC
	// decides them. The service account is granted no nonResourceURLs, so it
	// gets a cached 403 for /metrics, /logs and /debug/pprof; the default
	// system:discovery and system:public-info-viewer bindings allow discovery,
	// /healthz and /version. The CDN Worker additionally restricts which path
	// prefixes reach the origin at all.
	if t.discovery {
		return discoveryTTL, nil, nil
	}

	var rule *config.Rule
	for _, candidate := range h.rules[t.group+"/"+t.resource] {
		if candidate.Version == "*" || candidate.Version == t.version {
			rule = candidate
			break
		}
	}
	if rule == nil {
		return 0, nil, errors.New(t.route() + " is not in this proxy's allowlist")
	}

	// Subresources are not served. A readable resource does not imply its
	// subresources are readable -- pods/log exposes far more than a pod object.
	if t.subresource != "" {
		return 0, nil, errors.New("the " + t.subresource + " subresource is not served by this proxy")
	}

	if len(rule.Namespaces) > 0 {
		// A cluster-wide read would return objects from every namespace,
		// including ones outside this rule's list.
		if t.namespace == "" {
			return 0, nil, errors.New("reads of " + t.route() + " are restricted to specific namespaces, " +
				"so a cluster-wide read is not permitted; request a namespace instead")
		}
		if !namespaceAllowed(rule.Namespaces, t.namespace) {
			return 0, nil, errors.New("reads of " + t.route() + " in namespace " + t.namespace + " are not permitted")
		}
	}
	return rule.TTL.D(), rule.RequireLabels, nil
}

// labelGate hides a single object that lacks the required labels.
type labelGate struct {
	labels map[string]string
	// notFound is served both for an object without the labels and for one
	// that does not exist, so a caller cannot tell the two apart.
	notFound []byte
}

// requireLabels adds labels to the query's labelSelector. Selector terms are
// ANDed, so a client's own selector can only narrow the result. Several
// labelSelector parameters are folded into one, so none is ignored upstream.
func requireLabels(query url.Values, labels map[string]string) {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	terms := make([]string, 0, len(keys)+len(query["labelSelector"]))
	for _, k := range keys {
		terms = append(terms, k+"="+labels[k])
	}
	for _, v := range query["labelSelector"] {
		if v = strings.TrimSpace(v); v != "" {
			terms = append(terms, v)
		}
	}
	query.Set("labelSelector", strings.Join(terms, ","))
}

// hasLabels reports whether a JSON object carries all of labels. A body that
// does not parse fails the check, so an unexpected response is hidden rather
// than served.
func hasLabels(body []byte, labels map[string]string) bool {
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	for k, v := range labels {
		if got, ok := obj.Metadata.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// resolve returns a cache entry, fetching upstream if needed, along with the
// cache outcome. A nil entry means upstream failed and nothing usable was held.
func (h *Handler) resolve(ctx context.Context, key, path string, query url.Values, ttl time.Duration,
	gate *labelGate) (*cache.Entry, string) {
	now := time.Now()

	if entry, ok := h.cache.Get(key); ok {
		if entry.Fresh(now) {
			return entry, "hit"
		}
		if entry.Usable(now) {
			// Serve the stale copy and refresh behind it, so an expiring entry
			// does not release a stampede at the API server.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := h.fetch(ctx, key, path, query, ttl, gate); err != nil {
					h.log.Warn("background refresh failed", "path", path, "error", err)
				}
			}()
			return entry, "stale"
		}
	}

	entry, err := h.fetch(ctx, key, path, query, ttl, gate)
	if err == nil {
		return entry, "miss"
	}

	// Upstream failed. Prefer stale data over propagating the failure.
	if entry, ok := h.cache.Get(key); ok {
		h.log.Warn("serving stale response; upstream failed", "path", path, "error", err)
		return entry, "stale-error"
	}
	return nil, "bypass"
}

// fetch reads upstream and caches the result. A non-nil gate replaces an object
// without its labels with a 404 before caching, so the cache never holds it.
func (h *Handler) fetch(ctx context.Context, key, path string, query url.Values, ttl time.Duration,
	gate *labelGate) (*cache.Entry, error) {
	resp, err := h.upstream.Get(ctx, key, path, query.Encode())
	if err != nil {
		return nil, err
	}

	entry := &cache.Entry{
		Status:          resp.Status,
		Body:            resp.Body,
		ContentType:     resp.ContentType,
		ETag:            resp.ETag,
		ResourceVersion: resp.ResourceVersion,
		StoredAt:        time.Now(),
	}

	if gate != nil && (resp.Status == http.StatusNotFound ||
		resp.Status >= 200 && resp.Status < 300 && !hasLabels(resp.Body, gate.labels)) {
		entry = &cache.Entry{
			Status:      http.StatusNotFound,
			Body:        gate.notFound,
			ContentType: "application/json",
			StoredAt:    entry.StoredAt,
		}
	}

	switch {
	case resp.Status >= 200 && resp.Status < 300:
		entry.FreshFor, entry.StaleFor = ttl, staleFor
	case resp.Status == http.StatusForbidden || resp.Status == http.StatusNotFound:
		entry.FreshFor = negativeTTL
	default:
		// Other upstream failures are passed through but never cached.
		return entry, nil
	}

	h.cache.Put(key, entry)
	return entry, nil
}

// write emits a cache entry as the response.
func (h *Handler) write(w http.ResponseWriter, r *http.Request, entry *cache.Entry,
	query url.Values, ttl time.Duration, status string) {

	hdr := w.Header()
	// Only these headers reach the client; upstream response headers are not
	// copied through, so audit IDs and the API server's own no-cache directives
	// do not leak or confuse the CDN.
	hdr.Set("Content-Type", entry.ContentType)
	hdr.Set("Cache-Control", cacheControl(entry, query, ttl))
	hdr.Set("Vary", "Accept-Encoding")
	hdr.Set("X-Kdn-Cache", status)
	hdr.Set("Age", strconv.Itoa(max(0, int(entry.Age(time.Now()).Seconds()))))
	if entry.ETag != "" {
		hdr.Set("ETag", entry.ETag)
	}
	if entry.ResourceVersion != "" {
		hdr.Set("X-Kdn-Resource-Version", entry.ResourceVersion)
	}

	// A matching validator means the client already holds this body. This is
	// what makes CDN revalidation cheap.
	if entry.ETag != "" && etagMatches(r.Header.Get("If-None-Match"), entry.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	hdr.Set("Content-Length", strconv.Itoa(len(entry.Body)))
	w.WriteHeader(entry.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(entry.Body)
	}
}

// cacheControl computes the header the CDN acts on.
func cacheControl(entry *cache.Entry, query url.Values, ttl time.Duration) string {
	switch {
	case entry.Status == http.StatusForbidden || entry.Status == http.StatusNotFound:
		return "public, max-age=" + secs(negativeTTL)
	case entry.Status < 200 || entry.Status >= 300:
		return "no-store"
	case pinned(query):
		return "public, max-age=" + secs(pinnedTTL) + ", immutable"
	}

	if query.Get("continue") != "" && ttl > paginatedTTL {
		ttl = paginatedTTL
	}
	return "public, max-age=" + secs(ttl) +
		", stale-while-revalidate=" + secs(staleFor) +
		", stale-if-error=" + secs(staleFor)
}

// pinned reports whether the caller named an exact resourceVersion, making the
// response immutable. resourceVersion=0 means "any version the server has
// handy", and NotOlderThan sets a floor rather than an exact state, so neither
// is pinned.
func pinned(query url.Values) bool {
	rv := strings.TrimSpace(query.Get("resourceVersion"))
	if rv == "" || rv == "0" {
		return false
	}
	return query.Get("resourceVersionMatch") != "NotOlderThan"
}

// cacheKey is the path plus its query in a stable order, so that parameter
// ordering alone does not fork the cache.
func cacheKey(path string, query url.Values) string {
	if len(query) == 0 {
		return path
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(path)
	for i, k := range keys {
		vals := append([]string(nil), query[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			if i == 0 {
				b.WriteByte('?')
			} else {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

// writeStatus renders a Kubernetes Status object, so client-go produces a typed
// error rather than failing to parse an unexpected body.
func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	body := statusBody(code, reason, message)

	w.Header().Set("Content-Type", "application/json")
	// Refusals are config-dependent, so neither the CDN nor a client should
	// hold on to them.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// statusBody is a Kubernetes Status object as JSON.
func statusBody(code int, reason, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    message,
		"reason":     reason,
		"code":       code,
	})
	return append(body, '\n')
}

// namespaceAllowed matches a namespace against exact names and trailing-"*"
// patterns.
func namespaceAllowed(patterns []string, ns string) bool {
	for _, p := range patterns {
		if p == "*" || p == ns {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(ns, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, c := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimPrefix(strings.TrimSpace(c), "W/") == etag {
			return true
		}
	}
	return false
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "t", "yes":
		return true
	}
	return false
}

func secs(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) }
