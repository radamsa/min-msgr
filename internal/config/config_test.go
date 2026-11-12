package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/minmsgr")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
	if cfg.MaxMessageBytes != defaultMaxMessageBytes {
		t.Errorf("MaxMessageBytes = %d, want %d", cfg.MaxMessageBytes, defaultMaxMessageBytes)
	}
	if cfg.MaxAPILimit != defaultMaxAPILimit {
		t.Errorf("MaxAPILimit = %d, want %d", cfg.MaxAPILimit, defaultMaxAPILimit)
	}
	if cfg.DefaultAPILimit != defaultDefaultAPILimit {
		t.Errorf("DefaultAPILimit = %d, want %d", cfg.DefaultAPILimit, defaultDefaultAPILimit)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
	if cfg.RetentionInterval != defaultRetentionEvery {
		t.Errorf("RetentionInterval = %v, want %v", cfg.RetentionInterval, defaultRetentionEvery)
	}
	if cfg.PublicBaseURL != defaultPublicBaseURL {
		t.Errorf("PublicBaseURL = %q, want %q", cfg.PublicBaseURL, defaultPublicBaseURL)
	}
	if cfg.CookieSecure {
		t.Error("CookieSecure = true, want false")
	}
	if cfg.SessionTTL != defaultSessionTTL {
		t.Errorf("SessionTTL = %v, want %v", cfg.SessionTTL, defaultSessionTTL)
	}
	if cfg.OAuthGitHub.Enabled() || cfg.OAuthGoogle.Enabled() || cfg.OAuthYandex.Enabled() {
		t.Error("OAuth providers should be disabled by default")
	}
}

func TestLoadOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/minmsgr")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("ARCHIVES_DIR", "/tmp/archives")
	t.Setenv("MAX_MESSAGE_BYTES", "1024")
	t.Setenv("MAX_API_LIMIT", "20")
	t.Setenv("DEFAULT_API_LIMIT", "10")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")
	t.Setenv("RETENTION_INTERVAL", "5m")
	t.Setenv("PUBLIC_BASE_URL", "https://chat.example.com/")
	t.Setenv("COOKIE_SECURE", "true")
	t.Setenv("SESSION_TTL", "24h")
	t.Setenv("OAUTH_GITHUB_CLIENT_ID", "github-id")
	t.Setenv("OAUTH_GITHUB_CLIENT_SECRET", "github-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.ArchivesDir != "/tmp/archives" {
		t.Errorf("unexpected string config: %+v", cfg)
	}
	if cfg.MaxMessageBytes != 1024 || cfg.MaxAPILimit != 20 || cfg.DefaultAPILimit != 10 {
		t.Errorf("unexpected limit config: %+v", cfg)
	}
	if cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 3s", cfg.ShutdownTimeout)
	}
	if cfg.RetentionInterval != 5*time.Minute {
		t.Errorf("RetentionInterval = %v, want 5m", cfg.RetentionInterval)
	}
	if cfg.PublicBaseURL != "https://chat.example.com" {
		t.Errorf("PublicBaseURL = %q, want https://chat.example.com", cfg.PublicBaseURL)
	}
	if !cfg.CookieSecure {
		t.Error("CookieSecure = false, want true")
	}
	if cfg.SessionTTL != 24*time.Hour {
		t.Errorf("SessionTTL = %v, want 24h", cfg.SessionTTL)
	}
	if !cfg.OAuthGitHub.Enabled() || cfg.OAuthGitHub.ClientID != "github-id" {
		t.Errorf("OAuthGitHub = %+v", cfg.OAuthGitHub)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "missing database url", key: "DATABASE_URL", value: "", want: "DATABASE_URL"},
		{name: "non integer", key: "MAX_MESSAGE_BYTES", value: "abc", want: "MAX_MESSAGE_BYTES"},
		{name: "zero message bytes", key: "MAX_MESSAGE_BYTES", value: "0", want: "MAX_MESSAGE_BYTES"},
		{name: "huge message bytes", key: "MAX_MESSAGE_BYTES", value: "999999999", want: "MAX_MESSAGE_BYTES"},
		{name: "zero api limit", key: "MAX_API_LIMIT", value: "0", want: "MAX_API_LIMIT"},
		{name: "huge api limit", key: "MAX_API_LIMIT", value: "100000", want: "MAX_API_LIMIT"},
		{name: "default above max", key: "DEFAULT_API_LIMIT", value: "101", want: "DEFAULT_API_LIMIT"},
		{name: "invalid duration", key: "SHUTDOWN_TIMEOUT", value: "soon", want: "SHUTDOWN_TIMEOUT"},
		{name: "invalid retention duration", key: "RETENTION_INTERVAL", value: "soon", want: "RETENTION_INTERVAL"},
		{name: "invalid session duration", key: "SESSION_TTL", value: "soon", want: "SESSION_TTL"},
		{name: "invalid cookie bool", key: "COOKIE_SECURE", value: "sometimes", want: "COOKIE_SECURE"},
		{name: "invalid base url", key: "PUBLIC_BASE_URL", value: "ftp://example.com", want: "PUBLIC_BASE_URL"},
		{name: "base url with query", key: "PUBLIC_BASE_URL", value: "https://example.com/?a=1", want: "PUBLIC_BASE_URL"},
		{name: "partial github config", key: "OAUTH_GITHUB_CLIENT_ID", value: "id", want: "OAUTH_GITHUB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/minmsgr")
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want mention of %s", err, tt.want)
			}
		})
	}
}

func TestValidateRejectsEmptyPaths(t *testing.T) {
	cfg := Config{
		HTTPAddr:        ":8080",
		DatabaseURL:     "postgres://localhost/minmsgr",
		ArchivesDir:     "",
		MaxMessageBytes: 1,
		MaxAPILimit:     1,
		DefaultAPILimit: 1,
		ShutdownTimeout: time.Second,
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ARCHIVES_DIR") {
		t.Fatalf("Validate() error = %v, want ARCHIVES_DIR", err)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"HTTP_ADDR", "DATABASE_URL", "ARCHIVES_DIR", "MAX_MESSAGE_BYTES",
		"MAX_API_LIMIT", "DEFAULT_API_LIMIT", "SHUTDOWN_TIMEOUT", "RETENTION_INTERVAL",
		"PUBLIC_BASE_URL", "COOKIE_SECURE", "SESSION_TTL",
		"OAUTH_GITHUB_CLIENT_ID", "OAUTH_GITHUB_CLIENT_SECRET",
		"OAUTH_GOOGLE_CLIENT_ID", "OAUTH_GOOGLE_CLIENT_SECRET",
		"OAUTH_YANDEX_CLIENT_ID", "OAUTH_YANDEX_CLIENT_SECRET",
	} {
		t.Setenv(key, "")
	}
}
