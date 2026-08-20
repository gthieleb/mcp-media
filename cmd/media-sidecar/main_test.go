package main

import (
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
