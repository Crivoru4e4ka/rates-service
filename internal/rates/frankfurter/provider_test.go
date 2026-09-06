package frankfurter

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

const okResponse = `{"amount":1.0,"base":"USD","date":"2026-09-04","rates":{"MXN":16.8872}}`

func newTestProvider(t *testing.T, status int, body string, captured map[string]string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			q := r.URL.Query()
			captured["base"] = q.Get("base")
			captured["symbols"] = q.Get("symbols")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, 5*time.Second, nil)
}

func mustPair(t *testing.T, s string) domain.Pair {
	t.Helper()
	p, err := domain.ParsePair(s)
	if err != nil {
		t.Fatalf("ParsePair(%q): %v", s, err)
	}
	return p
}

func TestRate_DirectBase(t *testing.T) {
	captured := map[string]string{}
	prov := newTestProvider(t, http.StatusOK, okResponse, captured)

	rate, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN"))
	if err != nil {
		t.Fatalf("Rate(): %v", err)
	}
	if math.Abs(rate.Price-16.8872) > 1e-9 {
		t.Fatalf("price = %v, want 16.8872", rate.Price)
	}
	if rate.Source != SourceName {
		t.Fatalf("source = %q", rate.Source)
	}
	if captured["base"] != "USD" || captured["symbols"] != "MXN" {
		t.Fatalf("query: base=%q symbols=%q", captured["base"], captured["symbols"])
	}
}

func TestRate_AnchorFromResponse(t *testing.T) {
	// API вернул иную базу, чем запрашивали — кросс-курс через CrossPrice.
	body := `{"base":"EUR","rates":{"USD":1.1,"MXN":19.6}}`
	prov := newTestProvider(t, http.StatusOK, body, nil)

	rate, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN"))
	if err != nil {
		t.Fatalf("Rate(): %v", err)
	}
	if math.Abs(rate.Price-19.6/1.1) > 1e-9 {
		t.Fatalf("price = %v, want %v", rate.Price, 19.6/1.1)
	}
}

func TestRate_Errors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryable  bool
		wantSubStr string
	}{
		{
			name:       "429 транзистентен",
			status:     http.StatusTooManyRequests,
			body:       `{"message":"rate limit"}`,
			retryable:  true,
			wantSubStr: "неожиданный статус 429",
		},
		{
			name:       "404 постоянная ошибка",
			status:     http.StatusNotFound,
			body:       `{"message":"not found"}`,
			retryable:  false,
			wantSubStr: "неожиданный статус 404",
		},
		{
			name:       "битый json",
			status:     http.StatusOK,
			body:       "{oops",
			retryable:  false,
			wantSubStr: "разобрать JSON",
		},
		{
			name:       "нет курсов",
			status:     http.StatusOK,
			body:       `{"base":"USD","rates":{}}`,
			retryable:  false,
			wantSubStr: "нет курсов",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := newTestProvider(t, tt.status, tt.body, nil)
			_, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN"))
			if err == nil {
				t.Fatal("ожидалась ошибка, получено nil")
			}
			if !strings.Contains(err.Error(), tt.wantSubStr) {
				t.Fatalf("err = %v, want содержит %q", err, tt.wantSubStr)
			}
			if rates.IsRetryable(err) != tt.retryable {
				t.Fatalf("IsRetryable = %v, want %v", rates.IsRetryable(err), tt.retryable)
			}
		})
	}
}

func TestRate_ContextCanceled(t *testing.T) {
	prov := newTestProvider(t, http.StatusOK, okResponse, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prov.Rate(ctx, mustPair(t, "USD/MXN")); err == nil {
		t.Fatal("ожидалась ошибка отмены контекста")
	}
}

func TestNew_InvalidBaseURL(t *testing.T) {
	prov := New("::not-a-url::", time.Second, nil)
	if _, err := prov.Rate(context.Background(), mustPair(t, "USD/MXN")); err == nil {
		t.Fatal("ожидалась ошибка конфигурации")
	}
}
