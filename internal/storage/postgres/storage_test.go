package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"plata-rates/internal/domain"
)

// newTestStorage подключается к БД из TEST_DATABASE_URL.
// Переменная не задана — интеграционные тесты пропускаются.
// Схема создаётся через Migrate, таблицы чистятся между тестами.
func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан — интеграционный тест пропущен")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("подключиться к БД: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("пинг БД: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE update_requests, quotes`); err != nil {
		t.Fatalf("очистить таблицы: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `TRUNCATE update_requests, quotes`)
	})
	return New(pool)
}

func mustPairT(t *testing.T, s string) domain.Pair {
	t.Helper()
	p, err := domain.ParsePair(s)
	if err != nil {
		t.Fatalf("ParsePair(%q): %v", s, err)
	}
	return p
}

func TestStorage_CreateAndPendingDedupe(t *testing.T) {
	st := newTestStorage(t)
	ctx := context.Background()

	req1, created1, err := st.CreateUpdateRequest(ctx, mustPairT(t, "EUR/MXN"), "")
	if err != nil {
		t.Fatal(err)
	}
	if !created1 || req1.Status != domain.StatusPending {
		t.Fatalf("created=%v status=%v", created1, req1.Status)
	}

	req2, created2, err := st.CreateUpdateRequest(ctx, mustPairT(t, "EUR/MXN"), "")
	if err != nil {
		t.Fatal(err)
	}
	if created2 || req2.ID != req1.ID {
		t.Fatalf("дедупликация pending нарушена: created=%v ids %s/%s", created2, req1.ID, req2.ID)
	}

	if _, created3, err := st.CreateUpdateRequest(ctx, mustPairT(t, "USD/MXN"), ""); err != nil || !created3 {
		t.Fatalf("другая пара должна создать новый запрос: created=%v err=%v", created3, err)
	}
}

func TestStorage_IdempotencyKey(t *testing.T) {
	st := newTestStorage(t)
	ctx := context.Background()

	req1, _, err := st.CreateUpdateRequest(ctx, mustPairT(t, "EUR/MXN"), "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteUpdateRequest(ctx, req1.ID, 19.6, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	req2, created2, err := st.CreateUpdateRequest(ctx, mustPairT(t, "EUR/MXN"), "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if created2 || req2.ID != req1.ID {
		t.Fatalf("идемпотентность по ключу нарушена: created=%v ids %s/%s", created2, req1.ID, req2.ID)
	}

	if _, created3, err := st.CreateUpdateRequest(ctx, mustPairT(t, "EUR/MXN"), ""); err != nil || !created3 {
		t.Fatalf("после completed (без ключа) должен создаваться новый запрос: created=%v err=%v", created3, err)
	}
}

func TestStorage_CompleteAndQuote(t *testing.T) {
	st := newTestStorage(t)
	ctx := context.Background()

	req, _, err := st.CreateUpdateRequest(ctx, mustPairT(t, "USD/MXN"), "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.UpsertQuote(ctx, domain.Quote{Pair: req.Pair, Price: 18.5, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteUpdateRequest(ctx, req.ID, 18.5, now); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetUpdateRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusCompleted || got.Price == nil || *got.Price != 18.5 {
		t.Fatalf("unexpected request: %+v", got)
	}

	q, err := st.GetQuote(ctx, req.Pair)
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != 18.5 || !q.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected quote: %+v", q)
	}

	// Повторный апсерт обновляет значение.
	now2 := now.Add(time.Minute)
	if err := st.UpsertQuote(ctx, domain.Quote{Pair: req.Pair, Price: 18.7, UpdatedAt: now2}); err != nil {
		t.Fatal(err)
	}
	q2, err := st.GetQuote(ctx, req.Pair)
	if err != nil {
		t.Fatal(err)
	}
	if q2.Price != 18.7 || !q2.UpdatedAt.Equal(now2) {
		t.Fatalf("unexpected quote после апсерта: %+v", q2)
	}
}

func TestStorage_FailRequest(t *testing.T) {
	st := newTestStorage(t)
	ctx := context.Background()

	req, _, err := st.CreateUpdateRequest(ctx, mustPairT(t, "EUR/USD"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FailUpdateRequest(ctx, req.ID, "provider down"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUpdateRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusFailed || got.Error == nil || *got.Error != "provider down" {
		t.Fatalf("unexpected request: %+v", got)
	}
}

func TestStorage_NotFound(t *testing.T) {
	st := newTestStorage(t)
	ctx := context.Background()

	if _, err := st.GetQuote(ctx, mustPairT(t, "EUR/MXN")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetUpdateRequest(ctx, "5f0c9e2a-1b7c-4a5e-9d3f-2c6b8a1e4d7f"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStorage_ListPending(t *testing.T) {
	st := newTestStorage(t)
	ctx := context.Background()

	pairs := []string{"USD/MXN", "EUR/MXN", "EUR/USD"}
	for _, p := range pairs {
		if _, _, err := st.CreateUpdateRequest(ctx, mustPairT(t, p), ""); err != nil {
			t.Fatal(err)
		}
	}
	reqs, err := st.ListPendingUpdateRequests(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != len(pairs) {
		t.Fatalf("pending = %d, want %d", len(reqs), len(pairs))
	}
}
