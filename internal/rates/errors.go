package rates

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ErrTransient — маркер транзистентной ошибки провайдера (сеть, 429/403/5xx).
// Такие ошибки имеет смысл повторять с backoff; остальные считаются
// постоянными (некорректный ключ, битый формат ответа и т.п.).
var ErrTransient = errors.New("transient provider error")

// TransientError — транзистентная ошибка с опциональным значением Retry-After.
type TransientError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Transient помечает ошибку как транзистентную (подходит для повтора).
func Transient(err error, retryAfter time.Duration) error {
	return &TransientError{Err: fmt.Errorf("%w: %w", ErrTransient, err), RetryAfter: retryAfter}
}

// IsRetryable сообщает, имеет ли смысл повторить запрос.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTransient)
}

// RetryAfterFrom достаёт значение Retry-After из цепочки ошибок (если есть).
func RetryAfterFrom(err error) time.Duration {
	var te *TransientError
	if errors.As(err, &te) && te.RetryAfter > 0 {
		return te.RetryAfter
	}
	return 0
}

// RetryAfterFromHeader разбирает HTTP-заголовок Retry-After
// (секунды или HTTP-дата). Возвращает 0, если заголовка нет или он некорректен.
func RetryAfterFromHeader(h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
