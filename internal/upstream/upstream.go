// Package upstream talks to the Kubernetes API server.
//
// Two things here are the reason the proxy protects the API server at all, and
// neither can be done by a CDN:
//
//   - Request coalescing: simultaneous identical cache misses become a single
//     upstream request. A cold-cache stampede of 10,000 requests produces one
//     API server call.
//   - A concurrency ceiling: however much traffic arrives, only a bounded
//     number of requests are in flight upstream.
package upstream

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// maxResponseBytes bounds how much the proxy will buffer, so an unexpectedly
	// enormous list cannot exhaust memory.
	maxResponseBytes = 256 << 20

	// tokenReloadInterval is how often the service account token is re-read.
	// Projected tokens are short-lived and rewritten in place by the kubelet, so
	// one read at startup stops working within the hour.
	tokenReloadInterval = 30 * time.Second

	// shedTimeout is how long a request waits for an upstream slot before being
	// refused. Shedding fast is deliberate: unbounded queueing turns an overload
	// into a latency collapse for every client at once.
	shedTimeout = 2 * time.Second
)

// ErrOverloaded means no upstream slot became available in time.
var ErrOverloaded = errors.New("upstream concurrency limit reached")

// Response is a buffered upstream response.
type Response struct {
	Status          int
	Body            []byte
	ContentType     string
	ResourceVersion string
	ETag            string
}

// Client reads the Kubernetes API with the proxy's own service account. Client
// credentials are never forwarded, so the API server only ever sees this
// identity.
type Client struct {
	base    *url.URL
	httpc   *http.Client
	timeout time.Duration

	tokenPath string
	tokenMu   sync.RWMutex
	token     string
	tokenRead time.Time

	sem   chan struct{}
	group singleflight.Group

	coalesced atomic.Uint64
	shed      atomic.Uint64
}

// New builds a Client.
func New(server, caFile, tokenFile string, insecure bool, timeout time.Duration, concurrency int) (*Client, error) {
	base, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("parse upstream.server %q: %w", server, err)
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, fmt.Errorf("upstream.server must be an http(s) URL, got %q", server)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case insecure:
		tlsCfg.InsecureSkipVerify = true
	case caFile != "":
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read upstream.caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("upstream.caFile %q has no usable certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	}

	c := &Client{
		base:    base,
		timeout: timeout,
		httpc: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
				// Keep the connection pool at least as large as the concurrency
				// ceiling so permitted requests are not serialised behind it.
				MaxIdleConns:        concurrency * 2,
				MaxIdleConnsPerHost: concurrency * 2,
				MaxConnsPerHost:     concurrency * 2,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
			// Following a redirect would send the proxy's credentials to a host
			// the config never named.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("upstream redirect refused")
			},
		},
		tokenPath: tokenFile,
		sem:       make(chan struct{}, concurrency),
	}

	if c.tokenPath != "" {
		if _, err := c.currentToken(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// currentToken returns the service account token, re-reading it when the cached
// copy goes stale.
func (c *Client) currentToken() (string, error) {
	c.tokenMu.RLock()
	tok, read := c.token, c.tokenRead
	c.tokenMu.RUnlock()
	if tok != "" && time.Since(read) < tokenReloadInterval {
		return tok, nil
	}

	raw, err := os.ReadFile(c.tokenPath)
	if err != nil {
		// Don't discard a token that still works because a reload failed.
		if tok != "" {
			return tok, nil
		}
		return "", fmt.Errorf("read upstream.tokenFile: %w", err)
	}
	tok = strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("upstream.tokenFile %q is empty", c.tokenPath)
	}

	c.tokenMu.Lock()
	c.token, c.tokenRead = tok, time.Now()
	c.tokenMu.Unlock()
	return tok, nil
}

// Get reads path?rawQuery, coalescing concurrent requests that share key.
func (c *Client) Get(ctx context.Context, key, path, rawQuery string) (*Response, error) {
	v, err, shared := c.group.Do(key, func() (any, error) {
		return c.do(ctx, path, rawQuery)
	})
	if shared {
		c.coalesced.Add(1)
	}
	if err != nil {
		return nil, err
	}
	return v.(*Response), nil
}

func (c *Client) do(ctx context.Context, path, rawQuery string) (*Response, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.sem }()

	target := *c.base
	target.Path = path
	target.RawQuery = rawQuery

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kube-distribution-network")
	// Accept-Encoding is deliberately unset: net/http adds gzip itself and
	// transparently decompresses only when it owns that header. Setting it by
	// hand would hand us a still-compressed body.

	if c.tokenPath != "" {
		tok, err := c.currentToken()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	sum := sha256.Sum256(body)
	return &Response{
		Status:          resp.StatusCode,
		Body:            body,
		ContentType:     contentType,
		ResourceVersion: resourceVersion(contentType, body),
		ETag:            `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`,
	}, nil
}

// acquire waits for an upstream slot.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	default:
	}

	timer := time.NewTimer(shedTimeout)
	defer timer.Stop()
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-timer.C:
		c.shed.Add(1)
		return ErrOverloaded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resourceVersion pulls metadata.resourceVersion out of a JSON body.
func resourceVersion(contentType string, body []byte) string {
	if !strings.Contains(contentType, "json") {
		return ""
	}
	var m struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return m.Metadata.ResourceVersion
}

// Stats reports upstream counters.
type Stats struct {
	Coalesced uint64 `json:"coalesced"`
	Shed      uint64 `json:"shed"`
	InFlight  int    `json:"inFlight"`
}

// Stats returns a snapshot.
func (c *Client) Stats() Stats {
	return Stats{
		Coalesced: c.coalesced.Load(),
		Shed:      c.shed.Load(),
		InFlight:  len(c.sem),
	}
}
