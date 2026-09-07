// Package metrics — минимальные счётчики в текстовом формате Prometheus
// без внешних зависимостей. Все методы nil-safe: можно вызывать на nil-указателе.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// counterVec — счётчик с фиксированным набором лейблов.
type counterVec struct {
	name   string
	help   string
	labels []string

	mu     sync.Mutex
	keys   []string
	values map[string]*atomic.Uint64
}

func newCounterVec(name, help string, labels ...string) *counterVec {
	return &counterVec{
		name:   name,
		help:   help,
		labels: labels,
		values: make(map[string]*atomic.Uint64),
	}
}

func (c *counterVec) inc(labelValues ...string) {
	if c == nil {
		return
	}
	key := labelKey(c.labels, labelValues)

	c.mu.Lock()
	v, ok := c.values[key]
	if !ok {
		v = &atomic.Uint64{}
		c.values[key] = v
		c.keys = append(c.keys, key)
	}
	c.mu.Unlock()

	v.Add(1)
}

func (c *counterVec) write(w io.Writer) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	fmt.Fprintf(w, "# HELP %s %s\n", c.name, c.help)
	fmt.Fprintf(w, "# TYPE %s counter\n", c.name)
	for _, key := range c.keys {
		fmt.Fprintf(w, "%s{%s} %d\n", c.name, key, c.values[key].Load())
	}
}

func labelKey(names, values []string) string {
	parts := make([]string, 0, len(names))
	for i, n := range names {
		val := ""
		if i < len(values) {
			val = values[i]
		}
		parts = append(parts, fmt.Sprintf("%s=%q", n, val))
	}
	return strings.Join(parts, ",")
}

// Metrics — счётчики сервиса.
type Metrics struct {
	HTTPRequests  *counterVec // {method, route, code}
	UpdatesTotal  *counterVec // {status}
	ProviderTotal *counterVec // {provider, result}
	CacheTotal    *counterVec // {result: hit|miss}

	providerName         string
	providerSecondsSum   atomic.Uint64 // биты float64 (CAS-обновление)
	providerSecondsCount atomic.Uint64

	// QueueLen возвращает текущую длину очереди обновлений (gauge).
	QueueLen func() int
}

// New создаёт набор метрик; provider — имя активного провайдера котировок.
func New(provider string) *Metrics {
	return &Metrics{
		HTTPRequests:  newCounterVec("rates_http_requests_total", "Количество HTTP-запросов.", "method", "route", "code"),
		UpdatesTotal:  newCounterVec("rates_updates_total", "Итоги фоновых обновлений котировок.", "status"),
		ProviderTotal: newCounterVec("rates_provider_requests_total", "Вызовы внешнего провайдера котировок.", "provider", "result"),
		CacheTotal:    newCounterVec("rates_provider_cache_total", "Попадания и промахи TTL-кэша провайдера.", "result"),
		providerName:  provider,
	}
}

// ObserveHTTP фиксирует обработанный HTTP-запрос.
func (m *Metrics) ObserveHTTP(method, route, code string) {
	if m == nil {
		return
	}
	m.HTTPRequests.inc(method, route, code)
}

// ObserveUpdate фиксирует результат фонового обновления.
func (m *Metrics) ObserveUpdate(status string) {
	if m == nil {
		return
	}
	m.UpdatesTotal.inc(status)
}

// ObserveProvider фиксирует вызов внешнего провайдера и его длительность.
func (m *Metrics) ObserveProvider(d time.Duration, err error) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.ProviderTotal.inc(m.providerName, result)
	for {
		old := m.providerSecondsSum.Load()
		sum := math.Float64frombits(old) + d.Seconds()
		if m.providerSecondsSum.CompareAndSwap(old, math.Float64bits(sum)) {
			break
		}
	}
	m.providerSecondsCount.Add(1)
}

// ObserveProviderCache фиксирует попадание/промах TTL-кэша провайдера.
func (m *Metrics) ObserveProviderCache(hit bool) {
	if m == nil {
		return
	}
	result := "miss"
	if hit {
		result = "hit"
	}
	m.CacheTotal.inc(result)
}

// Handler отдаёт метрики в текстовом формате Prometheus.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var buf bytes.Buffer

		m.HTTPRequests.write(&buf)
		m.UpdatesTotal.write(&buf)
		m.ProviderTotal.write(&buf)
		m.CacheTotal.write(&buf)

		buf.WriteString("# HELP rates_provider_request_seconds Суммарное время вызовов провайдера.\n")
		buf.WriteString("# TYPE rates_provider_request_seconds summary\n")
		fmt.Fprintf(&buf, "rates_provider_request_seconds_sum %f\n", math.Float64frombits(m.providerSecondsSum.Load()))
		fmt.Fprintf(&buf, "rates_provider_request_seconds_count %d\n", m.providerSecondsCount.Load())

		if m.QueueLen != nil {
			buf.WriteString("# HELP rates_queue_length Текущая длина очереди обновлений.\n")
			buf.WriteString("# TYPE rates_queue_length gauge\n")
			fmt.Fprintf(&buf, "rates_queue_length %d\n", m.QueueLen())
		}

		_, _ = w.Write(buf.Bytes())
	})
}
