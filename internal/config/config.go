package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — конфигурация сервиса, загружается из переменных окружения.
type Config struct {
	HTTPAddr    string
	DatabaseURL string

	Provider        string        // frankfurter | exchangeratesapi
	RatesAPIURL     string        // базовый URL провайдера
	RatesAPIKey     string        // access_key (используется только exchangeratesapi)
	ProviderTimeout time.Duration // таймаут одного вызова провайдера

	ProviderMaxAttempts    int           // попыток вызова провайдера (включая первую)
	ProviderRetryBaseDelay time.Duration // базовая задержка перед повтором
	ProviderRetryMaxDelay  time.Duration // потолок задержки между повторами
	ProviderMinInterval    time.Duration // минимальный интервал между вызовами провайдера (глобально)
	ProviderCacheTTL       time.Duration // TTL кэша ответов провайдера (0 — выключен)

	SupportedCurrencies []string // допустимые валюты пар, напр. USD,EUR,MXN

	Workers        int           // число фоновых воркеров
	QueueSize      int           // размер внутренней очереди на обновление
	ResyncInterval time.Duration // период повторной постановки pending-запросов в очередь

	HTTPReadTimeout  time.Duration
	HTTPWriteTimeout time.Duration
	HTTPIdleTimeout  time.Duration
	ShutdownTimeout  time.Duration

	PprofEnabled bool // поднимать /debug/pprof

	LogLevel  string // debug|info|warn|error
	LogFormat string // text|json
}

// Load читает конфигурацию из окружения, подставляя значения по умолчанию.
func Load() (Config, error) {
	provider := strings.ToLower(env("RATES_PROVIDER", "frankfurter"))
	var defaultURL string
	switch provider {
	case "frankfurter":
		defaultURL = "https://api.frankfurter.dev/v1"
	case "exchangeratesapi":
		defaultURL = "https://api.exchangeratesapi.io/v1"
	default:
		return Config{}, fmt.Errorf(
			"RATES_PROVIDER: неизвестный провайдер %q (допустимо: frankfurter, exchangeratesapi)", provider)
	}

	cfg := Config{
		HTTPAddr:    env("HTTP_ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", "postgres://rates:rates@localhost:5432/rates?sslmode=disable"),

		Provider:        provider,
		RatesAPIURL:     env("RATES_API_URL", defaultURL),
		RatesAPIKey:     env("RATES_API_KEY", ""),
		ProviderTimeout: envDuration("RATES_PROVIDER_TIMEOUT", 5*time.Second),

		ProviderMaxAttempts:    envInt("PROVIDER_MAX_ATTEMPTS", 3),
		ProviderRetryBaseDelay: envDuration("PROVIDER_RETRY_BASE_DELAY", 500*time.Millisecond),
		ProviderRetryMaxDelay:  envDuration("PROVIDER_RETRY_MAX_DELAY", 10*time.Second),
		ProviderMinInterval:    envDuration("PROVIDER_MIN_INTERVAL", 1*time.Second),
		ProviderCacheTTL:       envDuration("PROVIDER_CACHE_TTL", 60*time.Second),

		SupportedCurrencies: parseCurrencies(env("SUPPORTED_CURRENCIES", "USD,EUR,MXN")),

		Workers:        envInt("WORKERS", 4),
		QueueSize:      envInt("QUEUE_SIZE", 1024),
		ResyncInterval: envDuration("RESYNC_INTERVAL", 30*time.Second),

		HTTPReadTimeout:  envDuration("HTTP_READ_TIMEOUT", 5*time.Second),
		HTTPWriteTimeout: envDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
		HTTPIdleTimeout:  envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout:  envDuration("SHUTDOWN_TIMEOUT", 15*time.Second),

		PprofEnabled: envBool("PPROF_ENABLED", false),

		LogLevel:  env("LOG_LEVEL", "info"),
		LogFormat: env("LOG_FORMAT", "text"),
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.QueueSize < 1 {
		cfg.QueueSize = 1
	}
	if cfg.ProviderMaxAttempts < 1 {
		cfg.ProviderMaxAttempts = 1
	}
	if len(cfg.SupportedCurrencies) < 2 {
		return Config{}, fmt.Errorf("SUPPORTED_CURRENCIES: нужно минимум две валюты, получено %q",
			strings.Join(cfg.SupportedCurrencies, ","))
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func parseCurrencies(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		c := strings.ToUpper(strings.TrimSpace(p))
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}
