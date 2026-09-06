package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"plata-rates/internal/metrics"
)

type ctxKeyRequestID struct{}

// statusRecorder запоминает статус ответа для логирования и метрик.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withRequestID проставляет X-Request-ID (из заголовка клиента или новый)
// и кладёт его в контекст для корреляции в логах.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyRequestID{}).(string); ok {
		return v
	}
	return ""
}

// routeLabel — значение label "route" для метрик. Ограничивает кардинальность:
// реальные ID в пути сворачиваются в шаблон, незнакомые пути — в "unmatched".
func routeLabel(path string) string {
	switch {
	case strings.HasPrefix(path, "/quotes/requests/"):
		return "/quotes/requests/{id}"
	case path == "/" || path == "/quotes" || path == "/healthz" || path == "/version" ||
		path == "/metrics" || path == "/openapi.yaml" || path == "/swagger" || path == "/swagger/":
		return path
	case strings.HasPrefix(path, "/debug/pprof"):
		return "/debug/pprof"
	default:
		return "unmatched"
	}
}

// withRequestLog логирует каждый HTTP-запрос и обновляет метрики.
func withRequestLog(next http.Handler, logger *slog.Logger, m *metrics.Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := routeLabel(r.URL.Path)
		m.ObserveHTTP(r.Method, route, strconv.Itoa(rec.status))

		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"route", route,
			"status", rec.status,
			"duration", time.Since(start).String(),
			"remote", r.RemoteAddr,
			"request_id", requestID(r),
		)
	})
}

// withRecovery защищает сервер от паник в обработчиках.
func withRecovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic восстановлен",
					"err", rec,
					"stack", string(debug.Stack()),
				)
				writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
