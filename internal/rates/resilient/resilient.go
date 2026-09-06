// Package resilient — обёртка над провайдером котировок: глобальное
// ограничение частоты вызовов (token-interval) и повторные попытки
// с экспоненциальным backoff и джиттером для транзистентных ошибок.
package resilient

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

type Options struct {
	MaxAttempts int           // всего попыток (включая первую)
	BaseDelay   time.Duration // базовая задержка перед первой повторной попыткой
	MaxDelay    time.Duration // потолок задержки между попытками
	MinInterval time.Duration // минимальный интервал между вызовами провайдера (глобально)
	Logger      *slog.Logger
}

type Resilient struct {
	inner  rates.Provider
	opts   Options
	logger *slog.Logger

	mu          sync.Mutex
	nextAllowed time.Time // ближайший момент, когда можно звонить провайдеру
}

// New оборачивает провайдера; при нулевых опциях использует разумные дефолты.
func New(inner rates.Provider, opts Options) *Resilient {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = 500 * time.Millisecond
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = 10 * time.Second
	}
	if opts.MinInterval < 0 {
		opts.MinInterval = 0
	}
	return &Resilient{inner: inner, opts: opts, logger: opts.Logger}
}

// Rate выполняет вызов провайдера с ограничением частоты и повторами.
func (r *Resilient) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	var lastErr error
	for attempt := 1; attempt <= r.opts.MaxAttempts; attempt++ {
		if err := r.gate(ctx); err != nil {
			return rates.Rate{}, err
		}

		rate, err := r.inner.Rate(ctx, pair)
		if err == nil {
			return rate, nil
		}
		lastErr = err

		if ctx.Err() != nil || !rates.IsRetryable(err) || attempt == r.opts.MaxAttempts {
			return rates.Rate{}, err
		}

		delay := r.delayFor(attempt, rates.RetryAfterFrom(err))
		r.logger.Warn("провайдер временно недоступен, повторяю",
			"attempt", attempt, "max_attempts", r.opts.MaxAttempts,
			"delay", delay.String(), "pair", pair.String(), "err", err)

		if err := sleepCtx(ctx, delay); err != nil {
			return rates.Rate{}, err
		}
	}
	return rates.Rate{}, lastErr
}

// gate резервирует слот вызова: гарантирует, что между фактическими вызовами
// провайдера (по всем горутинам) проходит не меньше MinInterval.
func (r *Resilient) gate(ctx context.Context) error {
	if r.opts.MinInterval == 0 {
		return ctx.Err()
	}

	r.mu.Lock()
	now := time.Now()
	var wait time.Duration
	if r.nextAllowed.After(now) {
		wait = r.nextAllowed.Sub(now)
		r.nextAllowed = r.nextAllowed.Add(r.opts.MinInterval)
	} else {
		r.nextAllowed = now.Add(r.opts.MinInterval)
	}
	r.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// delayFor — задержка перед повтором: экспоненциальный backoff с джиттером
// ±20%, но не больше MaxDelay; значение Retry-After имеет приоритет.
func (r *Resilient) delayFor(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > r.opts.MaxDelay {
			return r.opts.MaxDelay
		}
		return retryAfter
	}

	d := r.opts.BaseDelay << (attempt - 1)
	if d <= 0 || d > r.opts.MaxDelay {
		d = r.opts.MaxDelay
	}
	jitter := d / 5
	if jitter > 0 {
		d += time.Duration(rand.Int64N(int64(2*jitter))) - jitter
	}
	if d > r.opts.MaxDelay {
		d = r.opts.MaxDelay
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
