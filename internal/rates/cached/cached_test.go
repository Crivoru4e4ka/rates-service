package cached

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/metrics"
	"plata-rates/internal/rates"
)

type fakeInner struct {
	calls    atomic.Int32
	failures int
	err      error
	price    float64
}

func (f *fakeInner) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	n := int(f.calls.Add(1))
	if n <= f.failures {
		return rates.Rate{}, f.err
	}
	return rates.Rate{Pair: pair, Price: f.price, FetchedAt: time.Now().UTC()}, nil
}

func mustPair(t *testing.T, s string) domain.Pair {
	t.Helper()
	p, err := domain.ParsePair(s)
	if err != nil {
		t.Fatalf("ParsePair(%q): %v", s, err)
	}
	return p
}

func TestRate_HitWithinTTL(t *testing.T) {
	inner := &fakeInner{price: 19.5}
	cur := time.Unix(1700000000, 0)
	c := New(inner, Options{TTL: time.Minute, Now: func() time.Time { return cur }})

	r1, err := c.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err != nil {
		t.Fatal(err)
	}

	if r2.Price != 19.5 {
		t.Fatalf("price = %v, want 19.5", r2.Price)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("внутренних вызовов = %d, want 1 (второй должен быть из кэша)", got)
	}
	if !r2.FetchedAt.Equal(r1.FetchedAt) {
		t.Fatalf("hit должен вернуть исходный FetchedAt: %v != %v", r2.FetchedAt, r1.FetchedAt)
	}
}

func TestRate_MissAfterTTLExpiry(t *testing.T) {
	inner := &fakeInner{price: 19.5}
	cur := time.Unix(1700000000, 0)
	c := New(inner, Options{TTL: time.Minute, Now: func() time.Time { return cur }})

	ctx := context.Background()
	p := mustPair(t, "EUR/MXN")
	if _, err := c.Rate(ctx, p); err != nil {
		t.Fatal(err)
	}

	cur = cur.Add(2 * time.Minute) // TTL истёк
	if _, err := c.Rate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("внутренних вызовов = %d, want 2 (после истечения TTL нужен новый вызов)", got)
	}
}

func TestRate_PairsIndependent(t *testing.T) {
	inner := &fakeInner{price: 1}
	c := New(inner, Options{TTL: time.Minute})

	ctx := context.Background()
	if _, err := c.Rate(ctx, mustPair(t, "EUR/MXN")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rate(ctx, mustPair(t, "USD/MXN")); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("внутренних вызовов = %d, want 2 (разные пары не делят запись)", got)
	}
}

func TestRate_DisabledWhenTTLZero(t *testing.T) {
	inner := &fakeInner{price: 1}
	c := New(inner, Options{}) // TTL = 0 — кэш выключен

	ctx := context.Background()
	p := mustPair(t, "EUR/MXN")
	for i := 0; i < 2; i++ {
		if _, err := c.Rate(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("внутренних вызовов = %d, want 2 (TTL=0 — кэш прозрачен)", got)
	}
}

func TestRate_ErrorsNotCached(t *testing.T) {
	inner := &fakeInner{failures: 1, err: errors.New("provider down"), price: 2}
	c := New(inner, Options{TTL: time.Minute})

	ctx := context.Background()
	p := mustPair(t, "EUR/MXN")
	if _, err := c.Rate(ctx, p); err == nil {
		t.Fatal("первый вызов должен вернуть ошибку")
	}
	r2, err := c.Rate(ctx, p)
	if err != nil {
		t.Fatalf("второй вызов должен пойти к провайдеру и succeed: %v", err)
	}
	if r2.Price != 2 {
		t.Fatalf("price = %v, want 2", r2.Price)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("внутренних вызовов = %d, want 2 (ошибки не кэшируются)", got)
	}
}

func TestRate_CacheMetricsHitMiss(t *testing.T) {
	inner := &fakeInner{price: 1}
	m := metrics.New("test")
	c := New(inner, Options{TTL: time.Minute, Metrics: m})

	ctx := context.Background()
	p := mustPair(t, "EUR/MXN")
	for i := 0; i < 3; i++ {
		if _, err := c.Rate(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	body := string(buf)

	for _, want := range []string{
		`rates_provider_cache_total{result="hit"} 2`,
		`rates_provider_cache_total{result="miss"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("метрики не содержат %q:\n%s", want, body)
		}
	}
}
