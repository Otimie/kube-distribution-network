// Command kdn is a read-only caching proxy that puts the Kubernetes API behind
// a CDN.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/kube-distribution-network/internal/cache"
	"github.com/example/kube-distribution-network/internal/config"
	"github.com/example/kube-distribution-network/internal/proxy"
	"github.com/example/kube-distribution-network/internal/servertls"
	"github.com/example/kube-distribution-network/internal/upstream"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kdn: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/kdn/config.yaml", "path to the configuration file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	up, err := upstream.New(
		cfg.Upstream.Server, cfg.Upstream.CAFile, cfg.Upstream.TokenFile,
		cfg.Upstream.InsecureSkipTLSVerify, cfg.Upstream.Timeout.D(), cfg.UpstreamConcurrency,
	)
	if err != nil {
		return err
	}

	tlsConfig, err := servertls.New(cfg.TLS)
	if err != nil {
		return err
	}

	responseCache := cache.New(cfg.CacheMaxBytes)

	public := &http.Server{
		Addr:      cfg.Listen,
		Handler:   proxy.New(cfg, responseCache, up, log),
		TLSConfig: tlsConfig,
		// Includes TLS handshake failures, e.g. a client without a certificate
		// when mTLS is on; keep them in the structured log.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// ReadHeaderTimeout is what makes a slowloris attack expensive.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	// Health and stats live on a separate port so they never collide with
	// proxied API paths and are not exposed through the CDN.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"cache":    responseCache.Stats(),
			"upstream": up.Stats(),
		})
	})
	admin := &http.Server{
		Addr:              cfg.AdminListen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("starting", "version", version, "listen", cfg.Listen,
		"adminListen", cfg.AdminListen, "upstream", cfg.Upstream.Server, "rules", len(cfg.Rules),
		"tls", cfg.TLS.Enabled(), "mtls", cfg.TLS.ClientCAFile != "")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	serve := func(name string, s *http.Server) {
		var err error
		if s.TLSConfig != nil {
			// The certificate comes from TLSConfig.GetCertificate, so no files here.
			err = s.ListenAndServeTLS("", "")
		} else {
			err = s.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s listener: %w", name, err)
		}
	}
	go serve("public", public)
	go serve("admin", admin)

	select {
	case err := <-errCh:
		shutdown(public, admin)
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdown(public, admin)
		return nil
	}
}

// shutdown drains both listeners, giving in-flight requests a chance to finish.
func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
}
