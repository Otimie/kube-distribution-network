// Package config defines the proxy's configuration file format.
package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can carry values like "30s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the whole configuration.
type Config struct {
	// Listen serves proxied API traffic; point the CDN at it.
	Listen string `yaml:"listen"`

	// AdminListen serves /healthz and /stats, on a separate port so they never
	// collide with proxied API paths.
	AdminListen string `yaml:"adminListen"`

	Upstream Upstream `yaml:"upstream"`

	// TLS secures the Listen port, the hop between the CDN and this origin.
	// Optional: with no certificate configured, Listen serves plain HTTP. The
	// admin port is never TLS; it is for in-cluster probes only.
	TLS TLS `yaml:"tls"`

	// CORS lets browser pages on other origins read responses. Off by default.
	CORS CORS `yaml:"cors"`

	// CacheMaxBytes is the response cache's total body budget. Entries are
	// evicted least-recently-used above it.
	CacheMaxBytes int64 `yaml:"cacheMaxBytes"`

	// UpstreamConcurrency caps simultaneous requests to the API server.
	// Together with request coalescing, this is what bounds API server load
	// however much traffic arrives at the edge.
	UpstreamConcurrency int `yaml:"upstreamConcurrency"`

	// Rules is the list of readable resources. It is deny-by-default: the proxy
	// reads the cluster with its own service account while edge clients are
	// anonymous, so anything listed here is readable by anyone who can reach
	// the proxy.
	Rules []Rule `yaml:"rules"`
}

// Upstream describes how to reach the Kubernetes API server.
type Upstream struct {
	Server    string `yaml:"server"`
	CAFile    string `yaml:"caFile"`
	TokenFile string `yaml:"tokenFile"`

	// InsecureSkipTLSVerify is for local development only.
	InsecureSkipTLSVerify bool `yaml:"insecureSkipTLSVerify"`

	Timeout Duration `yaml:"timeout"`
}

// TLS configures the public listener's TLS and, optionally, mutual TLS.
type TLS struct {
	// CertFile and KeyFile are the serving certificate and key, PEM-encoded.
	// Setting both turns on TLS. They are re-read when they change on disk, so
	// a rotated certificate (e.g. by cert-manager) is picked up without a
	// restart.
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`

	// ClientCAFile turns on mutual TLS: every connection must present a client
	// certificate signed by a CA in this PEM bundle, or the handshake fails.
	// Use it to accept traffic only from the CDN (on Cloudflare, Authenticated
	// Origin Pulls). Read once at startup.
	ClientCAFile string `yaml:"clientCAFile"`
}

// Enabled reports whether the public listener serves TLS.
func (t TLS) Enabled() bool { return t.CertFile != "" }

// CORS configures cross-origin reads from browsers.
type CORS struct {
	// AllowedOrigin is one origin like "https://dashboard.example.com", or "*"
	// for any. Empty turns CORS off. There is deliberately no list: allowing
	// several would make responses vary by Origin, which the CDN cache ignores.
	AllowedOrigin string `yaml:"allowedOrigin"`

	// AllowCredentials sends Access-Control-Allow-Credentials: true, which
	// browsers require before a page that fetches with credentials (cookies)
	// may read the response. The proxy ignores client credentials, so this
	// exposes nothing new. Needs a specific AllowedOrigin: browsers reject
	// credentials with "*".
	AllowCredentials bool `yaml:"allowCredentials"`
}

// Rule makes one resource type readable.
type Rule struct {
	// Group is the API group; "" is the core group.
	Group string `yaml:"group"`

	// Version is the API version, e.g. "v1"; "*" matches any.
	Version string `yaml:"version"`

	// Resource is the plural name, e.g. "pods".
	Resource string `yaml:"resource"`

	// Namespaces restricts reads to these namespaces. Entries are exact names
	// or a trailing-wildcard pattern like "team-*". Empty means all namespaces.
	//
	// Setting this also causes cluster-wide reads of the resource to be
	// refused, since those would return objects from every namespace.
	Namespaces []string `yaml:"namespaces"`

	// TTL becomes max-age: how long the CDN and clients may serve this
	// resource without revalidating. It is also the origin cache's freshness
	// window. This is the main knob — a 30s TTL means the API server sees at
	// most a couple of requests a minute for this resource, whatever the fleet
	// size.
	TTL Duration `yaml:"ttl"`
}

const (
	defaultCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// Default returns the operational defaults, with no rules.
func Default() *Config {
	return &Config{
		Listen:      ":8080",
		AdminListen: ":9090",
		Upstream: Upstream{
			Server:    "https://kubernetes.default.svc",
			CAFile:    defaultCAFile,
			TokenFile: defaultTokenFile,
			Timeout:   Duration(30 * time.Second),
		},
		CacheMaxBytes:       512 << 20,
		UpstreamConcurrency: 32,
	}
}

// Load reads and validates a YAML config, layered over Default.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Reject unknown keys, so a typo fails loudly instead of silently leaving a
	// setting at its default.
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate checks the config for consistency.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen must be set")
	}
	if c.Upstream.Server == "" {
		return fmt.Errorf("upstream.server must be set")
	}
	if c.UpstreamConcurrency < 1 {
		return fmt.Errorf("upstreamConcurrency must be at least 1")
	}
	if c.CacheMaxBytes < 1 {
		return fmt.Errorf("cacheMaxBytes must be positive")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls.certFile and tls.keyFile must be set together")
	}
	if c.TLS.ClientCAFile != "" && !c.TLS.Enabled() {
		return fmt.Errorf("tls.clientCAFile requires tls.certFile and tls.keyFile: mutual TLS needs TLS")
	}
	if o := c.CORS.AllowedOrigin; o != "" && o != "*" {
		// Browsers compare the header byte-for-byte with their own origin, so a
		// trailing slash or path would silently never match.
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			u.String() != o {
			return fmt.Errorf("cors.allowedOrigin: %q is not an origin like https://example.com", o)
		}
	}
	if c.CORS.AllowCredentials && (c.CORS.AllowedOrigin == "" || c.CORS.AllowedOrigin == "*") {
		return fmt.Errorf("cors.allowCredentials requires cors.allowedOrigin to be one origin: browsers reject credentials with \"*\"")
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("rules is empty: the proxy is deny-by-default and would serve nothing")
	}

	seen := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Group == "core" {
			r.Group = ""
		}
		if r.Resource == "" {
			return fmt.Errorf("rules[%d]: resource must be set", i)
		}
		if r.Version == "" {
			return fmt.Errorf("rules[%d] (%s): version must be set (use \"*\" for any)", i, r.Resource)
		}
		if r.TTL.D() <= 0 {
			return fmt.Errorf("rules[%d] (%s): ttl must be positive", i, r.Resource)
		}
		key := r.Group + "/" + r.Version + "/" + r.Resource
		if seen[key] {
			return fmt.Errorf("rules[%d]: duplicate rule for %s", i, key)
		}
		seen[key] = true
	}
	return nil
}
