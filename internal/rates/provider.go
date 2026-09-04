package rates

import (
	"context"
	"time"

	"plata-rates/internal/domain"
)

// Rate — результат обращения к внешнему источнику котировок.
type Rate struct {
	Pair      domain.Pair
	Price     float64
	FetchedAt time.Time
	Source    string
}

// Provider — внешний источник цен.
type Provider interface {
	Rate(ctx context.Context, pair domain.Pair) (Rate, error)
}
