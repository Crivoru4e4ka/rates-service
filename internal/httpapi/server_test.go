package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plata-rates/internal/domain"
)

// ---- фейковый сервис ----

type fakeService struct {
	createFn   func(ctx context.Context, pair, key string) (domain.UpdateRequest, bool, error)
	getReqFn   func(ctx context.Context, id string) (domain.UpdateRequest, error)
	getQuoteFn func(ctx context.Context, pair string) (domain.Quote, error)
	pingErr    error
	lastKey    string
}

func (f *fakeService) CreateUpdateRequest(ctx context.Context, pair, key string) (domain.UpdateRequest, bool, error) {
	f.lastKey = key
	if f.createFn != nil {
		return f.createFn(ctx, pair, key)
	}
	return domain.UpdateRequest{}, false, nil
}

func (f *fakeService) GetUpdateRequest(ctx context.Context, id string) (domain.UpdateRequest, error) {
	if f.getReqFn != nil {
		return f.getReqFn(ctx, id)
	}
	return domain.UpdateRequest{}, nil
}

func (f *fakeService) GetLatestQuote(ctx context.Context, pair string) (domain.Quote, error) {
	if f.getQuoteFn != nil {
		return f.getQuoteFn(ctx, pair)
	}
	return domain.Quote{}, nil
}

func (f *fakeService) Ping(ctx context.Context) error { return f.pingErr }

func testServer(t *testing.T, svc Service) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(svc, nil, Options{}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func newPending(id, pair string) domain.UpdateRequest {
	p, err := domain.ParsePair(pair)
	if err != nil {
		panic(err) // в тестах все пары валидны
	}
	return domain.UpdateRequest{
		ID:        id,
		Pair:      p,
		Status:    domain.StatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

func postJSON(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatalf("разобрать JSON: %v", err)
	}
	return m
}

// ---- тесты ----

func TestCreateUpdate_202(t *testing.T) {
	svc := &fakeService{
		createFn: func(_ context.Context, pair, key string) (domain.UpdateRequest, bool, error) {
			return newPending("00000000-0000-0000-0000-000000000001", pair), true, nil
		},
	}
	srv := testServer(t, svc)

	resp := postJSON(t, srv.URL+"/quotes", `{"pair":"EUR/MXN"}`, map[string]string{"Idempotency-Key": "k1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if svc.lastKey != "k1" {
		t.Fatalf("lastKey = %q, want k1", svc.lastKey)
	}

	body := decode(t, resp.Body)
	if body["id"] != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("id = %v", body["id"])
	}
	if body["pair"] != "EUR/MXN" || body["status"] != "pending" {
		t.Fatalf("body = %v", body)
	}
}

func TestCreateUpdate_Existing_200(t *testing.T) {
	svc := &fakeService{
		createFn: func(_ context.Context, pair, key string) (domain.UpdateRequest, bool, error) {
			return newPending("00000000-0000-0000-0000-000000000002", pair), false, nil
		},
	}
	srv := testServer(t, svc)

	resp := postJSON(t, srv.URL+"/quotes", `{"pair":"EUR/MXN"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCreateUpdate_BadRequests(t *testing.T) {
	svc := &fakeService{}
	srv := testServer(t, svc)

	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "битый json", body: "{oops", status: 400, code: "invalid_json"},
		{name: "пустая пара", body: `{"pair":""}`, status: 400, code: "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := postJSON(t, srv.URL+"/quotes", tt.body, nil)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			body := decode(t, resp.Body)
			if body["error"].(map[string]any)["code"] != tt.code {
				t.Fatalf("code = %v", body["error"])
			}
		})
	}
}

func TestCreateUpdate_UnsupportedPair_422(t *testing.T) {
	svc := &fakeService{
		createFn: func(_ context.Context, pair, key string) (domain.UpdateRequest, bool, error) {
			return domain.UpdateRequest{}, false,
				fmt.Errorf("%w: %s", domain.ErrUnsupportedPair, pair)
		},
	}
	srv := testServer(t, svc)

	resp := postJSON(t, srv.URL+"/quotes", `{"pair":"EUR/GBP"}`, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	body := decode(t, resp.Body)
	if body["error"].(map[string]any)["code"] != "unsupported_pair" {
		t.Fatalf("code = %v", body["error"])
	}
}

func TestGetRequest(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		setup  func(ctx context.Context, id string) (domain.UpdateRequest, error)
		status int
	}{
		{
			name: "найден",
			id:   "00000000-0000-0000-0000-000000000003",
			setup: func(_ context.Context, id string) (domain.UpdateRequest, error) {
				req := newPending(id, "EUR/MXN")
				price := 19.62
				req.Status = domain.StatusCompleted
				req.Price = &price
				return req, nil
			},
			status: 200,
		},
		{
			name: "неверный id",
			id:   "abc",
			setup: func(_ context.Context, _ string) (domain.UpdateRequest, error) {
				return domain.UpdateRequest{}, domain.ErrInvalidID
			},
			status: 400,
		},
		{
			name: "не найден",
			id:   "00000000-0000-0000-0000-000000000004",
			setup: func(_ context.Context, _ string) (domain.UpdateRequest, error) {
				return domain.UpdateRequest{}, domain.ErrNotFound
			},
			status: 404,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeService{getReqFn: tt.setup}
			srv := testServer(t, svc)

			resp, err := http.Get(srv.URL + "/quotes/requests/" + tt.id)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if tt.status == http.StatusOK {
				body := decode(t, resp.Body)
				if body["status"] != "completed" || body["price"].(float64) != 19.62 {
					t.Fatalf("body = %v", body)
				}
			}
		})
	}
}

func TestGetQuote(t *testing.T) {
	svc := &fakeService{
		getQuoteFn: func(_ context.Context, pair string) (domain.Quote, error) {
			p, err := domain.ParsePair(pair)
			if err != nil {
				return domain.Quote{}, err
			}
			return domain.Quote{Pair: p, Price: 19.62, UpdatedAt: time.Now().UTC()}, nil
		},
	}
	srv := testServer(t, svc)

	resp, err := http.Get(srv.URL + "/quotes?pair=EUR/MXN")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := decode(t, resp.Body)
	if body["pair"] != "EUR/MXN" || body["price"].(float64) != 19.62 {
		t.Fatalf("body = %v", body)
	}
}

func TestGetQuote_MissingParam(t *testing.T) {
	srv := testServer(t, &fakeService{})
	resp, err := http.Get(srv.URL + "/quotes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetQuote_NotFound(t *testing.T) {
	svc := &fakeService{
		getQuoteFn: func(_ context.Context, _ string) (domain.Quote, error) {
			return domain.Quote{}, fmt.Errorf("%w: quote", domain.ErrNotFound)
		},
	}
	srv := testServer(t, svc)
	resp, err := http.Get(srv.URL + "/quotes?pair=USD/MXN")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		srv := testServer(t, &fakeService{})
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
	t.Run("db down", func(t *testing.T) {
		srv := testServer(t, &fakeService{pingErr: errors.New("db down")})
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	})
}

func TestOpenAPISpec_Served(t *testing.T) {
	srv := testServer(t, &fakeService{})
	resp, err := http.Get(srv.URL + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "openapi: 3.0.3") {
		t.Fatal("спека не содержит openapi: 3.0.3")
	}
	if !strings.Contains(string(data), "/quotes/requests/{id}") {
		t.Fatal("спека не содержит путь /quotes/requests/{id}")
	}
}

func TestSwaggerUI_Served(t *testing.T) {
	srv := testServer(t, &fakeService{})
	resp, err := http.Get(srv.URL + "/swagger/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "SwaggerUIBundle") {
		t.Fatal("страница Swagger UI не содержит SwaggerUIBundle")
	}
}
