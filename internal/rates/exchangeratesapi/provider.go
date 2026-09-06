// Package exchangeratesapi — провайдер котировок через https://exchangeratesapi.io/.
//
// Запрос всегда выполняется с base=EUR, а цена произвольной пары вычисляется
// как кросс-курс из курсов ответа против анкора. Это позволяет работать даже
// на тарифах, где смена базовой валюты ограничена.
package exchangeratesapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"plata-rates/internal/domain"
	"plata-rates/internal/rates"
)

// anchorCurrency — базовая валюта запроса к API.
const anchorCurrency = "EUR"

// SourceName — имя источника для поля Source у rates.Rate.
const SourceName = "exchangeratesapi.io"

type Provider struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
	logger     *slog.Logger

	initErr error // ошибка разбора RATES_API_URL, возвращается при вызове Rate
}

// New создаёт провайдер. При некорректном baseURL провайдер создаётся, но
// каждый вызов Rate будет возвращать ошибку конфигурации.
func New(baseURL, apiKey string, timeout time.Duration, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p := &Provider{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		p.initErr = fmt.Errorf("exchangeratesapi: некорректный RATES_API_URL %q", baseURL)
		return p
	}
	p.baseURL = u
	return p
}

type latestResponse struct {
	Success   bool               `json:"success"`
	Timestamp int64              `json:"timestamp"`
	Base      string             `json:"base"`
	Date      string             `json:"date"`
	Rates     map[string]float64 `json:"rates"`
	Error     *apiError          `json:"error"`
}

type apiError struct {
	Code int    `json:"code"`
	Type string `json:"type"`
	Info string `json:"info"`
}

// Rate получает цену пары из внешнего API.
func (p *Provider) Rate(ctx context.Context, pair domain.Pair) (rates.Rate, error) {
	if p.initErr != nil {
		return rates.Rate{}, p.initErr
	}

	endpoint := p.baseURL.JoinPath("latest")
	q := url.Values{}
	q.Set("access_key", p.apiKey)
	q.Set("base", anchorCurrency)
	if symbols := pairSymbols(pair); symbols != "" {
		q.Set("symbols", symbols)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String()+"?"+q.Encode(), nil)
	if err != nil {
		return rates.Rate{}, fmt.Errorf("exchangeratesapi: построить запрос: %w", err)
	}
	// exchangeratesapi.io стоит за Cloudflare: дефолтный User-Agent Go-клиента
	// периодически получает 403 (block page), поэтому шлём браузероподобный UA.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; rates-service/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return rates.Rate{}, rates.Transient(fmt.Errorf("exchangeratesapi: запрос %s: %w",
			redact(endpoint.String(), p.apiKey), err), 0)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rates.Rate{}, rates.Transient(fmt.Errorf("exchangeratesapi: читать ответ: %w", err), 0)
	}

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("exchangeratesapi: неожиданный статус %d: %s", resp.StatusCode, snippet(body))
		if transientStatus(resp.StatusCode) {
			return rates.Rate{}, rates.Transient(err, rates.RetryAfterFromHeader(resp.Header, time.Now()))
		}
		return rates.Rate{}, err
	}

	var lr latestResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return rates.Rate{}, fmt.Errorf("exchangeratesapi: разобрать JSON: %w", err)
	}
	if lr.Error != nil {
		return rates.Rate{}, fmt.Errorf("exchangeratesapi: ошибка API %d (%s): %s",
			lr.Error.Code, lr.Error.Type, lr.Error.Info)
	}
	if !lr.Success && len(lr.Rates) == 0 {
		return rates.Rate{}, errors.New("exchangeratesapi: в ответе нет success=true и нет rates")
	}

	price, err := rates.CrossPrice(lr.Base, lr.Rates, pair)
	if err != nil {
		return rates.Rate{}, fmt.Errorf("exchangeratesapi: %w", err)
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

// pairSymbols — список валют пары (без анкора) для параметра symbols.
func pairSymbols(pair domain.Pair) string {
	set := make(map[string]struct{}, 2)
	if pair.Base != anchorCurrency {
		set[pair.Base] = struct{}{}
	}
	if pair.Quote != anchorCurrency {
		set[pair.Quote] = struct{}{}
	}
	symbols := make([]string, 0, len(set))
	for c := range set {
		symbols = append(symbols, c)
	}
	sort.Strings(symbols)
	return strings.Join(symbols, ",")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// redact убирает access_key из URL для логов/ошибок.
func redact(endpoint, key string) string {
	if key == "" {
		return endpoint
	}
	return strings.ReplaceAll(endpoint, key, "***")
}
