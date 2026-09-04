// Package service — бизнес-логика сервиса котировок и фоновые воркеры.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

// Repo — хранилище, необходимое сервису.
type Repo interface {
	Ping(ctx context.Context) error

	CreateUpdateRequest(ctx context.Context, pair domain.Pair, idempotencyKey string) (domain.UpdateRequest, bool, error)
	GetUpdateRequest(ctx context.Context, id string) (domain.UpdateRequest, error)
	ListPendingUpdateRequests(ctx context.Context, limit int) ([]domain.UpdateRequest, error)
	CompleteUpdateRequest(ctx context.Context, id string, price float64, completedAt time.Time) error
	FailUpdateRequest(ctx context.Context, id string, reason string) error

	UpsertQuote(ctx context.Context, quote domain.Quote) error
	GetQuote(ctx context.Context, pair domain.Pair) (domain.Quote, error)
}

type Options struct {
	QueueSize      int           // размер очереди обновлений
	Workers        int           // число фоновых воркеров
	RequestTimeout time.Duration // таймаут обращения к провайдеру
	ResyncInterval time.Duration // период повторной постановки pending-запросов
	Logger         *slog.Logger
}

type Service struct {
	repo     Repo
	provider rates.Provider
	allowed  map[string]struct{}
	opts     Options
	logger   *slog.Logger

	queue        chan domain.UpdateRequest
	workersWG    sync.WaitGroup
	resyncCtx    context.Context
	resyncCancel context.CancelFunc
	resyncWG     sync.WaitGroup
}

// New создаёт сервис и запускает фоновые воркеры. Pending-запросы,
// оставшиеся с прошлого запуска, ставятся в очередь при старте.
func New(repo Repo, provider rates.Provider, supportedCurrencies []string, opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.QueueSize < 1 {
		opts.QueueSize = 1
	}
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 5 * time.Second
	}
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = 30 * time.Second
	}

	allowed := make(map[string]struct{}, len(supportedCurrencies))
	for _, c := range supportedCurrencies {
		allowed[c] = struct{}{}
	}

	s := &Service{
		repo:     repo,
		provider: provider,
		allowed:  allowed,
		opts:     opts,
		logger:   opts.Logger,
		queue:    make(chan domain.UpdateRequest, opts.QueueSize),
	}
	s.resyncCtx, s.resyncCancel = context.WithCancel(context.Background())

	for i := 0; i < opts.Workers; i++ {
		s.workersWG.Add(1)
		go s.worker()
	}
	s.resyncWG.Add(1)
	go s.resyncLoop()

	s.resync() // подхватываем pending-запросы предыдущего запуска
	return s
}

// CreateUpdateRequest валидирует пару и создаёт запрос на фоновое обновление.
// created=false означает, что вернулся уже существующий запрос (идемпотентность).
func (s *Service) CreateUpdateRequest(ctx context.Context, pairStr, idempotencyKey string) (domain.UpdateRequest, bool, error) {
	pair, err := domain.ParsePair(pairStr)
	if err != nil {
		return domain.UpdateRequest{}, false, err
	}
	if !s.isSupported(pair) {
		return domain.UpdateRequest{}, false, fmt.Errorf("%w: %s (поддерживаются: %s)",
			domain.ErrUnsupportedPair, pair.String(), strings.Join(s.supported(), ","))
	}

	req, created, err := s.repo.CreateUpdateRequest(ctx, pair, idempotencyKey)
	if err != nil {
		return domain.UpdateRequest{}, false, err
	}
	if created {
		s.enqueue(req)
	}
	return req, created, nil
}

func (s *Service) GetUpdateRequest(ctx context.Context, id string) (domain.UpdateRequest, error) {
	if !domain.ValidRequestID(id) {
		return domain.UpdateRequest{}, domain.ErrInvalidID
	}
	return s.repo.GetUpdateRequest(ctx, id)
}

func (s *Service) GetLatestQuote(ctx context.Context, pairStr string) (domain.Quote, error) {
	pair, err := domain.ParsePair(pairStr)
	if err != nil {
		return domain.Quote{}, err
	}
	if !s.isSupported(pair) {
		return domain.Quote{}, fmt.Errorf("%w: %s (поддерживаются: %s)",
			domain.ErrUnsupportedPair, pair.String(), strings.Join(s.supported(), ","))
	}
	return s.repo.GetQuote(ctx, pair)
}

// Ping проверяет доступность зависимостей (для /healthz).
func (s *Service) Ping(ctx context.Context) error {
	return s.repo.Ping(ctx)
}

// Shutdown останавливает фоновые воркеры. Новые задачи не принимаются,
// принятые дожидаемся в пределах ctx.
func (s *Service) Shutdown(ctx context.Context) {
	s.resyncCancel()
	s.resyncWG.Wait() // сначала завершаем resync, чтобы не было гонки с close(s.queue)
	close(s.queue)

	done := make(chan struct{})
	go func() {
		s.workersWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("shutdown: часть запросов могла остаться в статусе pending", "err", ctx.Err())
	}
}

func (s *Service) isSupported(p domain.Pair) bool {
	_, okBase := s.allowed[p.Base]
	_, okQuote := s.allowed[p.Quote]
	return okBase && okQuote
}

func (s *Service) supported() []string {
	out := make([]string, 0, len(s.allowed))
	for c := range s.allowed {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// enqueue ставит запрос в очередь; при переполнении запрос останется pending
// и будет подхвачен resyncLoop.
func (s *Service) enqueue(req domain.UpdateRequest) {
	select {
	case s.queue <- req:
	default:
		s.logger.Warn("очередь переполнена, запрос будет обработан позже", "id", req.ID)
	}
}

func (s *Service) worker() {
	defer s.workersWG.Done()
	for req := range s.queue {
		s.process(req)
	}
}

// process выполняет одно фоновое обновление: обращается к провайдеру,
// сохраняет котировку и переводит запрос в терминальный статус.
func (s *Service) process(req domain.UpdateRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.RequestTimeout)
	defer cancel()

	rate, err := s.provider.Rate(ctx, req.Pair)
	if err != nil {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer bgCancel()
		if ferr := s.repo.FailUpdateRequest(bgCtx, req.ID, err.Error()); ferr != nil {
			s.logger.Error("не пометить запрос как failed", "id", req.ID, "err", ferr)
		}
		s.logger.Warn("обновление котировки не удалось", "id", req.ID, "pair", req.Pair.String(), "err", err)
		return
	}

	now := time.Now().UTC()
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bgCancel()
	if err := s.repo.UpsertQuote(bgCtx, domain.Quote{Pair: req.Pair, Price: rate.Price, UpdatedAt: now}); err != nil {
		if ferr := s.repo.FailUpdateRequest(bgCtx, req.ID, err.Error()); ferr != nil {
			s.logger.Error("не пометить запрос как failed", "id", req.ID, "err", ferr)
		}
		s.logger.Error("не сохранить котировку", "id", req.ID, "pair", req.Pair.String(), "err", err)
		return
	}
	if err := s.repo.CompleteUpdateRequest(bgCtx, req.ID, rate.Price, now); err != nil {
		s.logger.Error("не пометить запрос как completed", "id", req.ID, "err", err)
		return
	}
	s.logger.Info("котировка обновлена", "id", req.ID, "pair", req.Pair.String(), "price", rate.Price)
}

func (s *Service) resyncLoop() {
	defer s.resyncWG.Done()
	ticker := time.NewTicker(s.opts.ResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.resyncCtx.Done():
			return
		case <-ticker.C:
		}
		s.resync()
	}
}

// resync повторно ставит в очередь pending-запросы (переполнение очереди,
// зависшие задачи после рестарта и т.п.).
func (s *Service) resync() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reqs, err := s.repo.ListPendingUpdateRequests(ctx, 100)
	if err != nil {
		s.logger.Warn("resync: получить pending-запросы", "err", err)
		return
	}
	for _, req := range reqs {
		s.enqueue(req)
	}
	if len(reqs) > 0 {
		s.logger.Info("resync: pending-запросы отправлены в очередь", "count", len(reqs))
	}
}
