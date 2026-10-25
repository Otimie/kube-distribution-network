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

A rule with `requireLabels` narrows that to objects carrying those labels, which
hands the decision to whoever can label them. See
[Publishing by label](#publishing-by-label).

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

### TLS and mTLS between the CDN and the origin

Optional, and off by default. Without it the CDN-to-origin hop is plain HTTP:
anyone on that path can alter a response the CDN then caches and serves to the
whole fleet, and anyone who finds the origin's address can skip the Worker and
the edge rate limits entirely.

Configured under `tls` (see `config.example.yaml`):

- `certFile` + `keyFile` — serve TLS on `listen`. TLS 1.2 minimum. The pair is
  re-checked every 30s and reloaded when it changes, so cert-manager rotation
  needs no restart; a half-written rotation keeps the old pair in service.
- `clientCAFile` — additionally require a client certificate signed by this CA
  (mutual TLS). Only the CDN can then complete a handshake. Read at startup.

The admin port (`/healthz`, `/stats`) stays plain HTTP for kubelet probes. It
must not be exposed outside the cluster.

On Cloudflare:

1. **Origin certificate.** Issue one under *SSL/TLS → Origin Server* (or use
   any certificate valid for the origin hostname), put it in the `kdn-tls`
   Secret, and uncomment the `tls` blocks in `deploy/kubernetes.yaml`. Point the
   Service or load balancer at port 8080 as TLS.
2. **SSL/TLS mode: Full (strict).** Anything weaker means Cloudflare does not
   verify the origin certificate, which forfeits most of the point.
3. **Authenticated Origin Pulls** for mTLS. Prefer *per-hostname* pulls with a
   client certificate you issue yourself, and set `clientCAFile` to your CA.
   The zone-level option uses a certificate shared by every Cloudflare
   customer, so trusting its CA only proves the request came through
   *some* Cloudflare account — anyone can point their own zone at your origin
   and bypass your Worker and rate limits.

Check it from outside the CDN:

```sh
curl -sv https://origin.example.com:8080/version   # handshake fails: no client cert
curl -sv --cert client.crt --key client.key https://origin.example.com:8080/version  # 200
```

### CORS for browser clients

Off by default, and only needed for a browser page on another origin, such as a
dashboard. `kubectl` and `client-go` don't use CORS. Set one origin:

```yaml
cors:
  allowedOrigin: https://dashboard.example.com   # or "*"
```

The proxy then sends `Access-Control-Allow-Origin` on every response and answers
preflights itself; the Worker passes preflights through uncached. It allows only
`GET` and `HEAD`.

Some pages, such as Headlamp, fetch with `credentials: 'include'`, and browsers
won't let them read the response without `Access-Control-Allow-Credentials:
true`. Turn that on with `allowCredentials`, which needs a specific
`allowedOrigin`, since browsers reject credentials with `"*"`:

```yaml
cors:
  allowedOrigin: https://dashboard.example.com
  allowCredentials: true
```

This only lets the page read responses. The proxy still ignores cookies and
`Authorization`, and the Worker strips them, so the data is the same public
data an anonymous request gets.

Only one origin is supported. The headers have to be the same whatever the
request's `Origin`, because the edge cache keeps one copy per URL and ignores
`Vary: Origin`: a response tailored to one page would be served to another.
Cached responses keep their old CORS headers until they expire, so a change
takes up to the longest `ttl` to apply everywhere.

CORS doesn't widen what's readable. Anything in `rules` is already public to
anyone who can reach the proxy; it only lets pages on that origin read it too.

### Publishing by label

A rule can require labels, so only objects their owners have opted in are
served:

```yaml
rules:
  - group: ""
    version: v1
    resource: namespaces
    requireLabels:
      kdn.io/public: "true"
    ttl: "60s"
```

```sh
kubectl label namespace default kdn.io/public=true
```

- **Lists** get the labels added to their `labelSelector`, so the API server
  does the filtering and pagination and `remainingItemCount` describe only the
  published objects. A client's own selector is ANDed with them, so it can only
  narrow the result.
- **Reads by name** of an object without the labels get the same 404, byte for
  byte, as an object that doesn't exist, so names can't be probed.
- **Removing the label** hides the object once cached copies expire: up to the
  rule's `ttl`, or longer while stale copies cover an API server outage.

The labels are a further condition, never a replacement: an object must match a
rule *and* carry them, so labelling a Secret publishes nothing unless a rule
allows secrets.

Kubernetes RBAC can't restrict which labels someone sets, so **anyone who can
update an object can publish it**. Restrict the label with a
ValidatingAdmissionPolicy (GA in Kubernetes 1.30). For example, to let only the
`kdn-publishers` group set or change it:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: kdn-public-label
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["*"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE"]
        resources: ["*"]
  matchConditions:
    - name: not-a-publisher
      expression: "!('kdn-publishers' in request.userInfo.groups)"
  variables:
    - name: label
      expression: "'kdn.io/public'"
  validations:
    - expression: >-
        (has(object.metadata.labels) && variables.label in object.metadata.labels
          ? object.metadata.labels[variables.label] : '') ==
        (oldObject != null && has(oldObject.metadata.labels) && variables.label in oldObject.metadata.labels
          ? oldObject.metadata.labels[variables.label] : '')
      message: only kdn-publishers may set or change the kdn.io/public label
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: kdn-public-label
spec:
  policyName: kdn-public-label
  validationActions: [Deny]
```

Narrow `resourceRules` to the resources your rules cover, and try it on a test
cluster first: it applies to every matching write.

### Removing fields

A rule can strip fields from what it publishes. They're removed before caching,
so neither the cache nor the CDN ever holds them:

```yaml
rules:
  - group: ""
    version: v1
    resource: nodes
    ttl: "30s"
    remove:
      - status.images
      - status.nodeInfo
      - metadata.managedFields
      - metadata.annotations."kubectl.kubernetes.io/last-applied-configuration"
```

Paths start at the object; on a list, they apply to every item. Lists inside an
object take a selector:

| Path | Removes |
| --- | --- |
| `spec.containers[*].args` | `args` from every container |
| `spec.containers[name: *].env[name: *].value` | every env var's value, keeping its name |
| `spec.containers[name: app].env[name: DB_*]` | the `DB_*` env vars of container `app` entirely |

`[key: pattern]` matches items whose `key` is a string matching the pattern,
where `*` matches anything; items without the key don't match. `[*]` matches
every item. Quote keys that contain dots.

Removal **fails closed**. A field that isn't there is skipped, since there's
nothing to leak. But if a path finds the wrong kind of value, such as a list
where it expects an object, the rule no longer describes the data and the proxy
returns a 502 rather than serve it unredacted. Check the proxy's log for
`response could not be redacted`.

`kind`, `apiVersion`, `metadata`, `metadata.name` and `metadata.namespace` can't
be removed, since clients need them. `ETag` is computed from the served body,
so it doesn't reveal changes to removed fields. `requireLabels` is checked
before removal, so removing `metadata.labels` still works.

Changing `remove` doesn't touch copies the CDN already holds. Most expire
within the rule's `ttl`, but a read pinned to an exact `resourceVersion` is
cached for a year, so purge the CDN cache after adding a path that matters.

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
internal/redact/      field paths and removal for `remove`
internal/cache/       size-bounded LRU with staleness windows
internal/upstream/    API client: coalescing, concurrency ceiling
internal/servertls/   optional TLS / mTLS for the public listener
internal/cors/        optional CORS wrapper around the proxy handler
deploy/kubernetes.yaml  manifests, RBAC mirroring the allowlist
deploy/cloudflare/      the Worker
```

Two dependencies: `gopkg.in/yaml.v3` and `golang.org/x/sync`.
