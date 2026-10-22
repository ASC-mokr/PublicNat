// Entry point control plane: HTTP API + синхронизация конфигурации RatHole.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/nat-platform/backend/internal/api"
	"github.com/example/nat-platform/backend/internal/config"
	"github.com/example/nat-platform/backend/internal/rathole"
	"github.com/example/nat-platform/backend/internal/storage"
)

func main() {
	// Структурированное логирование (JSON) — каждый шаг генерации конфига и SIGHUP виден в логах.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	migrationsDir := getenvOr("MIGRATIONS_DIR", "./migrations")
	store, err := storage.New(ctx, cfg.DatabaseURL, migrationsDir)
	if err != nil {
		logger.Error("failed to connect database / apply migrations", "error", err, "migrations_dir", migrationsDir)
		os.Exit(1)
	}
	defer store.Close()
	logger.Info("database connected, migrations applied", "dir", migrationsDir)

	syncer := rathole.NewSyncer(store, rathole.Options{
		ConfigPath: cfg.RatholeConfigPath,
		BindAddr:   cfg.RatholeBindAddr,
		Container:  cfg.RatholeContainer,
		PID:        cfg.RatholePID,
		PIDFile:    cfg.RatholePIDFile,
		Logger:     logger,
	})

	// Первичная синхронизация при старте: конфиг всегда отражает состояние БД
	// (например, после рестарта бэкэнда или ручных правок в БД).
	if err := syncer.SyncRatholeConfig(context.Background()); err != nil {
		logger.Error("initial rathole config sync failed", "error", err)
		// Не завершаем работу: API всё равно полезен, а следующий create/delete повторит синхронизацию.
	}

	srv := api.NewServer(cfg, store, syncer, logger)
	httpSrv := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("HTTP API listening", "addr", cfg.APIAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server crashed", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown по SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	logger.Info("server stopped")
}

func getenvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
