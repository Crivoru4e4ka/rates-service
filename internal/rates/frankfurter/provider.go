// Package frankfurter — провайдер котировок через открытый API frankfurter.dev
// (референсные курсы Европейского центрального банка, API-ключ не требуется).
//
// Запрос выполняется с base=<BASE пары> и symbols=<QUOTE>; если API вернул
// иную базу, цена пары пересчитывается через rates.CrossPrice — как и у
// провайдера exchangeratesapi, математика не зависит от базы ответа.
package frankfurter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

// SourceName — имя источника для поля Source у rates.Rate.
const SourceName = "frankfurter.dev"

// userAgent — честный небраузерный UA: frankfurter не блокирует таких клиентов.
const userAgent = "rates-service/1.0"

type Provider struct {
	baseURL    *url.URL
	httpClient *http.Client
	logger     *slog.Logger

	initErr error // ошибка разбора RATES_API_URL, возвращается при вызове Rate
}

// New создаёт провайдер. При некорректном baseURL провайдер создаётся, но
// каждый вызов Rate будет возвращать ошибку конфигурации.
func New(baseURL string, timeout time.Duration, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p := &Provider{
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		p.initErr = fmt.Errorf("frankfurter: некорректный RATES_API_URL %q", baseURL)
		return p
	}
	p.baseURL = u
	return p
}

type latestResponse struct {
	Base  string             `json:"base"`
	Rates map[string]float64 `json:"rates"`
}

// Rate получает цену пары из внешнего API.
func (p *Provider) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	if p.initErr != nil {
		return rates.Rate{}, p.initErr
	}

	endpoint := p.baseURL.JoinPath("latest")
	q := url.Values{}
	q.Set("base", pair.Base)
	q.Set("symbols", pair.Quote)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String()+"?"+q.Encode(), nil)
	if err != nil {
		return rates.Rate{}, fmt.Errorf("frankfurter: построить запрос: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return rates.Rate{}, rates.Transient(fmt.Errorf("frankfurter: запрос %s: %w", endpoint.String(), err), 0)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rates.Rate{}, rates.Transient(fmt.Errorf("frankfurter: читать ответ: %w", err), 0)
	}

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("frankfurter: неожиданный статус %d: %s", resp.StatusCode, snippet(body))
		if transientStatus(resp.StatusCode) {
			return rates.Rate{}, rates.Transient(err, rates.RetryAfterFromHeader(resp.Header, time.Now()))
		}
		return rates.Rate{}, err
	}

	var lr latestResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return rates.Rate{}, fmt.Errorf("frankfurter: разобрать JSON: %w", err)
	}
	if len(lr.Rates) == 0 {
		return rates.Rate{}, fmt.Errorf("frankfurter: в ответе нет курсов")
	}

	anchor := lr.Base
	if anchor == "" {
		anchor = pair.Base
	}
	price, err := rates.CrossPrice(anchor, lr.Rates, pair)
	if err != nil {
		return rates.Rate{}, fmt.Errorf("frankfurter: %w", err)
	}

	return rates.Rate{
		Pair:      pair,
		Price:     price,
		FetchedAt: time.Now().UTC(),
		Source:    SourceName,
	}, nil
}

// transientStatus — статусы, при которых имеет смысл повторить запрос.
func transientStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusForbidden ||
		code >= 500
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
