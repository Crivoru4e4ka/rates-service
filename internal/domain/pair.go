package domain

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidPair — неверный формат валютной пары.
	ErrInvalidPair = errors.New("invalid currency pair")
	// ErrUnsupportedPair — пара не входит в список поддерживаемых.
	ErrUnsupportedPair = errors.New("unsupported currency pair")
	// ErrNotFound — объект не найден.
	ErrNotFound = errors.New("not found")
	// ErrInvalidID — идентификатор запроса не похож на UUID.
	ErrInvalidID = errors.New("invalid update request id")
)

// Pair — валютная пара вида "EUR/MXN".
type Pair struct {
	Base  string
	Quote string
}

// ParsePair разбирает строку вида "eur/mxn" в пару. Регистр игнорируется.
func ParsePair(s string) (Pair, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return Pair{}, fmt.Errorf("%w: ожидается формат BASE/QUOTE, получено %q", ErrInvalidPair, s)
	}
	base, quote := parts[0], parts[1]
	if !isCurrencyCode(base) || !isCurrencyCode(quote) {
		return Pair{}, fmt.Errorf("%w: коды валют должны состоять из 3 латинских букв, получено %q", ErrInvalidPair, s)
	}
	if base == quote {
		return Pair{}, fmt.Errorf("%w: base и quote не должны совпадать, получено %q", ErrInvalidPair, s)
	}
	return Pair{Base: base, Quote: quote}, nil
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func (p Pair) String() string { return p.Base + "/" + p.Quote }

// ValidRequestID проверяет, что id — каноническая строка вида UUID.
func ValidRequestID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}
