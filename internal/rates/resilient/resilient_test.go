package resilient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeInner struct {
	attempts atomic.Int32
	failures int   // сколько первых попыток вернуть ошибку
	err      error // ошибка для сбоев
	price    float64
}

func (f *fakeInner) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	n := int(f.attempts.Add(1))
	if n <= f.failures {
		return rates.Rate{}, f.err
	}
	return rates.Rate{Pair: pair, Price: f.price}, nil
}

func transientErr() error {
	return rates.Transient(errors.New("provider is down"), 0)
}

func mustPair(t *testing.T, s string) domain.Pair {
	t.Helper()
	p, err := domain.ParsePair(s)
	if err != nil {
		t.Fatalf("ParsePair(%q): %v", s, err)
	}
	return p
}

func TestRate_RetriesTransientThenSucceeds(t *testing.T) {
	inner := &fakeInner{failures: 2, err: transientErr(), price: 19.5}
	r := New(inner, Options{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Logger:      discardLogger(),
	})

	rate, err := r.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err != nil {
		t.Fatalf("Rate(): %v", err)
	}
	if rate.Price != 19.5 {
		t.Fatalf("price = %v", rate.Price)
	}
	if got := inner.attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestRate_NonRetryableFailsFast(t *testing.T) {
	inner := &fakeInner{failures: 1, err: errors.New("permanent")}
	r := New(inner, Options{MaxAttempts: 5, BaseDelay: time.Millisecond, Logger: discardLogger()})

	if _, err := r.Rate(context.Background(), mustPair(t, "EUR/MXN")); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if got := inner.attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (без ретраев)", got)
	}
}

func TestRate_ExhaustedAttempts(t *testing.T) {
	inner := &fakeInner{failures: 999, err: transientErr()}
	r := New(inner, Options{MaxAttempts: 4, BaseDelay: time.Millisecond, Logger: discardLogger()})

	_, err := r.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if got := inner.attempts.Load(); got != 4 {
		t.Fatalf("attempts = %d, want 4", got)
	}
	if !rates.IsRetryable(err) {
		t.Fatalf("IsRetryable = false, want true")
	}
}

func TestRate_ContextCanceledBeforeCall(t *testing.T) {
	inner := &fakeInner{}
	r := New(inner, Options{MaxAttempts: 3, BaseDelay: time.Second, Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Rate(ctx, mustPair(t, "EUR/MXN")); err == nil {
		t.Fatal("ожидалась ошибка отмены контекста")
	}
}

func TestGate_MinIntervalSpacing(t *testing.T) {
	inner := &fakeInner{price: 1}
	r := New(inner, Options{MaxAttempts: 1, MinInterval: 60 * time.Millisecond, Logger: discardLogger()})

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := r.Rate(context.Background(), mustPair(t, "EUR/MXN")); err != nil {
			t.Fatalf("Rate(): %v", err)
		}
	}
	elapsed := time.Since(start)
	// Три вызова: два из них должны подождать по ~60мс → суммарно >= ~100мс.
	if elapsed < 100*time.Millisecond {
		t.Fatalf("elapsed = %v, ожидалось >= 100мс (gate не работает)", elapsed)
	}
}
