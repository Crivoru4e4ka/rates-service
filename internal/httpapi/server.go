// Package httpapi — HTTP API сервиса (JSON).
package httpapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // регистрирует pprof-хендлеры на DefaultServeMux
	"runtime"
	"strings"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/metrics"
)

//go:embed spec/openapi.yaml
var openapiSpec []byte

//go:embed spec/index.html
var swaggerHTML []byte

// Service — интерфейс, необходимый HTTP-слою.
type Service interface {
	CreateUpdateRequest(ctx context.Context, pair, idempotencyKey string) (domain.UpdateRequest, bool, error)
	GetUpdateRequest(ctx context.Context, id string) (domain.UpdateRequest, error)
	GetLatestQuote(ctx context.Context, pair string) (domain.Quote, error)
	Ping(ctx context.Context) error
}

type Server struct {
	service Service
	logger  *slog.Logger
	version string
	metrics *metrics.Metrics
	handler http.Handler
}

// Options — параметры HTTP-сервера.
type Options struct {
	Version string           // версия сборки (ldflags -X main.version)
	Metrics *metrics.Metrics // счётчики; nil — /metrics не поднимается
	Pprof   bool             // поднимать /debug/pprof
}

// New собирает маршруты и оборачивает их middleware.
func New(svc Service, logger *slog.Logger, opts Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{service: svc, logger: logger, version: opts.Version, metrics: opts.Metrics}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusFound)
	})
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("POST /quotes", s.handleCreateUpdate)
	mux.HandleFunc("GET /quotes/requests/{id}", s.handleGetRequest)
	mux.HandleFunc("GET /quotes", s.handleGetQuote)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPISpec)
	mux.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /swagger/", s.handleSwaggerUI)
	if opts.Metrics != nil {
		mux.Handle("GET /metrics", opts.Metrics.Handler())
	}
	if opts.Pprof {
		// pprof-хендлеры зарегистрированы на DefaultServeMux blank-импортом.
		mux.Handle("GET /debug/pprof/", http.DefaultServeMux)
	}

	s.handler = withRecovery(withRequestLog(withRequestID(mux), logger, opts.Metrics), logger)
	return s
}

// Handler возвращает готовый http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ---- DTO и ответы ----

type createUpdateRequestBody struct {
	Pair string `json:"pair"`
}

type quoteResponse struct {
	Pair      string    `json:"pair"`
	Price     float64   `json:"price"`
	UpdatedAt time.Time `json:"updated_at"`
}

type updateRequestResponse struct {
	ID        string    `json:"id"`
	Pair      string    `json:"pair"`
	Status    string    `json:"status"`
	Price     *float64  `json:"price"`
	Error     *string   `json:"error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type versionResponse struct {
	Version string `json:"version"`
	Go      string `json:"go"`
}

type errorResponse struct {
	Error apiError `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: apiError{Code: code, Message: message}})
}

// ---- обработчики ----

// handleCreateUpdate — POST /quotes: асинхронный запрос на обновление котировки.
func (s *Server) handleCreateUpdate(w http.ResponseWriter, r *http.Request) {
	var body createUpdateRequestBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "тело запроса должно быть корректным JSON")
		return
	}
	if strings.TrimSpace(body.Pair) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "поле pair обязательно")
		return
	}

	req, created, err := s.service.CreateUpdateRequest(r.Context(), body.Pair, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeServiceError(w, err)
		return
	}

	status := http.StatusAccepted
	if !created {
		status = http.StatusOK // вернулся существующий запрос (идемпотентность)
	} else {
		// REST-удобство: клиент сразу получает адрес статуса запроса.
		w.Header().Set("Location", "/quotes/requests/"+req.ID)
	}
	writeJSON(w, status, updateRequestResponse{
		ID:        req.ID,
		Pair:      req.Pair.String(),
		Status:    string(req.Status),
		Price:     req.Price,
		Error:     req.Error,
		CreatedAt: req.CreatedAt,
		UpdatedAt: req.UpdatedAt,
	})
}

// handleGetRequest — GET /quotes/requests/{id}: статус запроса на обновление.
func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.service.GetUpdateRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updateRequestResponse{
		ID:        req.ID,
		Pair:      req.Pair.String(),
		Status:    string(req.Status),
		Price:     req.Price,
		Error:     req.Error,
		CreatedAt: req.CreatedAt,
		UpdatedAt: req.UpdatedAt,
	})
}

// handleGetQuote — GET /quotes?pair=BASE/QUOTE: последнее значение котировки.
func (s *Server) handleGetQuote(w http.ResponseWriter, r *http.Request) {
	pairStr := r.URL.Query().Get("pair")
	if strings.TrimSpace(pairStr) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"обязательный query-параметр pair (например pair=EUR/MXN)")
		return
	}

	quote, err := s.service.GetLatestQuote(r.Context(), pairStr)
	if err != nil {
		s.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, quoteResponse{
		Pair:      quote.Pair.String(),
		Price:     quote.Price,
		UpdatedAt: quote.UpdatedAt,
	})
}

// handleHealth — GET /healthz: пинг БД.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.service.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy", "БД недоступна")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVersion — GET /version: информация о сборке.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, versionResponse{
		Version: s.version,
		Go:      runtime.Version(),
	})
}

// handleOpenAPISpec — GET /openapi.yaml.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(openapiSpec)
}

// handleSwaggerUI — GET /swagger/: Swagger UI (спека берётся с /openapi.yaml).
func (s *Server) handleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(swaggerHTML)
}

func (s *Server) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidPair):
		writeError(w, http.StatusBadRequest, "invalid_pair", err.Error())
	case errors.Is(err, domain.ErrUnsupportedPair):
		writeError(w, http.StatusUnprocessableEntity, "unsupported_pair", err.Error())
	case errors.Is(err, domain.ErrInvalidID):
		writeError(w, http.StatusBadRequest, "invalid_id", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		s.logger.Error("внутренняя ошибка", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
	}
}
