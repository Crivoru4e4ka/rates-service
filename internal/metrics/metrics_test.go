package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveHTTP("GET", "/", "200")
	m.ObserveUpdate("completed")
	m.ObserveProvider(time.Millisecond, nil)
	// Не должно паниковать.
}

func TestHandlerRendersPrometheusText(t *testing.T) {
	m := New("test-provider")
	m.ObserveHTTP("GET", "/quotes", "200")
	m.ObserveHTTP("GET", "/quotes", "200")
	m.ObserveHTTP("POST", "/quotes", "202")
	m.ObserveUpdate("completed")
	m.ObserveUpdate("failed")
	m.ObserveProvider(1500*time.Millisecond, nil)
	m.ObserveProvider(500*time.Millisecond, errors.New("boom"))
	m.QueueLen = func() int { return 7 }

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	body := string(buf)

	want := []string{
		`rates_http_requests_total{method="GET",route="/quotes",code="200"} 2`,
		`rates_http_requests_total{method="POST",route="/quotes",code="202"} 1`,
		`rates_updates_total{status="failed"} 1`,
		`rates_provider_requests_total{provider="test-provider",result="ok"} 1`,
		`rates_provider_requests_total{provider="test-provider",result="error"} 1`,
		`rates_provider_request_seconds_count 2`,
		`rates_queue_length 7`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Fatalf("метрики не содержат %q:\n%s", w, body)
		}
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(body, "rates_provider_request_seconds_sum") {
		t.Fatalf("нет суммарной длительности провайдера:\n%s", body)
	}
}
