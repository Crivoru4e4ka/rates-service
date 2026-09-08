package fallback

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeP struct {
	calls  atomic.Int32
	err    error
	price  float64
	source string
}

func (f *fakeP) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	f.calls.Add(1)
	if f.err != nil {
		return rates.Rate{}, f.err
	}
	return rates.Rate{Pair: pair, Price: f.price, FetchedAt: time.Now().UTC(), Source: f.source}, nil
}

func mustPair(t *testing.T, s string) domain.Pair {
	t.Helper()
	p, err := domain.ParsePair(s)
	if err != nil {
		t.Fatalf("ParsePair(%q): %v", s, err)
	}
	return p
}

func TestPrimaryOK_SecondaryNotCalled(t *testing.T) {
	prim := &fakeP{price: 19.5, source: "primary"}
	sec := &fakeP{err: errors.New("secondary must not be called")}
	f := New(prim, sec, Options{
		PrimaryName: "primary", SecondaryName: "secondary",
		Logger: discardLogger(), Metrics: metrics.New("x"),
	})

	r, err := f.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Price != 19.5 || r.Source != "primary" {
		t.Fatalf("unexpected rate: %+v", r)
	}
	if sec.calls.Load() != 0 {
		t.Fatalf("secondary вызван при успехе primary: %d", sec.calls.Load())
	}
}

func TestFallbackServes_SecondaryUsed(t *testing.T) {
	prim := &fakeP{err: errors.New("primary down")}
	sec := &fakeP{price: 19.8, source: "secondary"}
	m := metrics.New("test")
	f := New(prim, sec, Options{
		PrimaryName: "primary", SecondaryName: "secondary",
		Logger: discardLogger(), Metrics: m,
	})

	r, err := f.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Price != 19.8 || r.Source != "secondary" {
		t.Fatalf("unexpected rate: %+v", r)
	}
	if prim.calls.Load() != 1 || sec.calls.Load() != 1 {
		t.Fatalf("calls: primary=%d secondary=%d", prim.calls.Load(), sec.calls.Load())
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
	if !strings.Contains(string(buf), `rates_fallback_total{provider="secondary"} 1`) {
		t.Fatalf("метрики не содержат фолбэк:\n%s", buf)
	}
}

func TestBothFail_ReturnsPrimaryError(t *testing.T) {
	prim := &fakeP{err: errors.New("primary down")}
	sec := &fakeP{err: errors.New("secondary down")}
	f := New(prim, sec, Options{
		PrimaryName: "primary", SecondaryName: "secondary", Logger: discardLogger(),
	})

	_, err := f.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if err.Error() != "primary down" {
		t.Fatalf("err = %v, want ошибка основного", err)
	}
	if prim.calls.Load() != 1 || sec.calls.Load() != 1 {
		t.Fatalf("calls: primary=%d secondary=%d", prim.calls.Load(), sec.calls.Load())
	}
}

func TestNoSecondary_Passthrough(t *testing.T) {
	prim := &fakeP{err: errors.New("primary down")}
	f := New(prim, nil, Options{PrimaryName: "primary", Logger: discardLogger()})

	_, err := f.Rate(context.Background(), mustPair(t, "EUR/MXN"))
	if err == nil || err.Error() != "primary down" {
		t.Fatalf("err = %v, want ошибка primary", err)
	}
}

func TestContextCanceled_NoFallback(t *testing.T) {
	prim := &fakeP{err: context.Canceled}
	sec := &fakeP{price: 1}
	f := New(prim, sec, Options{PrimaryName: "primary", SecondaryName: "secondary", Logger: discardLogger()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Rate(ctx, mustPair(t, "EUR/MXN")); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if sec.calls.Load() != 0 {
		t.Fatal("фолбэк не должен выполняться при исчерпанном ctx")
	}
}
