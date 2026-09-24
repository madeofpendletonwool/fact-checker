package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseChatResponse(t *testing.T) {
	raw := `{"model": "test-model", "choices": [{"message": {"content": "hello"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 10, "completion_tokens": 5}}`
	resp, err := parseChatResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Text != "hello" || resp.InputTokens != 10 || resp.OutputTokens != 5 || resp.FinishReason != "stop" {
		t.Fatalf("unexpected fields: %+v", resp)
	}
}

func TestParseChatResponseAPIError(t *testing.T) {
	raw := `{"error": {"type": "invalid_request_error", "message": "bad"}}`
	if _, err := parseChatResponse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("want API error, got %v", err)
	}
}

func TestParseChatResponseMissingBlocks(t *testing.T) {
	cases := []string{
		`{"choices": [], "usage": {}}`,
		`{"choices": [{"message": {"content": "x"}}], "usage": null}`,
	}
	for _, raw := range cases {
		if _, err := parseChatResponse([]byte(raw)); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}

func TestCostTracker(t *testing.T) {
	tr := CostTracker{PriceInMTok: 3, PriceOutMTok: 15}
	tr.Add(Response{InputTokens: 1_000_000, OutputTokens: 100_000, LatencySeconds: 1})
	tr.Add(Response{InputTokens: 500_000, OutputTokens: 50_000, LatencySeconds: 3})
	if !tr.Priced() {
		t.Fatal("tracker should be priced")
	}
	if got, want := tr.CostUSD(), 3*1.5+15*0.15; got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	if tr.AvgLatency() != 2 {
		t.Fatalf("avg latency = %v", tr.AvgLatency())
	}
	if tr.MaxLatency() != 3 {
		t.Fatalf("max latency = %v", tr.MaxLatency())
	}
}

func TestClientCompleteRoundTrip(t *testing.T) {
	var gotAuth, gotUA string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = fmt.Fprint(w, `{"model": "m", "choices": [{"message": {"content": "{\"claims\": []}"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 7, "completion_tokens": 3}}`)
	}))
	defer srv.Close()

	client, err := NewClient("secret-key", "test-model", srv.URL, ClientOptions{RequestInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"claims": []}` || resp.InputTokens != 7 || resp.OutputTokens != 3 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotUA, "fact-checker") {
		t.Fatalf("user agent = %q", gotUA)
	}
	messages := gotBody["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	if gotBody["model"] != "test-model" {
		t.Fatalf("model = %v", gotBody["model"])
	}
}

func TestClientRetriesOn503ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices": [{"message": {"content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`)
	}))
	defer srv.Close()

	client, err := NewClient("k", "m", srv.URL, ClientOptions{RequestInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestClientHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var firstHit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			firstHit.Store(true)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices": [{"message": {"content": "ok"}}], "usage": {}}`)
	}))
	defer srv.Close()

	client, err := NewClient("k", "m", srv.URL, ClientOptions{RequestInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := client.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("Retry-After ignored: elapsed %v", elapsed)
	}
}

func TestClientOverloadNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error": {"message": "This model's maximum context length is 4096 tokens"}}`)
	}))
	defer srv.Close()

	client, err := NewClient("k", "m", srv.URL, ClientOptions{RequestInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), "s", "u")
	var overload *OverloadError
	if !errors.As(err, &overload) {
		t.Fatalf("want OverloadError, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("overload must not retry; calls = %d", got)
	}
}

func TestClientFatalStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error": {"message": "bad key"}}`)
	}))
	defer srv.Close()

	client, err := NewClient("k", "m", srv.URL, ClientOptions{RequestInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Complete(context.Background(), "s", "u")
	var llmErr *Error
	if !errors.As(err, &llmErr) {
		t.Fatalf("want *Error, got %v", err)
	}
	if strings.Contains(err.Error(), "k") && !strings.Contains(err.Error(), "HTTP") {
		t.Fatalf("error leaks or misreports: %v", err)
	}
}

func TestClientThrottleSpacesConcurrentCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices": [{"message": {"content": "x"}}], "usage": {}}`)
	}))
	defer srv.Close()

	interval := 50 * time.Millisecond
	client, err := NewClient("k", "m", srv.URL, ClientOptions{RequestInterval: interval})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	hits := []time.Time{}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Complete(context.Background(), "s", "u"); err != nil {
				t.Errorf("complete: %v", err)
			}
			mu.Lock()
			hits = append(hits, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()

	sorted := append([]time.Time(nil), hits...)
	for i := 1; i < len(sorted); i++ {
		if gap := sorted[i].Sub(sorted[i-1]); gap < interval-15*time.Millisecond {
			t.Fatalf("calls %d and %d only %v apart (interval %v)", i-1, i, gap, interval)
		}
	}
}

func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("", "m", "http://x", ClientOptions{}); err == nil {
		t.Fatal("empty key must fail")
	}
	if _, err := NewClient("k", "", "http://x", ClientOptions{}); err == nil {
		t.Fatal("empty model must fail")
	}
}
