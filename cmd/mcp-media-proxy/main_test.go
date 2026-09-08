package main

import (
	"strings"
	"testing"

	"github.com/gthieleb/mcp-media/internal/proxy"
)

func TestLoadConfigValid(t *testing.T) {
	cfg, err := loadConfig(mapEnv(t, map[string]string{
		"UPSTREAM_MCP_URL":     "http://localhost:7777",
		"MINT_URL":             "http://localhost:8091/",
		"MEDIA_INTERNAL_TOKEN": "tok",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.UpstreamURL != "http://localhost:7777" {
		t.Errorf("UpstreamURL = %q", cfg.UpstreamURL)
	}
	if cfg.MintURL != "http://localhost:8091/" {
		t.Errorf("MintURL = %q (normalization is the MintClient's job)", cfg.MintURL)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want default", cfg.ListenAddr)
	}
	if cfg.InlineMaxBytes != proxy.InlineMaxBytesDefault {
		t.Errorf("InlineMaxBytes = %d, want default", cfg.InlineMaxBytes)
	}
	if cfg.ToolMatch != "" || cfg.FetchBaseURL != "" {
		t.Error("optional values must default to empty")
	}
}

func TestLoadConfigFull(t *testing.T) {
	cfg, err := loadConfig(mapEnv(t, map[string]string{
		"PROXY_MODE":           "full",
		"UPSTREAM_MCP_URL":     "http://up:7777",
		"MINT_URL":             "http://mint:8091",
		"MEDIA_INTERNAL_TOKEN": "tok",
		"TOOL_MATCH":           `^save_`,
		"PROXY_LISTEN_ADDR":    ":9999",
		"INLINE_MAX_BYTES":     "2048",
		"MEDIA_FETCH_BASE_URL": "http://127.0.0.1:8090",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != proxy.ModeFull || cfg.AutoDetected {
		t.Errorf("mode = %q (auto=%v), want explicit full", cfg.Mode, cfg.AutoDetected)
	}
	if cfg.ToolMatch != "^save_" || cfg.ListenAddr != ":9999" || cfg.InlineMaxBytes != 2048 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.FetchBaseURL != "http://127.0.0.1:8090" {
		t.Errorf("FetchBaseURL = %q", cfg.FetchBaseURL)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	valid := map[string]string{
		"PROXY_MODE":           "full",
		"UPSTREAM_MCP_URL":     "http://up:7777",
		"MINT_URL":             "http://mint:8091",
		"MEDIA_INTERNAL_TOKEN": "tok",
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "missing upstream", env: drop(valid, "UPSTREAM_MCP_URL"), want: "UPSTREAM_MCP_URL"},
		{name: "missing mint", env: drop(valid, "MINT_URL"), want: "MINT_URL"},
		{name: "missing token", env: drop(valid, "MEDIA_INTERNAL_TOKEN"), want: "MEDIA_INTERNAL_TOKEN"},
		{
			name: "upstream bad scheme",
			env:  merge(valid, "UPSTREAM_MCP_URL", "ftp://up:7777"),
			want: "must use http or https",
		},
		{
			name: "mint no host",
			env:  merge(valid, "MINT_URL", "http://"),
			want: "has no host",
		},
		{
			name: "fetch base with userinfo",
			env:  merge(valid, "MEDIA_FETCH_BASE_URL", "http://user:pw@host:8090"),
			want: "userinfo",
		},
		{
			name: "bad tool match regex",
			env:  merge(valid, "TOOL_MATCH", "([bad"),
			want: "not a valid regex",
		},
		{
			name: "zero inline max bytes",
			env:  merge(valid, "INLINE_MAX_BYTES", "0"),
			want: "positive integer",
		},
		{
			name: "non-numeric inline max bytes",
			env:  merge(valid, "INLINE_MAX_BYTES", "big"),
			want: "positive integer",
		},
		{
			name: "fetch base bad scheme",
			env:  merge(valid, "MEDIA_FETCH_BASE_URL", "gopher://x"),
			want: "must use http or https",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(mapEnv(t, tc.env))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigStandaloneMode(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantMode proxy.Mode
		wantErr  string
	}{
		{
			name: "no mode with upstream implies full",
			env: map[string]string{
				"UPSTREAM_MCP_URL":     "http://up:7777",
				"MINT_URL":             "http://mint:8091",
				"MEDIA_INTERNAL_TOKEN": "tok",
			},
			wantMode: proxy.ModeFull,
		},
		{
			name: "no mode without upstream auto-detects standalone",
			env: map[string]string{
				"MINT_URL":             "http://mint:8091",
				"MEDIA_INTERNAL_TOKEN": "tok",
			},
			wantMode: proxy.ModeStandalone,
		},
		{
			name: "explicit full",
			env: map[string]string{
				"PROXY_MODE":           "full",
				"UPSTREAM_MCP_URL":     "http://up:7777",
				"MINT_URL":             "http://mint:8091",
				"MEDIA_INTERNAL_TOKEN": "tok",
			},
			wantMode: proxy.ModeFull,
		},
		{
			name: "explicit standalone without upstream",
			env: map[string]string{
				"PROXY_MODE":           "standalone",
				"MINT_URL":             "http://mint:8091",
				"MEDIA_INTERNAL_TOKEN": "tok",
			},
			wantMode: proxy.ModeStandalone,
		},
		{
			name: "invalid mode value",
			env: map[string]string{
				"PROXY_MODE":           "solo",
				"MINT_URL":             "http://mint:8091",
				"MEDIA_INTERNAL_TOKEN": "tok",
			},
			wantErr: "PROXY_MODE",
		},
		{
			name: "explicit full without upstream",
			env: map[string]string{
				"PROXY_MODE":           "full",
				"MINT_URL":             "http://mint:8091",
				"MEDIA_INTERNAL_TOKEN": "tok",
			},
			wantErr: "UPSTREAM_MCP_URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(mapEnv(t, tc.env))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.Mode != tc.wantMode {
				t.Errorf("Mode = %q, want %q", cfg.Mode, tc.wantMode)
			}
			if tc.wantMode == proxy.ModeStandalone && cfg.UpstreamURL != "" {
				t.Errorf("standalone must not set UpstreamURL, got %q", cfg.UpstreamURL)
			}
			if tc.wantMode == proxy.ModeStandalone && cfg.ToolMatch != "" {
				t.Errorf("standalone must not set ToolMatch, got %q", cfg.ToolMatch)
			}
		})
	}
}

// TestLoadConfigStandaloneRejectsUpstreamEnv guards the mode contract: a
// leftover UPSTREAM_MCP_URL must not silently switch the standalone proxy
// back into mirroring.
func TestLoadConfigStandaloneRejectsUpstreamEnv(t *testing.T) {
	_, err := loadConfig(mapEnv(t, map[string]string{
		"PROXY_MODE":           "standalone",
		"UPSTREAM_MCP_URL":     "http://up:7777",
		"MINT_URL":             "http://mint:8091",
		"MEDIA_INTERNAL_TOKEN": "tok",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "UPSTREAM_MCP_URL") {
		t.Errorf("error %q does not name UPSTREAM_MCP_URL", err)
	}
}

// TestLoadConfigAutoDetectStandalone asserts the documented fallback: full
// mode (the default) without UPSTREAM_MCP_URL auto-detects standalone.
func TestLoadConfigAutoDetectStandalone(t *testing.T) {
	cfg, err := loadConfig(mapEnv(t, map[string]string{
		"MINT_URL":             "http://mint:8091",
		"MEDIA_INTERNAL_TOKEN": "tok",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Mode != proxy.ModeStandalone {
		t.Errorf("Mode = %q, want standalone auto-detect", cfg.Mode)
	}
	if !cfg.AutoDetected {
		t.Error("AutoDetected = false, want true")
	}
}

// TestErrorMessagesNeverEmbedRawURLs guards the convention that config
// errors must not echo URL values (they may carry credentials).
func TestErrorMessagesNeverEmbedRawURLs(t *testing.T) {
	_, err := loadConfig(mapEnv(t, map[string]string{
		"UPSTREAM_MCP_URL":     "http://user:secret@up:7777",
		"MINT_URL":             "http://mint:8091",
		"MEDIA_INTERNAL_TOKEN": "tok",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user:") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

func mapEnv(t *testing.T, m map[string]string) func(string) string {
	t.Helper()
	return func(key string) string { return m[key] }
}

func drop(m map[string]string, key string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func merge(m map[string]string, key, val string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = val
	return out
}
