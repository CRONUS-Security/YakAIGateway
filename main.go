// YakAIGateway is a personal reverse proxy in front of an OpenAI-compatible
// upstream (aibalance.yaklang.com). It forwards /v1/models,
// /v1/chat/completions, /v1/responses, /v1/messages and /v1/embeddings, owns
// the memfit TOTP auth header and the client fingerprint, rewrites agent
// system-prompt signatures the upstream blacklists, and streams responses
// (SSE) through with per-chunk flushing.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"
)

type Config struct {
	Listen       string
	Upstream     string
	APIKey       string
	UserAgent    string
	UserID       string
	YakVersion   string
	YakBuildTime string
	UsageType    string
	MaxBody      int64
	Verbose      bool
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8787", "listen address")
	flag.StringVar(&cfg.Upstream, "upstream", "https://aibalance.yaklang.com", "upstream base URL")
	flag.StringVar(&cfg.APIKey, "api-key", "free-user", "bearer token forced on every upstream request")
	flag.StringVar(&cfg.UserAgent, "user-agent", "", "User-Agent forced on every upstream request (empty sends none, matching the yaklang chat client)")
	flag.StringVar(&cfg.UserID, "user-id", "", "X-User-Id forced on every upstream request (empty: persistent UUID in the user config dir)")
	flag.StringVar(&cfg.YakVersion, "yak-version", "1.4.9-beta6", "X-Yak-Version forced on every upstream request")
	flag.StringVar(&cfg.YakBuildTime, "yak-build-time", "Fri Nov  6 18:35:24 2026 +0800", "X-Yak-Build-Time forced on every upstream request (1.4.9-beta6 release build)")
	flag.StringVar(&cfg.UsageType, "usage-type", "lightweight", "X-Yak-AI-Model-Usage-Type injected when the client sends none (empty omits)")
	flag.Int64Var(&cfg.MaxBody, "max-body", 32<<20, "maximum request body size in bytes")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "debug logging (includes TOTP seed)")
	flag.Parse()

	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if cfg.UserID == "" {
		cfg.UserID = loadOrCreateClientID(log)
	}

	// One pooled transport for all upstream traffic. ResponseHeaderTimeout is
	// deliberately 0: LLM time-to-first-token can exceed a minute. HTTP/2 is
	// disabled: the yaklang client speaks HTTP/1.1, and matching the protocol
	// keeps the fingerprint consistent.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Transport: transport}

	totp := NewTOTPManager(cfg.Upstream, client, cfg.UserAgent, cfg.UserID, log)
	proxy := NewProxy(cfg, client, totp, log)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxy.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout/WriteTimeout stay 0 so long SSE streams are never killed.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("yakaigateway listening", "addr", cfg.Listen, "upstream", cfg.Upstream, "user-id", cfg.UserID)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("graceful shutdown failed", "err", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}
}
