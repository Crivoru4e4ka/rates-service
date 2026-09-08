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
	"plata-rates/internal/metrics"
	"plata-rates/internal/rates"
	"plata-rates/internal/rates/cached"
	"plata-rates/internal/rates/exchangeratesapi"
	"plata-rates/internal/rates/fallback"
	"plata-rates/internal/rates/frankfurter"
	"plata-rates/internal/rates/resilient"
	"plata-rates/internal/service"
	pgstorage "plata-rates/internal/storage/postgres"
)

// version задаётся при сборке: -ldflags "-X main.version=v1.0.0".
var version = "dev"

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
	m := metrics.New(cfg.Provider)

	// Resilience у КАЖДОГО провайдера свой: ретраи не переносятся через
	// фолбэк на другого провайдера. Глобальный limiter + backoff + метрики
	// реальных вызовов.
	resilientOpts := resilient.Options{
		MaxAttempts: cfg.ProviderMaxAttempts,
		BaseDelay:   cfg.ProviderRetryBaseDelay,
		MaxDelay:    cfg.ProviderRetryMaxDelay,
		MinInterval: cfg.ProviderMinInterval,
		Logger:      logger,
		Metrics:     m,
	}
	var primary rates.Provider = resilient.New(newProvider(cfg, logger), resilientOpts)

	// Автоматический фолбэк (всегда включён): если основной провайдер вернул
	// ошибку после своих ретраев, запрос уходит к резервному — если тот
	// применим. Оба источника отдают курсы ЕЦБ, поэтому переключение
	// семантически прозрачно.
	secondary := newSecondaryProvider(cfg, logger)
	if secondary != nil {
		secondary = resilient.New(secondary, resilientOpts)
		primary = fallback.New(primary, secondary, fallback.Options{
			Logger:        logger,
			Metrics:       m,
			PrimaryName:   cfg.Provider,
			SecondaryName: secondaryName(cfg.Provider),
		})
	}

	// TTL-кэш ответов: повторные запросы одной пары в пределах TTL не тратят
	// квоту внешнего API. Кэш СНАРУЖИ остальных — cache-hit не ждёт limiter,
	// не тратит ретраи и не вызывает фолбэк.
	provider := cached.New(primary, cached.Options{
		TTL:     cfg.ProviderCacheTTL,
		Logger:  logger,
		Metrics: m,
	})

	svc := service.New(storage, provider, cfg.SupportedCurrencies, service.Options{
		QueueSize:      cfg.QueueSize,
		Workers:        cfg.Workers,
		RequestTimeout: cfg.ProviderTimeout,
		ResyncInterval: cfg.ResyncInterval,
		Logger:         logger,
		Metrics:        m,
	})
	m.QueueLen = svc.QueueLength

	if cfg.Provider == "exchangeratesapi" && cfg.RatesAPIKey == "" {
		logger.Warn("RATES_API_KEY пуст: запросы на обновление будут завершаться ошибкой, пока ключ не задан")
	}

	api := httpapi.New(svc, logger, httpapi.Options{
		Version: version,
		Metrics: m,
		Pprof:   cfg.PprofEnabled,
	})
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
		logger.Info("http-сервер запущен",
			"addr", cfg.HTTPAddr, "version", version, "provider", cfg.Provider,
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

// newProvider выбирает реализацию источника котировок по конфигурации.
// Оба провайдера реализуют rates.Provider, поэтому замена — только конфигом.
func newProvider(cfg config.Config, logger *slog.Logger) rates.Provider {
	switch cfg.Provider {
	case "exchangeratesapi":
		return exchangeratesapi.New(cfg.RatesAPIURL, cfg.RatesAPIKey, cfg.ProviderTimeout, logger)
	default: // frankfurter
		return frankfurter.New(cfg.RatesAPIURL, cfg.ProviderTimeout, logger)
	}
}

// secondaryName — имя резервного провайдера (для логов и метрики фолбэка).
func secondaryName(primary string) string {
	if primary == "frankfurter" {
		return exchangeratesapi.SourceName
	}
	return frankfurter.SourceName
}

// newSecondaryProvider — резервный провайдер: «другой» относительно
// основного. exchangeratesapi применим только при непустом RATES_API_KEY.
// Резервный всегда использует канонический URL своего API —
// RATES_API_URL переопределяет только основного провайдера.
func newSecondaryProvider(cfg config.Config, logger *slog.Logger) rates.Provider {
	switch cfg.Provider {
	case "exchangeratesapi":
		return frankfurter.New("https://api.frankfurter.dev/v1", cfg.ProviderTimeout, logger)
	case "frankfurter":
		if cfg.RatesAPIKey == "" {
			return nil // exchangeratesapi без ключа неработоспособен
		}
		return exchangeratesapi.New("https://api.exchangeratesapi.io/v1", cfg.RatesAPIKey, cfg.ProviderTimeout, logger)
	}
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
