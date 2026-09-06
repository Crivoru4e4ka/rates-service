package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

// ---- фейки ----

type fakeRepo struct {
	mu       sync.Mutex
	next     int
	requests map[string]domain.UpdateRequest
	byKey    map[string]string
	pending  map[string]string // pair -> id
	quotes   map[string]domain.Quote
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		requests: map[string]domain.UpdateRequest{},
		byKey:    map[string]string{},
		pending:  map[string]string{},
		quotes:   map[string]domain.Quote{},
	}
}

func (f *fakeRepo) Ping(ctx context.Context) error { return nil }

func (f *fakeRepo) CreateUpdateRequest(_ context.Context, pair domain.Pair, key string) (domain.UpdateRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key != "" {
		if id, ok := f.byKey[key]; ok {
			return f.requests[id], false, nil
		}
	}
	if id, ok := f.pending[pair.String()]; ok {
		return f.requests[id], false, nil
	}
	f.next++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.next)
	req := domain.UpdateRequest{
		ID:        id,
		Pair:      pair,
		Status:    domain.StatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	f.requests[id] = req
	f.pending[pair.String()] = id
	if key != "" {
		f.byKey[key] = id
	}
	return req, true, nil
}

func (f *fakeRepo) GetUpdateRequest(_ context.Context, id string) (domain.UpdateRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.requests[id]
	if !ok {
		return domain.UpdateRequest{}, fmt.Errorf("%w: update request %s", domain.ErrNotFound, id)
	}
	return req, nil
}

func (f *fakeRepo) ListPendingUpdateRequests(_ context.Context, limit int) ([]domain.UpdateRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.UpdateRequest
	for _, r := range f.requests {
		if r.Status == domain.StatusPending {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) CompleteUpdateRequest(_ context.Context, id string, price float64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.requests[id]
	if !ok || req.Status != domain.StatusPending {
		return nil
	}
	req.Status = domain.StatusCompleted
	p := price
	req.Price = &p
	req.UpdatedAt = at
	f.requests[id] = req
	delete(f.pending, req.Pair.String())
	return nil
}

func (f *fakeRepo) FailUpdateRequest(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.requests[id]
	if !ok || req.Status != domain.StatusPending {
		return nil
	}
	req.Status = domain.StatusFailed
	e := reason
	req.Error = &e
	req.UpdatedAt = time.Now().UTC()
	f.requests[id] = req
	delete(f.pending, req.Pair.String())
	return nil
}

func (f *fakeRepo) UpsertQuote(_ context.Context, q domain.Quote) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotes[q.Pair.String()] = q
	return nil
}

func (f *fakeRepo) GetQuote(_ context.Context, pair domain.Pair) (domain.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.quotes[pair.String()]
	if !ok {
		return domain.Quote{}, fmt.Errorf("%w: quote %s", domain.ErrNotFound, pair.String())
	}
	return q, nil
}

type fakeProvider struct {
	mu     sync.Mutex
	prices map[domain.Pair]float64
	err    error
}

func (f *fakeProvider) Rate(_ context.Context, pair domain.Pair) (rates.Rate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return rates.Rate{}, f.err
	}
	price, ok := f.prices[pair]
	if !ok {
		return rates.Rate{}, fmt.Errorf("нет цены для %s", pair.String())
	}
	return rates.Rate{Pair: pair, Price: price, FetchedAt: time.Now().UTC(), Source: "fake"}, nil
}

// ---- помощники ----

var testCurrencies = []string{"USD", "EUR", "MXN"}

func testService(t *testing.T, repo *fakeRepo, prov rates.Provider) *Service {
	t.Helper()
	svc := New(repo, prov, testCurrencies, Options{
		QueueSize:      10,
		Workers:        2,
		RequestTimeout: 2 * time.Second,
		ResyncInterval: time.Hour, // чтобы периодический resync не мешал тестам
	})
	t.Cleanup(func() { svc.Shutdown(context.Background()) })
	return svc
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("условие не выполнено за отведённое время")
}

// ---- тесты ----

func TestCreateUpdateRequest_Completes(t *testing.T) {
	repo := newFakeRepo()
	prov := &fakeProvider{prices: map[domain.Pair]float64{{Base: "EUR", Quote: "MXN"}: 19.62}}
	svc := testService(t, repo, prov)

	req, created, err := svc.CreateUpdateRequest(context.Background(), "EUR/MXN", "")
	if err != nil {
		t.Fatal(err)
	}
	if !created || req.Status != domain.StatusPending {
		t.Fatalf("created=%v status=%v", created, req.Status)
	}

	waitFor(t, 3*time.Second, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.requests[req.ID].Status == domain.StatusCompleted
	})

	repo.mu.Lock()
	defer repo.mu.Unlock()
	r := repo.requests[req.ID]
	q := repo.quotes["EUR/MXN"]
	if r.Price == nil || *r.Price != 19.62 {
		t.Fatalf("request price = %v, want 19.62", r.Price)
	}
	if q.Price != 19.62 {
		t.Fatalf("quote price = %v, want 19.62", q.Price)
	}
}

func TestCreateUpdateRequest_InvalidPair(t *testing.T) {
	svc := testService(t, newFakeRepo(), &fakeProvider{})
	_, _, err := svc.CreateUpdateRequest(context.Background(), "EURMXN", "")
	if !errors.Is(err, domain.ErrInvalidPair) {
		t.Fatalf("err = %v, want ErrInvalidPair", err)
	}
}

func TestCreateUpdateRequest_UnsupportedPair(t *testing.T) {
	svc := testService(t, newFakeRepo(), &fakeProvider{})
	_, _, err := svc.CreateUpdateRequest(context.Background(), "EUR/GBP", "")
	if !errors.Is(err, domain.ErrUnsupportedPair) {
		t.Fatalf("err = %v, want ErrUnsupportedPair", err)
	}
}

func TestCreateUpdateRequest_IdempotencyKey(t *testing.T) {
	repo := newFakeRepo()
	prov := &fakeProvider{prices: map[domain.Pair]float64{{Base: "EUR", Quote: "MXN"}: 19.62}}
	svc := testService(t, repo, prov)
	ctx := context.Background()

	req1, created1, err := svc.CreateUpdateRequest(ctx, "EUR/MXN", "client-key-1")
	if err != nil || !created1 {
		t.Fatalf("created=%v err=%v", created1, err)
	}
	waitFor(t, 3*time.Second, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.requests[req1.ID].Status == domain.StatusCompleted
	})

	// Повторный вызов с тем же ключом возвращает тот же запрос.
	req2, created2, err := svc.CreateUpdateRequest(ctx, "EUR/MXN", "client-key-1")
	if err != nil {
		t.Fatal(err)
	}
	if created2 || req2.ID != req1.ID {
		t.Fatalf("идемпотентность нарушена: created=%v ids %s/%s", created2, req1.ID, req2.ID)
	}
}

func TestCreateUpdateRequest_PendingDedupe(t *testing.T) {
	repo := newFakeRepo()
	svc := testService(t, repo, &fakeProvider{}) // на старте pending нет

	// Сидируем pending напрямую в репозиторий (как «висящий» запрос).
	seed := domain.UpdateRequest{
		ID:        "00000000-0000-0000-0000-000000000001",
		Pair:      domain.Pair{Base: "EUR", Quote: "MXN"},
		Status:    domain.StatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	repo.mu.Lock()
	repo.requests[seed.ID] = seed
	repo.pending["EUR/MXN"] = seed.ID
	repo.mu.Unlock()

	req, created, err := svc.CreateUpdateRequest(context.Background(), "EUR/MXN", "")
	if err != nil {
		t.Fatal(err)
	}
	if created || req.ID != seed.ID {
		t.Fatalf("дедупликация нарушена: created=%v id=%s (want %s)", created, req.ID, seed.ID)
	}
}

func TestProviderFailure_MarksFailed(t *testing.T) {
	repo := newFakeRepo()
	prov := &fakeProvider{err: errors.New("api down")}
	svc := testService(t, repo, prov)

	req, _, err := svc.CreateUpdateRequest(context.Background(), "USD/MXN", "")
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.requests[req.ID].Status == domain.StatusFailed
	})

	repo.mu.Lock()
	defer repo.mu.Unlock()
	r := repo.requests[req.ID]
	if r.Error == nil || !strings.Contains(*r.Error, "api down") {
		t.Fatalf("error = %v, want содержит %q", r.Error, "api down")
	}
}

func TestStartupResync_PicksPending(t *testing.T) {
	repo := newFakeRepo()
	prov := &fakeProvider{prices: map[domain.Pair]float64{{Base: "USD", Quote: "MXN"}: 18.5}}

	// Pending-запрос «с прошлого запуска» — до создания сервиса.
	seed := domain.UpdateRequest{
		ID:        "00000000-0000-0000-0000-000000000009",
		Pair:      domain.Pair{Base: "USD", Quote: "MXN"},
		Status:    domain.StatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	repo.requests[seed.ID] = seed
	repo.pending["USD/MXN"] = seed.ID

	testService(t, repo, prov) // New вызывает resync и ставит запрос в очередь

	waitFor(t, 3*time.Second, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.requests[seed.ID].Status == domain.StatusCompleted
	})
}

func TestGetLatestQuote(t *testing.T) {
	repo := newFakeRepo()
	repo.quotes["EUR/MXN"] = domain.Quote{
		Pair:      domain.Pair{Base: "EUR", Quote: "MXN"},
		Price:     19.62,
		UpdatedAt: time.Now().UTC(),
	}
	svc := testService(t, repo, &fakeProvider{})
	ctx := context.Background()

	q, err := svc.GetLatestQuote(ctx, " eur/mxn ")
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != 19.62 || q.Pair.String() != "EUR/MXN" {
		t.Fatalf("unexpected quote: %+v", q)
	}

	if _, err := svc.GetLatestQuote(ctx, "EUR/GBP"); !errors.Is(err, domain.ErrUnsupportedPair) {
		t.Fatalf("err = %v, want ErrUnsupportedPair", err)
	}
	if _, err := svc.GetLatestQuote(ctx, "EURGBP"); !errors.Is(err, domain.ErrInvalidPair) {
		t.Fatalf("err = %v, want ErrInvalidPair", err)
	}
	if _, err := svc.GetLatestQuote(ctx, "USD/EUR"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetUpdateRequest_InvalidID(t *testing.T) {
	svc := testService(t, newFakeRepo(), &fakeProvider{})
	if _, err := svc.GetUpdateRequest(context.Background(), "abc"); !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID", err)
	}
}

// blockingProvider «зависает» внутри Rate, пока тест не разрешит продолжить.
type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingProvider) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
		return rates.Rate{Pair: pair, Price: 1, Source: "blocking"}, nil
	case <-ctx.Done():
		return rates.Rate{}, ctx.Err()
	}
}

func TestShutdown_WaitsForInFlightRequest(t *testing.T) {
	repo := newFakeRepo()
	prov := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}

	svc := New(repo, prov, testCurrencies, Options{
		QueueSize:      10,
		Workers:        1,
		RequestTimeout: 5 * time.Second,
		ResyncInterval: time.Hour,
	})
	t.Cleanup(func() { svc.Shutdown(context.Background()) }) // Shutdown идемпотентен

	req, _, err := svc.CreateUpdateRequest(context.Background(), "EUR/MXN", "")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-prov.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("воркер не начал обработку")
	}

	done := make(chan struct{})
	go func() {
		svc.Shutdown(context.Background())
		close(done)
	}()

	// Shutdown не должен завершиться, пока воркер в полёте.
	select {
	case <-done:
		t.Fatal("shutdown завершился до окончания обработки")
	case <-time.After(150 * time.Millisecond):
	}

	close(prov.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown не завершился после освобождения воркера")
	}

	repo.mu.Lock()
	status := repo.requests[req.ID].Status
	repo.mu.Unlock()
	if status != domain.StatusCompleted {
		t.Fatalf("status = %v, want completed", status)
	}
}
