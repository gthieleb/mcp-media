package main

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envStub builds a getenv func from a map (missing keys read as "").
func envStub(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// validEnv is the minimal valid configuration; tests mutate copies of it.
func validEnv() map[string]string {
	return map[string]string{
		"MEDIA_SIGNING_SECRET":  strings.Repeat("s", 32),
		"MEDIA_INTERNAL_TOKEN":  "test-token",
		"MEDIA_PUBLIC_BASE_URL": "http://localhost:8090",
	}
}

func TestLoadConfigValidDefaults(t *testing.T) {
	cfg, err := loadConfig(envStub(validEnv()))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Secret) != 32 {
		t.Errorf("Secret length = %d, want 32", len(cfg.Secret))
	}
	if cfg.Token != "test-token" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.PublicBaseURL != "http://localhost:8090" {
		t.Errorf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
	if len(cfg.Roots) != 1 || cfg.Roots[0] != "/data" {
		t.Errorf("Roots = %v, want [/data]", cfg.Roots)
	}
	if cfg.ServeAddr != ":8090" {
		t.Errorf("ServeAddr = %q, want :8090", cfg.ServeAddr)
	}
	if cfg.MintAddr != ":8091" {
		t.Errorf("MintAddr = %q, want :8091", cfg.MintAddr)
	}
	if cfg.DefaultTTL != 300*time.Second {
		t.Errorf("DefaultTTL = %s, want 300s", cfg.DefaultTTL)
	}
	if cfg.MaxTTL != 900*time.Second {
		t.Errorf("MaxTTL = %s, want 900s", cfg.MaxTTL)
	}
}

func TestLoadConfigValidOverrides(t *testing.T) {
	env := validEnv()
	env["MEDIA_ROOTS"] = " /data/a , /data/b "
	env["MEDIA_SERVE_ADDR"] = ":18090"
	env["MEDIA_MINT_ADDR"] = "127.0.0.1:18091"
	env["MEDIA_DEFAULT_TTL"] = "1m"
	env["MEDIA_MAX_TTL"] = "1h"

	cfg, err := loadConfig(envStub(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Roots) != 2 || cfg.Roots[0] != "/data/a" || cfg.Roots[1] != "/data/b" {
		t.Errorf("Roots = %v", cfg.Roots)
	}
	if cfg.ServeAddr != ":18090" {
		t.Errorf("ServeAddr = %q", cfg.ServeAddr)
	}
	if cfg.MintAddr != "127.0.0.1:18091" {
		t.Errorf("MintAddr = %q", cfg.MintAddr)
	}
	if cfg.DefaultTTL != time.Minute {
		t.Errorf("DefaultTTL = %s", cfg.DefaultTTL)
	}
	if cfg.MaxTTL != time.Hour {
		t.Errorf("MaxTTL = %s", cfg.MaxTTL)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]func(map[string]string){
		"missing secret": func(env map[string]string) { delete(env, "MEDIA_SIGNING_SECRET") },
		"short secret": func(env map[string]string) {
			env["MEDIA_SIGNING_SECRET"] = strings.Repeat("s", 31)
		},
		"missing token":    func(env map[string]string) { delete(env, "MEDIA_INTERNAL_TOKEN") },
		"missing base url": func(env map[string]string) { delete(env, "MEDIA_PUBLIC_BASE_URL") },
		"base url no host": func(env map[string]string) { env["MEDIA_PUBLIC_BASE_URL"] = "http://" },
		"base url bad proto": func(env map[string]string) {
			env["MEDIA_PUBLIC_BASE_URL"] = "ftp://example.com"
		},
		"base url with userinfo": func(env map[string]string) {
			env["MEDIA_PUBLIC_BASE_URL"] = "https://user:s3cr3tPW@example.com"
		},
		"base url parse error": func(env map[string]string) {
			env["MEDIA_PUBLIC_BASE_URL"] = "http://exa mple.com/media"
		},
		"relative root":     func(env map[string]string) { env["MEDIA_ROOTS"] = "data" },
		"empty roots":       func(env map[string]string) { env["MEDIA_ROOTS"] = " , " },
		"bad default ttl":   func(env map[string]string) { env["MEDIA_DEFAULT_TTL"] = "soon" },
		"bad max ttl":       func(env map[string]string) { env["MEDIA_MAX_TTL"] = "-5s" },
		"max below default": func(env map[string]string) { env["MEDIA_MAX_TTL"] = "60s" },
		"zero default ttl":  func(env map[string]string) { env["MEDIA_DEFAULT_TTL"] = "0s" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			mutate(env)
			if _, err := loadConfig(envStub(env)); err == nil {
				t.Fatalf("loadConfig(%s) succeeded, want error", name)
			} else if strings.Contains(err.Error(), env["MEDIA_SIGNING_SECRET"]) && len(env["MEDIA_SIGNING_SECRET"]) > 0 {
				t.Fatalf("error leaks secret: %v", err)
			}
		})
	}
}

func TestLoadConfigLongerSecretAccepted(t *testing.T) {
	env := validEnv()
	env["MEDIA_SIGNING_SECRET"] = strings.Repeat("k", 64)
	cfg, err := loadConfig(envStub(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Secret) != 64 {
		t.Errorf("Secret length = %d, want 64", len(cfg.Secret))
	}
}

func TestConfigLogValueRedactsSecrets(t *testing.T) {
	env := validEnv()
	cfg, err := loadConfig(envStub(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	logged := cfg.logValue().String()
	for _, leaked := range []string{env["MEDIA_SIGNING_SECRET"], env["MEDIA_INTERNAL_TOKEN"]} {
		if strings.Contains(logged, leaked) {
			t.Fatalf("startup log leaks secret material %q in %q", leaked, logged)
		}
	}
	if !strings.Contains(logged, "secret_bytes=32") || !strings.Contains(logged, "token_bytes=10") {
		t.Errorf("expected length-only secret info, got %q", logged)
	}
}

// Regression test for review I2: userinfo credentials in
// MEDIA_PUBLIC_BASE_URL must be rejected outright.
func TestLoadConfigRejectsUserinfoBaseURL(t *testing.T) {
	env := validEnv()
	env["MEDIA_PUBLIC_BASE_URL"] = "https://user:s3cr3tPW@example.com"

	if _, err := loadConfig(envStub(env)); err == nil {
		t.Fatal("loadConfig accepted a base URL with userinfo credentials")
	}
}

// Regression test for review I2: the raw MEDIA_PUBLIC_BASE_URL value must
// never be echoed into error messages — it may carry credentials.
func TestLoadConfigBaseURLErrorsDoNotLeak(t *testing.T) {
	const marker = "s3cr3tPW"
	cases := map[string]string{
		"userinfo rejected": "https://user:" + marker + "@example.com",
		"parse error":       "http://exa mple.com/" + marker,
		"bad scheme":        "ftp://" + marker + ".example.com",
		"no host":           "https://user:" + marker + "@",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			env["MEDIA_PUBLIC_BASE_URL"] = raw
			_, err := loadConfig(envStub(env))
			if err == nil {
				t.Fatalf("loadConfig accepted %q, want error", name)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error leaks base-URL credential material: %v", err)
			}
		})
	}
}

// --- run() lifecycle tests (review I3) -------------------------------------

// testConfig builds a valid run() configuration on loopback with
// OS-assigned free ports and a temp media root (hermetic, no fixed ports).
func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		Secret:        []byte(strings.Repeat("s", 32)),
		Token:         "test-token",
		PublicBaseURL: "http://localhost:8090",
		Roots:         []string{t.TempDir()},
		ServeAddr:     freeAddr(t),
		MintAddr:      freeAddr(t),
		DefaultTTL:    300 * time.Second,
		MaxTTL:        900 * time.Second,
	}
}

// freeAddr finds a free loopback address by binding :0 and closing.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// waitForPort polls addr until it accepts a TCP connection or the deadline
// expires (polling with deadline, no fixed sleep).
func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("addr %s did not accept connections within 2s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunHappyPathShutdownOnCancel(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()

	waitForPort(t, cfg.ServeAddr)
	waitForPort(t, cfg.MintAddr)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return within 2s of context cancel")
	}
}

// Regression test for review I1: a failing server must surface as a
// non-nil run() error (pre-fix run returned nil → process exit code 0).
func TestRunReturnsErrorOnServerFailure(t *testing.T) {
	cfg := testConfig(t)
	// Pre-occupy the serve port so the serve server fails to bind.
	l, err := net.Listen("tcp", cfg.ServeAddr)
	if err != nil {
		t.Fatalf("pre-occupy serve port: %v", err)
	}
	defer l.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- run(context.Background(), cfg) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("run returned nil although the serve server failed to bind")
		}
		if !strings.Contains(err.Error(), "serve server") {
			t.Errorf("error %v does not name the failing server", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return within 2s of the server failure")
	}
}

// Regression test for review M2: run must fail fast on a missing root.
func TestRunFailsOnMissingRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.Roots = []string{filepath.Join(t.TempDir(), "does-not-exist")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfg) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("run returned nil with a nonexistent media root")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not fail fast on a nonexistent media root")
	}
}
