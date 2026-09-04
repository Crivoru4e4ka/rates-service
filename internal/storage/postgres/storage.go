// Package postgres — реализация хранилища сервиса на PostgreSQL (pgx).
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"plata-rates/internal/domain"
)

type Storage struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Storage {
	return &Storage{pool: pool}
}

// Ping проверяет доступность БД (для /healthz).
func (s *Storage) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

const requestColumns = `id::text, pair, status, price::float8, error, created_at, updated_at`

// CreateUpdateRequest создаёт запрос на обновление котировки.
//
// Идемпотентность:
//  1. если передан непустой idempotencyKey и запрос с таким ключом уже есть — возвращается он;
//  2. если у пары уже есть запрос в статусе pending — возвращается он (дубликат не создаётся);
//  3. иначе создаётся новый запрос.
//
// created=false означает, что вернулся уже существующий запрос.
func (s *Storage) CreateUpdateRequest(ctx context.Context, pair domain.Pair, idempotencyKey string) (domain.UpdateRequest, bool, error) {
	if idempotencyKey != "" {
		req, found, err := s.requestByKey(ctx, idempotencyKey)
		if err != nil {
			return domain.UpdateRequest{}, false, err
		}
		if found {
			return req, false, nil
		}
	}

	if req, found, err := s.pendingRequestByPair(ctx, pair.String()); err != nil {
		return domain.UpdateRequest{}, false, err
	} else if found {
		return req, false, nil
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO update_requests (pair, idempotency_key)
		VALUES ($1, NULLIF($2, ''))
		RETURNING `+requestColumns,
		pair.String(), idempotencyKey,
	)
	req, err := scanRequest(row)
	if err != nil {
		// Гонка с параллельным запросом (нарушение unique-индексов):
		// повторно ищем существующий запрос.
		if isUniqueViolation(err) {
			if r, found, ferr := s.pendingRequestByPair(ctx, pair.String()); ferr == nil && found {
				return r, false, nil
			}
			if idempotencyKey != "" {
				if r, found, ferr := s.requestByKey(ctx, idempotencyKey); ferr == nil && found {
					return r, false, nil
				}
			}
		}
		return domain.UpdateRequest{}, false, fmt.Errorf("insert update request: %w", err)
	}
	return req, true, nil
}

func (s *Storage) GetUpdateRequest(ctx context.Context, id string) (domain.UpdateRequest, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM update_requests WHERE id = $1`, id,
	)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.UpdateRequest{}, fmt.Errorf("%w: update request %s", domain.ErrNotFound, id)
		}
		return domain.UpdateRequest{}, fmt.Errorf("get update request: %w", err)
	}
	return req, nil
}

func (s *Storage) ListPendingUpdateRequests(ctx context.Context, limit int) ([]domain.UpdateRequest, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+requestColumns+` FROM update_requests
		WHERE status = 'pending'
		ORDER BY created_at
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending update requests: %w", err)
	}
	defer rows.Close()

	var out []domain.UpdateRequest
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending update request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending update requests: %w", err)
	}
	return out, nil
}

func (s *Storage) CompleteUpdateRequest(ctx context.Context, id string, price float64, completedAt time.Time) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE update_requests
		SET status = 'completed', price = $2, error = NULL, updated_at = $3
		WHERE id = $1 AND status = 'pending'
	`, id, price, completedAt)
	if err != nil {
		return fmt.Errorf("complete update request: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// Уже завершён (повторная обработка из очереди) — не ошибка.
		return nil
	}
	return nil
}

func (s *Storage) FailUpdateRequest(ctx context.Context, id string, reason string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE update_requests
		SET status = 'failed', error = $2, updated_at = now()
		WHERE id = $1 AND status = 'pending'
	`, id, truncate(reason, 500))
	if err != nil {
		return fmt.Errorf("fail update request: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	return nil
}

func (s *Storage) UpsertQuote(ctx context.Context, quote domain.Quote) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO quotes (pair, price, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (pair) DO UPDATE
		SET price = EXCLUDED.price, updated_at = EXCLUDED.updated_at
	`, quote.Pair.String(), quote.Price, quote.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert quote: %w", err)
	}
	return nil
}

func (s *Storage) GetQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error) {
	var (
		pairStr string
		price   float64
		updated time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT pair, price::float8, updated_at FROM quotes WHERE pair = $1`, pair.String(),
	).Scan(&pairStr, &price, &updated)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Quote{}, fmt.Errorf("%w: quote %s", domain.ErrNotFound, pair.String())
		}
		return domain.Quote{}, fmt.Errorf("get quote: %w", err)
	}
	p, perr := domain.ParsePair(pairStr)
	if perr != nil {
		return domain.Quote{}, fmt.Errorf("пара %q в БД повреждена: %w", pairStr, perr)
	}
	return domain.Quote{Pair: p, Price: price, UpdatedAt: updated}, nil
}

func (s *Storage) requestByKey(ctx context.Context, key string) (domain.UpdateRequest, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM update_requests WHERE idempotency_key = $1`, key,
	)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.UpdateRequest{}, false, nil
		}
		return domain.UpdateRequest{}, false, fmt.Errorf("find update request by idempotency key: %w", err)
	}
	return req, true, nil
}

func (s *Storage) pendingRequestByPair(ctx context.Context, pair string) (domain.UpdateRequest, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM update_requests WHERE pair = $1 AND status = 'pending'`, pair,
	)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.UpdateRequest{}, false, nil
		}
		return domain.UpdateRequest{}, false, fmt.Errorf("find pending update request: %w", err)
	}
	return req, true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequest(row rowScanner) (domain.UpdateRequest, error) {
	var (
		req    domain.UpdateRequest
		pair   string
		status string
		price  sql.NullFloat64
		errMsg sql.NullString
	)
	if err := row.Scan(&req.ID, &pair, &status, &price, &errMsg, &req.CreatedAt, &req.UpdatedAt); err != nil {
		return domain.UpdateRequest{}, err
	}
	p, err := domain.ParsePair(pair)
	if err != nil {
		return domain.UpdateRequest{}, fmt.Errorf("пара %q в БД повреждена: %w", pair, err)
	}
	req.Pair = p
	req.Status = domain.RequestStatus(status)
	if price.Valid {
		v := price.Float64
		req.Price = &v
	}
	if errMsg.Valid {
		v := errMsg.String
		req.Error = &v
	}
	return req, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
