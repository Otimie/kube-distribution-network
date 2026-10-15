# Kubernetes Distribution Network

A read-only caching proxy that puts the Kubernetes API behind a CDN, so a large
fleet of readers cannot overwhelm the API server.

When thousands of agents poll for the same data, the API server does the same
work thousands of times. This fixes that, at the cost of being read-only and
not supporting `watch`.

Clients use unmodified `kubectl` and `client-go` — it serves real API paths.

## How it protects the API server

Four layers, each absorbing what gets past the one before:

1. **CDN edge cache** — most requests never reach the origin.
2. **Origin response cache** — absorbs edge misses, which happen on every cold
   URL and from each CDN location independently.
3. **Request coalescing** — simultaneous identical misses become *one* upstream
   request. 10,000 concurrent requests for an uncached URL produce a single API
   server call.
4. **Upstream concurrency ceiling** — a hard cap on in-flight API server
   requests, shedding fast above it.

Plus `stale-while-revalidate`, so an expiring entry doesn't release a stampede,
and stale-on-error, so an API server outage doesn't immediately become a client
outage.

## What doesn't work

**`watch` is refused** with HTTP 501 and a Kubernetes `Status` body. A watch is
an open-ended stream no cache can serve, and accepting one would hold a
long-lived connection against the API server being protected.

That has a real cost — `client-go` informers LIST then WATCH:

| Works | Doesn't |
| --- | --- |
| `kubectl get`, `describe` | `kubectl get -w` |
| One-shot reads, polling clients | Controllers, operators, informers |

Also unsupported: writes, subresources (so no `kubectl logs`), `exec`, and
`port-forward`.

## Security model

The proxy reads the cluster with **its own service account**; edge clients are
**unauthenticated**. That trade is what makes CDN caching work — with no
per-caller identity, one cache entry serves everyone.

So the `rules` allowlist is the authorization boundary: anything listed is
readable by anyone who can reach the proxy. Don't list secrets.

It serves `GET`/`HEAD` only, is deny-by-default for resources, serves no
subresources, and never forwards client credentials.

Paths off the resource surface — `/metrics`, `/logs`, `/debug/pprof` — are
forwarded, and **the API server's RBAC decides them.** The ClusterRole in
`deploy/kubernetes.yaml` grants no `nonResourceURLs`, so those come back as a
403 the proxy caches. Check yours with:

```sh
kubectl auth can-i get /metrics --as=system:serviceaccount:kdn:kdn
```

Note that the default `system:discovery` and `system:public-info-viewer`
bindings grant `/healthz`, `/livez`, `/readyz`, `/version` and `/openapi/*` to
every authenticated account, so those stay readable however narrow your own
ClusterRole is. They return `ok` and a version string. The Worker also limits
which path prefixes reach the origin at all.

## Setup

```sh
make lint          # fmt, vet, test
make build
make push VERSION=v0.1.0   # builds and pushes to ECR
```

Alternatively, GitHub Actions builds a multi-arch (amd64/arm64) image on every
tag push and pushes it to GHCR:

```sh
git tag v0.1.0 && git push origin v0.1.0   # triggers the workflow
# ghcr.io/otimie/kube-distribution-network:v0.1.0 (and :latest)
# main branch builds are tagged with the short SHA and :main
```

Edit the image reference and the allowlist in `deploy/kubernetes.yaml` first,
then:

```sh
kubectl apply -f deploy/kubernetes.yaml
```

Then the CDN:

```sh
cd deploy/cloudflare && wrangler deploy
```

### CDN notes

**Deploy the Worker.** It drops unknown query parameters from the cache key.
Without it, `?x=1`, `?x=2`, … each mint a new cache key, miss the edge, and hit
your origin every time — the CDN becomes an amplifier instead of a shield. (The
built-in alternative, a custom cache key in Cache Rules, is Enterprise-only.)

Also worth setting, in the Cloudflare dashboard:

- **Cache Rules**: cache everything on the hostname, edge and browser TTL set to
  *respect origin*. The origin already computes the right lifetime per resource
  — including a one-year `immutable` for reads pinned to an exact
  `resourceVersion`. Overriding TTLs discards that.
- **Tiered Cache (smart)**: without it each Cloudflare location misses
  independently, so one cold URL can mean hundreds of simultaneous origin
  requests.
- **Rate limiting**: per-IP, e.g. 100 requests / 10s. This has to be at the
  edge — the origin only ever sees Cloudflare's addresses.

### Verify

```sh
curl -si https://k8s.example.com/api/v1/nodes | head -20   # 200, Cache-Control, X-Kdn-Cache
curl -si 'https://k8s.example.com/api/v1/nodes?junk=1'     # edge miss
curl -si 'https://k8s.example.com/api/v1/nodes?junk=2'     # should be an edge HIT
curl -si 'https://k8s.example.com/api/v1/nodes?watch=true' # 501
curl -si https://k8s.example.com/api/v1/secrets            # 403
```

If the third command shows a miss, the Worker isn't deployed and you're exposed
to cache-busting.

## Configuration

See `config.example.yaml` — it's loaded by the test suite, so it can't rot.

The main knob is per-rule `ttl`, which becomes `max-age`. A 30s `ttl` means the
API server sees at most a couple of requests a minute for that resource,
whatever the fleet size.

The best thing a *client* can do is pin an exact `resourceVersion`: that
response is one immutable state of the cluster, served `immutable` with a
one-year `max-age`, so it's cached permanently.

## Response headers

| Header | Meaning |
| --- | --- |
| `Cache-Control` | What the CDN acts on; `immutable` for a pinned read |
| `ETag` | Send back as `If-None-Match` for a cheap 304 |
| `X-Kdn-Cache` | `hit`, `miss`, `stale`, `stale-error` |
| `X-Kdn-Resource-Version` | The `resourceVersion` the API server reported |

`GET :9090/stats` gives cache and upstream counters as JSON. The one to watch is
`upstream.coalesced` — requests absorbed by coalescing.

## Layout

```
cmd/kdn/              entrypoint, both listeners, shutdown
internal/config/      config and the resource allowlist
internal/proxy/       path parsing, allowlist checks, cache headers
internal/cache/       size-bounded LRU with staleness windows
internal/upstream/    API client: coalescing, concurrency ceiling
deploy/kubernetes.yaml  manifests, RBAC mirroring the allowlist
deploy/cloudflare/      the Worker
```

Two dependencies: `gopkg.in/yaml.v3` and `golang.org/x/sync`.
