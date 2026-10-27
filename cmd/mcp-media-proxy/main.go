// Command mcp-media-proxy is the Wave-2 MCP proxy: it fronts an arbitrary
// upstream MCP server and enriches it with media egress capabilities.
//
//   - mirrors every upstream tool onto its own downstream MCP server
//     (default listen :5780) and forwards calls,
//   - exposes the generic stream_media/download_file tools backed by the
//     sidecar mint API,
//   - optionally enriches matching tool responses with signed URLs
//     (TOOL_MATCH).
//
// In standalone mode (PROXY_MODE=standalone, Wave 6) it serves ONLY the
// generic media tools — no upstream mirroring, no enrichment. This is the
// egress surface for agent pods.
//
// Configuration is entirely env-driven and the process refuses to start on
// invalid config (fail fast). Secrets are validated but never logged — only
// their lengths.
//
// Environment:
//
//	PROXY_MODE           "full" (default) mirrors the upstream MCP server
//	                     with enrichment; "standalone" serves only the
//	                     generic media tools. Omitting both PROXY_MODE and
//	                     UPSTREAM_MCP_URL auto-detects standalone.
//	UPSTREAM_MCP_URL     streamable HTTP endpoint of the upstream MCP server
//	                     (required in full mode, http(s) with host; must be
//	                     unset in standalone mode)
//	MINT_URL             base URL of the sidecar mint API (required, http(s))
//	MEDIA_INTERNAL_TOKEN bearer token for POST /mint (required, non-empty)
//	MEDIA_AUTH_TOKEN     bearer token enforced on downstream clients and
//	                     presented to the upstream MCP server (optional;
//	                     unset = no auth. When set, requests without a
//	                     matching "Authorization: Bearer" header are
//	                     rejected with 401, and every upstream request
//	                     carries the same header so auth-enforcing
//	                     upstreams (MCP_AUTH_TOKEN / EMAIL_MCP_HTTP_TOKEN
//	                     guards) accept the proxy session. Minimum 32
//	                     characters to match those upstream guards.)
//	TOOL_MATCH           regex selecting tool names for response enrichment;
//	                     empty disables enrichment (optional, full mode only)
//	PROXY_LISTEN_ADDR    downstream listen address (default ":5780")
//	INLINE_MAX_BYTES     inline payload cap for stream_media
//	                     (default 102400)
//	MEDIA_FETCH_BASE_URL internal base replacing scheme+host of minted URLs
//	                     for server-side inline fetches (optional; signatures
//	                     do not cover the host)
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/gthieleb/mcp-media/internal/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultListenAddr = ":5780"

	proxyName    = "mcp-media-proxy"
	proxyVersion = "0.2.0"

	// genericTools are owned by this process; upstream tools with these
	// names are never mirrored over them.
	genericStreamMedia = "stream_media"
	genericDownload    = "download_file"

	// readHeaderTimeout bounds Slowloris-style attacks on the listener.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout bounds graceful shutdown on SIGINT/SIGTERM.
	shutdownTimeout = 10 * time.Second
)

// config is the validated startup configuration of the proxy.
type config struct {
	Mode           proxy.Mode
	UpstreamURL    string // "" → no upstream (standalone)
	MintURL        string
	Token          string
	AuthToken      string // optional downstream+upstream bearer token; "" → no auth
	ToolMatch      string // "" → enrichment disabled
	ListenAddr     string
	InlineMaxBytes int64
	FetchBaseURL   string // "" → fetch minted URLs as-is
	// AutoDetected reports that standalone mode was chosen by absence of
	// UPSTREAM_MCP_URL rather than an explicit PROXY_MODE.
	AutoDetected bool
}

// loadConfig reads and validates the proxy configuration. getenv is the
// environment lookup (os.Getenv in production, a stub in tests). Any
// invalid value is a fatal startup error.
func loadConfig(getenv func(string) string) (config, error) {
	modeEnv := getenv("PROXY_MODE")
	upstreamEnv := getenv("UPSTREAM_MCP_URL")

	cfg := config{Mode: proxy.ModeFull}
	switch modeEnv {
	case "":
		if upstreamEnv == "" {
			// Auto-detect: full mode needs an upstream to mirror; with no
			// upstream configured the proxy must serve the generic tools
			// instead (agent-pod standalone wiring).
			cfg.Mode = proxy.ModeStandalone
			cfg.AutoDetected = true
		}
	case string(proxy.ModeFull), string(proxy.ModeStandalone):
		cfg.Mode = proxy.Mode(modeEnv)
	default:
		return config{}, fmt.Errorf(
			"PROXY_MODE %q is invalid: must be %q or %q", modeEnv, proxy.ModeFull, proxy.ModeStandalone)
	}

	if cfg.Mode == proxy.ModeStandalone {
		// A leftover UPSTREAM_MCP_URL must not silently re-enable
		// mirroring; TOOL_MATCH has nothing to enrich in standalone.
		if upstreamEnv != "" {
			return config{}, errors.New(
				"UPSTREAM_MCP_URL must be unset in standalone mode (remove it or switch PROXY_MODE to full)")
		}
		if getenv("TOOL_MATCH") != "" {
			return config{}, errors.New(
				"TOOL_MATCH must be unset in standalone mode (there are no mirrored tools to enrich)")
		}
	} else {
		var err error
		cfg.UpstreamURL, err = httpURLEnv(getenv, "UPSTREAM_MCP_URL", true)
		if err != nil {
			return config{}, err
		}
	}

	mintURL, err := httpURLEnv(getenv, "MINT_URL", true)
	if err != nil {
		return config{}, err
	}
	cfg.MintURL = mintURL

	cfg.Token = getenv("MEDIA_INTERNAL_TOKEN")
	if cfg.Token == "" {
		return config{}, errors.New("MEDIA_INTERNAL_TOKEN is required")
	}

	cfg.AuthToken = getenv("MEDIA_AUTH_TOKEN")
	if cfg.AuthToken != "" && len(cfg.AuthToken) < 32 {
		// Matches the MCP_AUTH_TOKEN / EMAIL_MCP_HTTP_TOKEN guards of the
		// upstream MCP servers: a too-short token would be refused there
		// anyway — fail fast at the proxy instead of at session connect.
		return config{}, errors.New("MEDIA_AUTH_TOKEN is too short (need at least 32 characters)")
	}

	if cfg.Mode == proxy.ModeFull {
		cfg.ToolMatch = getenv("TOOL_MATCH")
		if cfg.ToolMatch != "" {
			if _, err := regexp.Compile(cfg.ToolMatch); err != nil {
				return config{}, fmt.Errorf("TOOL_MATCH %q is not a valid regex", cfg.ToolMatch)
			}
		}
	}

	cfg.ListenAddr = getenv("PROXY_LISTEN_ADDR")
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}

	cfg.InlineMaxBytes = proxy.InlineMaxBytesDefault
	if v := getenv("INLINE_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("INLINE_MAX_BYTES %q must be a positive integer", v)
		}
		cfg.InlineMaxBytes = n
	}

	cfg.FetchBaseURL, err = httpURLEnv(getenv, "MEDIA_FETCH_BASE_URL", false)
	if err != nil {
		return config{}, err
	}

	return cfg, nil
}

// httpURLEnv reads an http(s) URL from key. When required is false an empty
// value is returned as-is. Errors never embed the raw value: URLs may carry
// credentials.
func httpURLEnv(getenv func(string) string, key string, required bool) (string, error) {
	v := getenv(key)
	if v == "" {
		if required {
			return "", fmt.Errorf("%s is required", key)
		}
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("%s is not a valid URL", key)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https, got scheme %q", key, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s has no host", key)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not contain userinfo (credentials are not allowed)", key)
	}
	return v, nil
}

// logValue describes the configuration for startup logs. The token is NEVER
// logged — only its length.
func (c config) logValue() slog.Value {
	return slog.GroupValue(
		slog.String("mode", string(c.Mode)),
		slog.Bool("mode_auto_detected", c.AutoDetected),
		slog.String("upstream_url", c.UpstreamURL),
		slog.String("mint_url", c.MintURL),
		slog.String("tool_match", c.ToolMatch),
		slog.String("listen_addr", c.ListenAddr),
		slog.Int64("inline_max_bytes", c.InlineMaxBytes),
		slog.String("fetch_base_url", c.FetchBaseURL),
		slog.Int("token_bytes", len(c.Token)),
		slog.Bool("auth_enabled", c.AuthToken != ""),
		slog.Int("auth_token_bytes", len(c.AuthToken)),
	)
}

// run builds the downstream MCP server (generic tools + optional enrichment
// middleware), connects the upstream session in full mode, serves via
// streamable HTTP and blocks until ctx is cancelled or serving fails. Both
// the listener and the upstream session are shut down gracefully.
func run(ctx context.Context, cfg config) error {
	logger := slog.Default()

	mc, err := proxy.NewMintClient(cfg.MintURL, cfg.Token, nil)
	if err != nil {
		return fmt.Errorf("mint client: %w", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: proxyName, Version: proxyVersion}, nil)

	proxy.RegisterMediaTools(server, mc, proxy.MediaToolOptions{
		FetchBaseURL:   cfg.FetchBaseURL,
		InlineMaxBytes: cfg.InlineMaxBytes,
		Logger:         logger,
	})

	wiring := proxy.WiringForMode(cfg.Mode)
	if wiring.Enrich && cfg.ToolMatch != "" {
		enricher, err := proxy.NewEnricher(mc, cfg.ToolMatch, logger)
		if err != nil {
			return fmt.Errorf("enricher: %w", err)
		}
		server.AddReceivingMiddleware(enricher.Middleware())
	}

	if wiring.MirrorUpstream {
		up := proxy.NewUpstream(cfg.UpstreamURL, &proxy.UpstreamOptions{
			Logger:        logger,
			ReservedTools: []string{genericStreamMedia, genericDownload},
			AuthToken:     cfg.AuthToken,
		})
		if err := up.Connect(ctx, server); err != nil {
			return fmt.Errorf("upstream: %w", err)
		}
		defer func() { _ = up.Close() }()
	}

	handler := http.Handler(
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil),
	)
	if cfg.AuthToken != "" {
		// Same guard model as the auth-enforcing upstream MCP servers: the
		// proxy owns the client-facing endpoint (the service targetPort is
		// mutated to this container), so this is where downstream auth is
		// enforced.
		handler = bearerAuth(cfg.AuthToken, handler)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "server", "downstream", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("downstream server: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested", "reason", ctx.Err())
	case err := <-errCh:
		logger.Error("server failed, shutting down", "err", err)
		runErr = err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("downstream server shutdown: %w", err))
	}
	return runErr
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	slog.Info("starting mcp-media-proxy", "config", cfg.logValue())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("mcp-media-proxy terminated with error", "err", err)
		os.Exit(1)
	}
}

// bearerAuth rejects requests that don't carry "Authorization: Bearer <token>".
// Constant-time comparison; the challenge advertises the scheme so MCP
// clients (opencode etc.) surface an authentication prompt instead of a bare
// 401. Mirrors the guard of the wa-mcp/email-mcp SDK v2 http servers.
func bearerAuth(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
