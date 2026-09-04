package exchangeratesapi

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plata-rates/internal/domain"
)

const okResponse = `{"success":true,"timestamp":1788555905,"base":"EUR","date":"2026-09-04","rates":{"USD":1.161705,"MXN":19.617941}}`

func newTestProvider(t *testing.T, status int, body string, captured map[string]string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			q := r.URL.Query()
			captured["access_key"] = q.Get("access_key")
			captured["base"] = q.Get("base")
			captured["symbols"] = q.Get("symbols")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-key", 5*time.Second, nil)
}

func mustPair(t *testing.T, s string) domain.Pair {
	t.Helper()
	p, err := domain.ParsePair(s)
	if err != nil {
		t.Fatalf("ParsePair(%q): %v", s, err)
	}
	return p
}

func TestRate_CrossPrices(t *testing.T) {
	tests := []struct {
		name string
		pair string
		want float64
	}{
		{name: "EUR base — прямой курс", pair: "EUR/MXN", want: 19.617941},
		{name: "EUR quote — обратный курс", pair: "USD/EUR", want: 1 / 1.161705},
		{name: "кросс-курс", pair: "USD/MXN", want: 19.617941 / 1.161705},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := newTestProvider(t, http.StatusOK, okResponse, nil)
			rate, err := prov.Rate(context.Background(), mustPair(t, tt.pair))
			if err != nil {
				t.Fatalf("Rate(): %v", err)
			}
			if math.Abs(rate.Price-tt.want) > 1e-9 {
				t.Fatalf("price = %v, want %v", rate.Price, tt.want)
			}
			if rate.Source != SourceName {
				t.Fatalf("source = %q", rate.Source)
			}
		})
	}
}

func TestRate_QueryParams(t *testing.T) {
	captured := map[string]string{}
	prov := newTestProvider(t, http.StatusOK, okResponse, captured)
	if _, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN")); err != nil {
		t.Fatalf("Rate(): %v", err)
	}
	if captured["access_key"] != "test-key" {
		t.Fatalf("access_key = %q", captured["access_key"])
	}
	if captured["base"] != "EUR" {
		t.Fatalf("base = %q", captured["base"])
	}
	if captured["symbols"] != "MXN,USD" {
		t.Fatalf("symbols = %q", captured["symbols"])
	}
}

func TestRate_AnchorFromResponse(t *testing.T) {
	// Если API проигнорирует base=EUR и вернёт курсы против другой валюты —
	// математика кросс-курса не должна ломаться.
	body := `{"success":true,"base":"USD","rates":{"EUR":0.9,"MXN":18.0}}`
	prov := newTestProvider(t, http.StatusOK, body, nil)
	rate, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN"))
	if err != nil {
		t.Fatalf("Rate(): %v", err)
	}
	if math.Abs(rate.Price-18.0) > 1e-9 {
		t.Fatalf("price = %v, want 18", rate.Price)
	}
}

func TestRate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{
			name:    "ошибка API",
			status:  http.StatusOK,
			body:    `{"success":false,"error":{"code":101,"type":"invalid_access_key","info":"You have not supplied a valid API Access Key."}}`,
			wantSub: "invalid_access_key",
		},
		{name: "http 403", status: http.StatusForbidden, body: "forbidden", wantSub: "неожиданный статус 403"},
		{name: "битый json", status: http.StatusOK, body: "{oops", wantSub: "разобрать JSON"},
		{
			name:    "нет нужной валюты",
			status:  http.StatusOK,
			body:    `{"success":true,"base":"EUR","rates":{"MXN":19.6}}`,
			wantSub: "нет курсов для пары USD/MXN",
		},
		{
			name:    "нулевой курс",
			status:  http.StatusOK,
			body:    `{"success":true,"base":"EUR","rates":{"USD":0,"MXN":19.6}}`,
			wantSub: "некорректные курсы",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := newTestProvider(t, tt.status, tt.body, nil)
			_, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN"))
			if err == nil {
				t.Fatal("ожидалась ошибка, получено nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %v, want содержит %q", err, tt.wantSub)
			}
		})
	}
}

func TestRate_ContextCanceled(t *testing.T) {
	prov := newTestProvider(t, http.StatusOK, okResponse, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prov.Rate(ctx, mustPair(t, "EUR/MXN")); err == nil {
		t.Fatal("ожидалась ошибка отмены контекста")
	}
}

func TestNew_InvalidBaseURL(t *testing.T) {
	prov := New("::not-a-url::", "k", time.Second, nil)
	if _, err := prov.Rate(context.Background(), mustPair(t, "EUR/MXN")); err == nil {
		t.Fatal("ожидалась ошибка конфигурации")
	}
}
