// Package fallback — декоратор над провайдером котировок: автоматический
// резерв. Всегда включён, когда резервный провайдер применим (для
// exchangeratesapi требуется непустой RATES_API_KEY); без резервного
// декоратор прозрачен.
//
// Триггер: основной вернул ошибку ПОСЛЕ исчерпания своих ретраев (resilient
// обёрнут вокруг каждого провайдера отдельно — ретраи не переносятся через
// фолбэк). При истёкшем ctx фолбэк не выполняется — бюджет времени уже
// потрачен. При сбое обоих возвращается ошибка ОСНОВНОГО (сконфигурированного)
// провайдера, ошибка резервного уходит в лог.
package fallback

import (
	"context"
	"log/slog"

	"plata-rates/internal/domain"
	"plata-rates/internal/metrics"
	"plata-rates/internal/rates"
)

type Options struct {
	Logger        *slog.Logger
	Metrics       *metrics.Metrics // nil-safe
	PrimaryName   string           // имя основного провайдера (для логов)
	SecondaryName string           // имя резервного (логи + лейбл метрики)
}

type Fallback struct {
	primary       rates.Provider
	secondary     rates.Provider
	primaryName   string
	secondaryName string
	logger        *slog.Logger
	metrics       *metrics.Metrics
}

// New собирает декоратор; secondary == nil — работает только основной.
func New(primary, secondary rates.Provider, opts Options) *Fallback {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Fallback{
		primary:       primary,
		secondary:     secondary,
		primaryName:   opts.PrimaryName,
		secondaryName: opts.SecondaryName,
		logger:        opts.Logger,
		metrics:       opts.Metrics,
	}
}

// Rate: основной провайдер; при его ошибке и наличии резервного — запрос
// повторяется у резервного. Ошибка резервного логируется, наружу уходит
// ошибка основного (сконфигурированного источника).
func (f *Fallback) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	rate, err := f.primary.Rate(ctx, pair)
	if err == nil {
		return rate, nil
	}
	if f.secondary == nil || ctx.Err() != nil {
		return rates.Rate{}, err
	}

	f.metrics.ObserveFallback(f.secondaryName)
	f.logger.Warn("основной провайдер недоступен, переключаюсь на резервный",
		"primary", f.primaryName,
		"secondary", f.secondaryName,
		"pair", pair.String(),
		"err", err)

	rate2, err2 := f.secondary.Rate(ctx, pair)
	if err2 == nil {
		return rate2, nil
	}
	f.logger.Error("резервный провайдер также недоступен",
		"primary_err", err,
		"secondary_err", err2)
	return rates.Rate{}, err
}
