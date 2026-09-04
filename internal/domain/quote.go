package domain

import "time"

// Quote — последняя сохранённая котировка пары.
type Quote struct {
	Pair      Pair
	Price     float64
	UpdatedAt time.Time
}

// RequestStatus — статус запроса на обновление котировки.
type RequestStatus string

const (
	StatusPending   RequestStatus = "pending"
	StatusCompleted RequestStatus = "completed"
	StatusFailed    RequestStatus = "failed"
)

// UpdateRequest — запрос на фоновое обновление котировки.
type UpdateRequest struct {
	ID        string
	Pair      Pair
	Status    RequestStatus
	Price     *float64 // заполнено при status=completed
	Error     *string  // заполнено при status=failed
	CreatedAt time.Time
	UpdatedAt time.Time
}
