// Package cached — декоратор провайдера котировок: TTL-кэш успешных ответов
// в памяти процесса. Повторные запросы одной и той же пары в пределах TTL
// не расходуют квоту внешнего API (N запросов = 1 платный вызов).
//
// Ошибки не кэшируются: сбойный вызов можно сразу повторить новым запросом.
// Кэш ограничен числом поддерживаемых пар (whitelist), вытеснение не нужно.
package cached

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/metrics"
	"plata-rates/internal/rates"
)

type Options struct {
	TTL     time.Duration // срок жизни кэшированного ответа; 0 — кэш отключён
	Logger  *slog.Logger
	Metrics *metrics.Metrics // nil-safe
	Now     func() time.Time // точка времени для тестов; nil — time.Now
}

type entry struct {
	rate      rates.Rate
	expiresAt time.Time
}

type Cache struct {
	inner   rates.Provider
	ttl     time.Duration
	logger  *slog.Logger
	metrics *metrics.Metrics
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

// New оборачивает провайдера TTL-кэшем. При TTL <= 0 кэш прозрачен:
// все вызовы уходят во внутренний провайдер.
func New(inner rates.Provider, opts Options) *Cache {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{
		inner:   inner,
		ttl:     opts.TTL,
		logger:  opts.Logger,
		metrics: opts.Metrics,
		now:     opts.Now,
		entries: make(map[string]entry),
	}
}

// Rate возвращает цену пары: из кэша, если там ещё свежий успешный ответ,
// иначе — вызовом внутреннего провайдера с последующим сохранением.
// На попадании возвращается исходный FetchedAt — время реального получения
// данных, без «омоложения».
func (c *Cache) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	if c.ttl <= 0 {
		return c.inner.Rate(ctx, pair)
	}
	key := pair.String()

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expiresAt) {
		c.mu.Unlock()
		c.metrics.ObserveProviderCache(true)
		return e.rate, nil
	}
	c.mu.Unlock()

	rate, err := c.inner.Rate(ctx, pair)
	if err != nil {
		return rates.Rate{}, err
	}

	c.mu.Lock()
	c.entries[key] = entry{rate: rate, expiresAt: c.now().Add(c.ttl)}
	c.mu.Unlock()

	c.metrics.ObserveProviderCache(false)
	return rate, nil
}
