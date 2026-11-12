package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dav/min-msgr/internal/api"
	"github.com/dav/min-msgr/internal/config"
	"github.com/dav/min-msgr/internal/db"
	"github.com/dav/min-msgr/internal/oauth"
	"github.com/dav/min-msgr/internal/realtime"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/web"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	if err := os.MkdirAll(cfg.ArchivesDir, 0o750); err != nil {
		return fmt.Errorf("create archives directory: %w", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	hub := realtime.NewHub()
	limits := service.NewLimitsService(pool)
	admins := service.NewAdminService(pool)
	retention := service.NewRetentionService(pool, cfg.ArchivesDir, hub)
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		retention.Run(runCtx, cfg.RetentionInterval)
	}()

	webHandler, err := web.NewHandler(web.Options{
		OAuth:     service.NewOAuthService(pool),
		Sessions:  service.NewSessionService(pool, cfg.SessionTTL),
		Keys:      service.NewKeyService(pool),
		Archives:  service.NewArchiveService(pool, cfg.ArchivesDir),
		Chats:     postgres.New(pool),
		Trimmer:   limits,
		Hub:       hub,
		Admins:    admins,
		Providers: buildOAuthProviders(cfg),
		Config: web.Config{
			CookieSecure:    cfg.CookieSecure,
			MaxMessageBytes: cfg.MaxMessageBytes,
			PageLimit:       cfg.DefaultAPILimit,
		},
	})
	if err != nil {
		return fmt.Errorf("create web handler: %w", err)
	}

	apiHandler := api.NewHandler(postgres.New(pool), api.Config{
		MaxMessageBytes: cfg.MaxMessageBytes,
		MaxAPILimit:     cfg.MaxAPILimit,
		DefaultAPILimit: cfg.DefaultAPILimit,
		Notifier:        hub,
		Trimmer:         limits,
	})
	root := http.NewServeMux()
	root.Handle("/api/", apiHandler)
	root.Handle("/healthz", apiHandler)
	root.Handle("/", webHandler.Routes())

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", cfg.HTTPAddr)
		serverErrCh <- server.ListenAndServe()
	}()

	var serveErr error
	select {
	case serveErr = <-serverErrCh:
	case <-ctx.Done():
		slog.Info("shutdown requested")
	}
	cancelRun()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	background.Wait()

	if shutdownErr != nil {
		return fmt.Errorf("shutdown http server: %w", shutdownErr)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", serveErr)
	}
	return nil
}

func buildOAuthProviders(cfg config.Config) []*oauth.Provider {
	var providers []*oauth.Provider
	base := cfg.PublicBaseURL

	if cfg.OAuthGitHub.Enabled() {
		providers = append(providers, oauth.NewGitHub(oauth.ProviderConfig{
			ClientID:     cfg.OAuthGitHub.ClientID,
			ClientSecret: cfg.OAuthGitHub.ClientSecret,
			RedirectURL:  base + "/auth/github/callback",
		}))
	}
	if cfg.OAuthGoogle.Enabled() {
		providers = append(providers, oauth.NewGoogle(oauth.ProviderConfig{
			ClientID:     cfg.OAuthGoogle.ClientID,
			ClientSecret: cfg.OAuthGoogle.ClientSecret,
			RedirectURL:  base + "/auth/google/callback",
		}))
	}
	if cfg.OAuthYandex.Enabled() {
		providers = append(providers, oauth.NewYandex(oauth.ProviderConfig{
			ClientID:     cfg.OAuthYandex.ClientID,
			ClientSecret: cfg.OAuthYandex.ClientSecret,
			RedirectURL:  base + "/auth/yandex/callback",
		}))
	}
	return providers
}
