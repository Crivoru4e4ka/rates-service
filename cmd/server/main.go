package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plata-rates/internal/config"
	"plata-rates/internal/httpapi"
	"plata-rates/internal/rates/exchangeratesapi"
	"plata-rates/internal/service"
	pgstorage "plata-rates/internal/storage/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("загрузить конфигурацию: %w", err)
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("подключиться к postgres: %w", err)
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("пинг postgres: %w", err)
	}

	if err := pgstorage.Migrate(pingCtx, pool); err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	logger.Info("миграции применены")

	storage := pgstorage.New(pool)
	provider := exchangeratesapi.New(cfg.RatesAPIURL, cfg.RatesAPIKey, cfg.ProviderTimeout, logger)
	if cfg.RatesAPIKey == "" {
		logger.Warn("RATES_API_KEY пуст: запросы на обновление будут завершаться ошибкой, пока ключ не задан")
	}

	svc := service.New(storage, provider, cfg.SupportedCurrencies, service.Options{
		QueueSize:      cfg.QueueSize,
		Workers:        cfg.Workers,
		RequestTimeout: cfg.ProviderTimeout,
		ResyncInterval: cfg.ResyncInterval,
		Logger:         logger,
	})

	api := httpapi.New(svc, logger)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: cfg.HTTPReadTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http-сервер запущен", "addr", cfg.HTTPAddr,
			"swagger", "http://localhost"+cfg.HTTPAddr+"/swagger/")
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http-сервер: %w", err)
		}
	case <-ctx.Done():
		logger.Info("получен сигнал завершения")
	}

	shCtx, shCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shCancel()
	if err := srv.Shutdown(shCtx); err != nil {
		logger.Error("остановка http-сервера", "err", err)
	}
	svc.Shutdown(shCtx)
	logger.Info("сервис остановлен")
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.LogFormat, "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
