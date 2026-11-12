package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr        = ":8080"
	defaultArchivesDir     = "./data/archives"
	defaultMaxMessageBytes = 64 * 1024
	defaultMaxAPILimit     = 100
	defaultDefaultAPILimit = 50
	defaultShutdownTimeout = 10 * time.Second
	defaultRetentionEvery  = time.Hour
	defaultPublicBaseURL   = "http://localhost:8080"
	defaultSessionTTL      = 30 * 24 * time.Hour
	upperBoundMessageBytes = 16 * 1024 * 1024
	upperBoundAPILimit     = 1000
)

// OAuthProviderConfig holds credentials for one OAuth provider. An empty pair
// disables the provider.
type OAuthProviderConfig struct {
	ClientID     string
	ClientSecret string
}

// Enabled reports whether the provider has credentials.
func (p OAuthProviderConfig) Enabled() bool {
	return p.ClientID != "" && p.ClientSecret != ""
}

// Config contains runtime settings loaded from the environment.
type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	ArchivesDir       string
	MaxMessageBytes   int
	MaxAPILimit       int
	DefaultAPILimit   int
	ShutdownTimeout   time.Duration
	RetentionInterval time.Duration
	PublicBaseURL     string
	CookieSecure      bool
	SessionTTL        time.Duration
	OAuthGitHub       OAuthProviderConfig
	OAuthGoogle       OAuthProviderConfig
	OAuthYandex       OAuthProviderConfig
}

// Load reads configuration from environment variables, applies defaults, and
// validates the result. Invalid values fail before the server starts.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:          envString("HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL:       envString("DATABASE_URL", ""),
		ArchivesDir:       envString("ARCHIVES_DIR", defaultArchivesDir),
		ShutdownTimeout:   defaultShutdownTimeout,
		RetentionInterval: defaultRetentionEvery,
		PublicBaseURL:     strings.TrimRight(envString("PUBLIC_BASE_URL", defaultPublicBaseURL), "/"),
		SessionTTL:        defaultSessionTTL,
		OAuthGitHub: OAuthProviderConfig{
			ClientID:     os.Getenv("OAUTH_GITHUB_CLIENT_ID"),
			ClientSecret: os.Getenv("OAUTH_GITHUB_CLIENT_SECRET"),
		},
		OAuthGoogle: OAuthProviderConfig{
			ClientID:     os.Getenv("OAUTH_GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("OAUTH_GOOGLE_CLIENT_SECRET"),
		},
		OAuthYandex: OAuthProviderConfig{
			ClientID:     os.Getenv("OAUTH_YANDEX_CLIENT_ID"),
			ClientSecret: os.Getenv("OAUTH_YANDEX_CLIENT_SECRET"),
		},
	}

	var err error
	if cfg.CookieSecure, err = envBool("COOKIE_SECURE", false); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = envDuration("SESSION_TTL", defaultSessionTTL); err != nil {
		return Config{}, err
	}
	if cfg.MaxMessageBytes, err = envInt("MAX_MESSAGE_BYTES", defaultMaxMessageBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxAPILimit, err = envInt("MAX_API_LIMIT", defaultMaxAPILimit); err != nil {
		return Config{}, err
	}
	if cfg.DefaultAPILimit, err = envInt("DEFAULT_API_LIMIT", defaultDefaultAPILimit); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.RetentionInterval, err = envDuration("RETENTION_INTERVAL", defaultRetentionEvery); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects values that would make the service unsafe or unusable.
func (c Config) Validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("HTTP_ADDR must not be empty")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL must not be empty")
	}
	if c.ArchivesDir == "" {
		return fmt.Errorf("ARCHIVES_DIR must not be empty")
	}
	if c.MaxMessageBytes <= 0 || c.MaxMessageBytes > upperBoundMessageBytes {
		return fmt.Errorf("MAX_MESSAGE_BYTES must be between 1 and %d", upperBoundMessageBytes)
	}
	if c.MaxAPILimit <= 0 || c.MaxAPILimit > upperBoundAPILimit {
		return fmt.Errorf("MAX_API_LIMIT must be between 1 and %d", upperBoundAPILimit)
	}
	if c.DefaultAPILimit <= 0 || c.DefaultAPILimit > c.MaxAPILimit {
		return fmt.Errorf("DEFAULT_API_LIMIT must be between 1 and MAX_API_LIMIT")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("SHUTDOWN_TIMEOUT must be positive")
	}
	if c.RetentionInterval <= 0 {
		return fmt.Errorf("RETENTION_INTERVAL must be positive")
	}
	if c.SessionTTL <= 0 {
		return fmt.Errorf("SESSION_TTL must be positive")
	}
	if err := validateBaseURL(c.PublicBaseURL); err != nil {
		return err
	}
	providers := []struct {
		name string
		cfg  OAuthProviderConfig
	}{
		{name: "OAUTH_GITHUB", cfg: c.OAuthGitHub},
		{name: "OAUTH_GOOGLE", cfg: c.OAuthGoogle},
		{name: "OAUTH_YANDEX", cfg: c.OAuthYandex},
	}
	for _, provider := range providers {
		if (provider.cfg.ClientID == "") != (provider.cfg.ClientSecret == "") {
			return fmt.Errorf("%s_CLIENT_ID and %s_CLIENT_SECRET must be set together",
				provider.name, provider.name)
		}
	}
	return nil
}

func validateBaseURL(value string) error {
	if value == "" {
		return fmt.Errorf("PUBLIC_BASE_URL must not be empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("PUBLIC_BASE_URL is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("PUBLIC_BASE_URL must use http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("PUBLIC_BASE_URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("PUBLIC_BASE_URL must not include a query or fragment")
	}
	return nil
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}
