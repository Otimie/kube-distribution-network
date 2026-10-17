// Package servertls builds the TLS configuration for the public listener: the
// hop between the CDN and this origin.
//
// Without it, that hop is plain HTTP, and anyone on the path can read or alter
// responses the CDN will then cache and serve to the whole fleet. With a client
// CA it is also mutual TLS, so only the CDN can connect at all and the edge's
// protections (the Worker, rate limiting) cannot be bypassed by going to the
// origin directly.
package servertls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/example/kube-distribution-network/internal/config"
)

// reloadInterval is how often the certificate files are checked for changes.
// Certificates are rotated in place (by cert-manager, or a Secret update), so a
// pair read once at startup eventually expires under a running process.
const reloadInterval = 30 * time.Second

// New returns the server TLS config for cfg, or nil if TLS is not enabled. It
// loads every file up front, so a bad path or a mismatched pair fails at
// startup rather than on the first handshake.
func New(cfg config.TLS) (*tls.Config, error) {
	if !cfg.Enabled() {
		return nil, nil
	}

	r := &reloader{certFile: cfg.CertFile, keyFile: cfg.KeyFile}
	if err := r.load(); err != nil {
		return nil, err
	}

	tc := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.getCertificate,
	}

	if cfg.ClientCAFile != "" {
		pem, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls.clientCAFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.clientCAFile %q contains no PEM certificates", cfg.ClientCAFile)
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tc, nil
}

// reloader serves the current certificate, re-reading the pair when either
// file changes.
type reloader struct {
	certFile, keyFile string

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
	checked time.Time
}

func (r *reloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	cert, checked := r.cert, r.checked
	r.mu.RUnlock()
	if time.Since(checked) < reloadInterval {
		return cert, nil
	}

	// Don't stop serving a certificate that still works because a reload
	// failed: a rotation that writes the cert and key separately can be caught
	// half-done, and the next check will see the finished pair.
	_ = r.load()

	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// load re-reads the pair if either file has changed since the last load.
func (r *reloader) load() error {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return r.markChecked(fmt.Errorf("read tls.certFile: %w", err))
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return r.markChecked(fmt.Errorf("read tls.keyFile: %w", err))
	}

	r.mu.RLock()
	unchanged := r.cert != nil && certInfo.ModTime().Equal(r.certMod) && keyInfo.ModTime().Equal(r.keyMod)
	r.mu.RUnlock()
	if unchanged {
		return r.markChecked(nil)
	}

	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return r.markChecked(fmt.Errorf("load tls.certFile and tls.keyFile: %w", err))
	}

	r.mu.Lock()
	r.cert, r.certMod, r.keyMod, r.checked = &cert, certInfo.ModTime(), keyInfo.ModTime(), time.Now()
	r.mu.Unlock()
	return nil
}

// markChecked records that a check happened, so a persistent failure is retried
// once per interval rather than on every handshake, and passes err through.
func (r *reloader) markChecked(err error) error {
	r.mu.Lock()
	r.checked = time.Now()
	r.mu.Unlock()
	return err
}
