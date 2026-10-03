/**
 * Edge normalizer for the Kubernetes Distribution Network.
 *
 * Deploy this. It is the layer that stops cache-busting: without it, a caller
 * appending junk query parameters ("?x=1", "?x=2", ...) mints a unique cache key
 * per request, misses the edge every time, and reaches your origin on every
 * request -- turning the CDN into a request amplifier rather than a shield.
 *
 * Cloudflare's built-in alternative, a custom cache key via Cache Rules, is an
 * Enterprise-only feature. This Worker does the same job on any plan.
 *
 * It drops unknown query parameters, sorts the rest, refuses writes and
 * streaming reads, and serves from the edge cache using the origin's own
 * Cache-Control.
 *
 * Deploy with: wrangler deploy
 */

/** Query parameters allowed to influence the response. Everything else is
 *  dropped before the cache key is computed. */
const ALLOWED_PARAMS = new Set([
  "labelSelector",
  "fieldSelector",
  "resourceVersion",
  "resourceVersionMatch",
  "limit",
  "continue",
]);

/** The readable API surface. Everything else -- including the API server's
 *  /healthz, /metrics, /logs and /debug endpoints -- is refused. */
const ALLOWED_PREFIXES = ["/api", "/apis", "/version", "/openapi"];

const TRUTHY = new Set(["1", "true", "t", "yes"]);

/** Renders a Kubernetes Status object, so client-go produces a typed error
 *  rather than failing to parse an unexpected body. */
function status(code, reason, message) {
  return new Response(
    JSON.stringify({
      kind: "Status",
      apiVersion: "v1",
      metadata: {},
      status: "Failure",
      message,
      reason,
      code,
    }) + "\n",
    {
      status: code,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    },
  );
}

export default {
  async fetch(request, _env, ctx) {
    const url = new URL(request.url);

    if (request.method !== "GET" && request.method !== "HEAD") {
      return status(405, "MethodNotAllowed", `this endpoint is read-only; ${request.method} is not supported`);
    }

    const onSurface = ALLOWED_PREFIXES.some(
      (p) => url.pathname === p || url.pathname.startsWith(p + "/"),
    );
    if (!onSurface) {
      return status(404, "NotFound", `the path ${url.pathname} is not served here`);
    }

    // A watch is an open-ended stream no cache can serve.
    if (TRUTHY.has((url.searchParams.get("watch") || "").trim().toLowerCase())) {
      return status(
        501,
        "NotImplemented",
        "watch is not supported: a long-lived stream cannot be served from a cache. " +
          "Use repeated list requests, which are cached, instead.",
      );
    }

    // The anti-cache-busting step: drop unknown parameters and sort the rest, so
    // "?limit=500&junk=1" and "?junk=2&limit=500" share one cache entry.
    const canonical = new URL(url);
    const kept = [...canonical.searchParams.entries()]
      .filter(([k]) => ALLOWED_PARAMS.has(k))
      .sort(([a, av], [b, bv]) => (a === b ? av.localeCompare(bv) : a.localeCompare(b)));
    canonical.search = "";
    for (const [k, v] of kept) canonical.searchParams.append(k, v);

    const wantsBody = request.method !== "HEAD";

    // Always key on a GET, so a HEAD shares the entry its GET would produce.
    const cacheKey = new Request(canonical.toString(), { method: "GET" });
    const cache = caches.default;

    let cached = await cache.match(cacheKey);
    if (cached) {
      const hit = new Response(wantsBody ? cached.body : null, cached);
      hit.headers.set("x-kdn-edge", "hit");
      return hit;
    }

    // The origin ignores client credentials anyway; not forwarding them avoids
    // logging someone's token.
    const headers = new Headers(request.headers);
    for (const h of ["authorization", "cookie"]) headers.delete(h);

    const originResponse = await fetch(canonical.toString(), {
      method: "GET",
      headers,
      redirect: "manual",
    });

    const response = new Response(originResponse.body, originResponse);
    response.headers.set("x-kdn-edge", "miss");

    // Store per the origin's own Cache-Control: it marks refusals no-store and
    // marks reads pinned to an exact resourceVersion immutable.
    const cc = response.headers.get("cache-control") || "";
    if (originResponse.ok && !cc.includes("no-store") && !cc.includes("private")) {
      ctx.waitUntil(cache.put(cacheKey, response.clone()));
    }

    return wantsBody ? response : new Response(null, response);
  },
};
