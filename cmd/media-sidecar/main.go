// Command media-sidecar runs the mcp-media sidecar: two HTTP listeners in
// one process.
//
//   - public file server (default :8090, internal/serve): serves files from
//     the media roots via signed, expiring URLs.
//   - internal mint API (default :8091, internal/mint): issues those URLs to
//     trusted callers (the Wave-2 proxy) authenticated by a bearer token.
//
// Configuration is entirely env-driven and the process refuses to start on
// invalid config (fail fast, startup validation per the plan's security
// section). Secrets are validated but never logged — only their lengths.
//
// Environment:
//
//	MEDIA_SIGNING_SECRET   HMAC key for URL signing (required, >= 32 bytes)
//	MEDIA_INTERNAL_TOKEN   bearer token for POST /mint (required, non-empty)
//	MEDIA_PUBLIC_BASE_URL  externally reachable base of the file server,
//	                       http(s) with host (required)
//	MEDIA_ROOTS            comma-separated absolute media roots (default "/data")
//	MEDIA_SERVE_ADDR       listen address of the file server (default ":8090")
//	MEDIA_MINT_ADDR        listen address of the mint API (default ":8091")
//	MEDIA_DEFAULT_TTL      default URL TTL, Go duration (default "300s")
//	MEDIA_MAX_TTL          maximum URL TTL, Go duration (default "900s")
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gthieleb/mcp-media/internal/mint"
	"github.com/gthieleb/mcp-media/internal/serve"
)

const (
	defaultServeAddr  = ":8090"
	defaultMintAddr   = ":8091"
	defaultRoots      = "/data"
	defaultDefaultTTL = 300 * time.Second
	defaultMaxTTL     = 900 * time.Second

	// minSecretBytes is the minimum HMAC key length (plan security section).
	minSecretBytes = 32

	// readHeaderTimeout bounds Slowloris-style attacks on both listeners.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout bounds graceful shutdown on SIGINT/SIGTERM.
	shutdownTimeout = 10 * time.Second
)

// config is the validated startup configuration of the sidecar.
type config struct {
	Secret        []byte
	Token         string
	PublicBaseURL string
	Roots         []string
	ServeAddr     string
	MintAddr      string
	DefaultTTL    time.Duration
	MaxTTL        time.Duration
}

// loadConfig reads and validates the sidecar configuration. getenv is the
// environment lookup (os.Getenv in production, a stub in tests). Any
// invalid value is a fatal startup error.
func loadConfig(getenv func(string) string) (config, error) {
	var cfg config

	secret := getenv("MEDIA_SIGNING_SECRET")
	if secret == "" {
		return config{}, errors.New("MEDIA_SIGNING_SECRET is required")
	}
	if len(secret) < minSecretBytes {
		return config{}, fmt.Errorf("MEDIA_SIGNING_SECRET must be at least %d bytes, got %d", minSecretBytes, len(secret))
	}
	cfg.Secret = []byte(secret)

	cfg.Token = getenv("MEDIA_INTERNAL_TOKEN")
	if cfg.Token == "" {
		return config{}, errors.New("MEDIA_INTERNAL_TOKEN is required")
	}

	cfg.PublicBaseURL = getenv("MEDIA_PUBLIC_BASE_URL")
	if cfg.PublicBaseURL == "" {
		return config{}, errors.New("MEDIA_PUBLIC_BASE_URL is required")
	}
	// Never echo the raw value into errors: it may carry credentials.
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil {
		// url.Parse errors embed the raw URL — do not wrap them.
		return config{}, errors.New("MEDIA_PUBLIC_BASE_URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return config{}, fmt.Errorf("MEDIA_PUBLIC_BASE_URL must use http or https, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return config{}, errors.New("MEDIA_PUBLIC_BASE_URL has no host")
	}
	if u.User != nil {
		return config{}, errors.New("MEDIA_PUBLIC_BASE_URL must not contain userinfo (credentials are not allowed)")
	}

	roots := getenv("MEDIA_ROOTS")
	if roots == "" {
		roots = defaultRoots
	}
	for _, root := range strings.Split(roots, ",") {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return config{}, fmt.Errorf("MEDIA_ROOTS entry %q is not an absolute path", root)
		}
		cfg.Roots = append(cfg.Roots, root)
	}
	if len(cfg.Roots) == 0 {
		return config{}, errors.New("MEDIA_ROOTS must name at least one absolute directory")
	}

	cfg.ServeAddr = getenv("MEDIA_SERVE_ADDR")
	if cfg.ServeAddr == "" {
		cfg.ServeAddr = defaultServeAddr
	}
	cfg.MintAddr = getenv("MEDIA_MINT_ADDR")
	if cfg.MintAddr == "" {
		cfg.MintAddr = defaultMintAddr
	}

	cfg.DefaultTTL, err = durationEnv(getenv, "MEDIA_DEFAULT_TTL", defaultDefaultTTL)
	if err != nil {
		return config{}, err
	}
	cfg.MaxTTL, err = durationEnv(getenv, "MEDIA_MAX_TTL", defaultMaxTTL)
	if err != nil {
		return config{}, err
	}
	if cfg.DefaultTTL <= 0 {
		return config{}, fmt.Errorf("MEDIA_DEFAULT_TTL must be positive, got %s", cfg.DefaultTTL)
	}
	if cfg.MaxTTL < cfg.DefaultTTL {
		return config{}, fmt.Errorf("MEDIA_MAX_TTL %s must be >= MEDIA_DEFAULT_TTL %s", cfg.MaxTTL, cfg.DefaultTTL)
	}

	return cfg, nil
}

// durationEnv reads a Go duration from key, falling back to def when unset.
func durationEnv(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid duration: %w", key, v, err)
	}
	return d, nil
}

// logValue describes the configuration for startup logs. Secret and token
// values are NEVER logged — only their lengths.
func (c config) logValue() slog.Value {
	return slog.GroupValue(
		slog.String("public_base_url", c.PublicBaseURL),
		slog.Any("roots", c.Roots),
		slog.String("serve_addr", c.ServeAddr),
		slog.String("mint_addr", c.MintAddr),
		slog.String("default_ttl", c.DefaultTTL.String()),
		slog.String("max_ttl", c.MaxTTL.String()),
		slog.Int("secret_bytes", len(c.Secret)),
		slog.Int("token_bytes", len(c.Token)),
	)
}

// run starts both HTTP servers and blocks until ctx is cancelled or a
// server fails, then shuts both down gracefully. A server failure is
// returned as an error so main exits non-zero.
func run(ctx context.Context, cfg config) error {
	// Fail fast on missing or non-directory media roots.
	for _, root := range cfg.Roots {
		fi, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("media root %q: %w", root, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("media root %q is not a directory", root)
		}
	}

	mintHandler, err := mint.NewHandler(mint.Config{
		Secret:        cfg.Secret,
		Token:         cfg.Token,
		PublicBaseURL: cfg.PublicBaseURL,
		Roots:         cfg.Roots,
		DefaultTTL:    cfg.DefaultTTL,
		MaxTTL:        cfg.MaxTTL,
	})
	if err != nil {
		return fmt.Errorf("mint handler: %w", err)
	}

	servers := map[string]*http.Server{
		"serve": {Addr: cfg.ServeAddr, Handler: serve.NewHandler(cfg.Secret, cfg.Roots), ReadHeaderTimeout: readHeaderTimeout},
		"mint":  {Addr: cfg.MintAddr, Handler: mintHandler, ReadHeaderTimeout: readHeaderTimeout},
	}

	errCh := make(chan error, len(servers))
	for name, srv := range servers {
		go func() {
			slog.Info("listening", "server", name, "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server: %w", name, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutdown requested", "reason", ctx.Err())
	case err := <-errCh:
		// A server died: shut the other one down as well and report.
		slog.Error("server failed, shutting down", "err", err)
		runErr = err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var shutdownErr error
	for name, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("%s server shutdown: %w", name, err))
		}
	}
	return errors.Join(runErr, shutdownErr)
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	slog.Info("starting media-sidecar", "config", cfg.logValue())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("media-sidecar terminated with error", "err", err)
		os.Exit(1)
	}
}
