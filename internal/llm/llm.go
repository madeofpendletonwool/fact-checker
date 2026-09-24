// Package llm provides the model client shared by the pipeline stages.
//
// It speaks the OpenAI chat-completions shape against any compatible
// endpoint (FACTCHECK_AI_BASE_URL), following the politeness conventions
// ported from the reference implementation:
//
//   - an identified User-Agent on every request;
//   - a configurable minimum interval between requests, shared across
//     goroutines so a concurrent run still honours one global rate;
//   - exponential backoff honouring Retry-After on 429/5xx and network
//     errors;
//   - the API key travels in the Authorization header only — never in
//     errors, never logged.
//
// Stage code depends on the Completer interface, never on the concrete
// client, so tests replay fixtures through a fake.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default request-shape values for the client.
const (
	DefaultRequestInterval = 500 * time.Millisecond
	DefaultTimeout         = 2 * time.Minute
	DefaultMaxTokens       = 8192
	maxBackoff             = 60 * time.Second
	maxRetries             = 4
	userAgent              = "fact-checker/0.1.0 (+https://github.com/madeofpendletonwool/fact-checker)"
)

var retryStatus = map[int]bool{408: true, 409: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// Error reports an endpoint failure that retries did not clear.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

// NewError builds an *Error with the given message (used by fakes in
// tests and by stage code translating transport failures).
func NewError(msg string) error { return &Error{msg: msg} }

// NewOverloadError builds an *OverloadError with the given message.
func NewOverloadError(msg string) error { return &OverloadError{msg: msg} }

// OverloadError reports input larger than the model accepts. It is not
// retried: the unit's text is too large and the run must move on.
type OverloadError struct{ msg string }

func (e *OverloadError) Error() string { return e.msg }

// Response is one completed model call.
type Response struct {
	Text           string
	Model          string
	InputTokens    int
	OutputTokens   int
	FinishReason   string
	LatencySeconds float64
}

// Completer is the shape every model backend satisfies. One instance may be
// shared across goroutines; implementations serialise their rate limiting.
type Completer interface {
	Model() string
	Complete(ctx context.Context, system, user string) (Response, error)
}

// Client is a rate-limited chat-completions client.
type Client struct {
	apiKey   string
	model    string
	baseURL  string
	interval time.Duration
	timeout  time.Duration
	maxToken int
	http     *http.Client

	throttleMu sync.Mutex
	lastSlot   time.Time
}

// ClientOptions tunes the concrete client. Zero values take the defaults.
type ClientOptions struct {
	RequestInterval time.Duration
	Timeout         time.Duration
	MaxTokens       int
	HTTPClient      *http.Client
}

// NewClient validates the endpoint configuration and returns a client.
func NewClient(apiKey, model, baseURL string, opts ClientOptions) (*Client, error) {
	if apiKey == "" {
		return nil, errf("no API key configured (set FACTCHECK_AI_API_KEY)")
	}
	if model == "" {
		return nil, errf("no model configured (set FACTCHECK_MODEL_EXTRACT)")
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	interval := opts.RequestInterval
	if interval <= 0 {
		interval = DefaultRequestInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxToken := opts.MaxTokens
	if maxToken <= 0 {
		maxToken = DefaultMaxTokens
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		apiKey:   apiKey,
		model:    model,
		baseURL:  trimTrailingSlash(baseURL),
		interval: interval,
		timeout:  timeout,
		maxToken: maxToken,
		http:     httpClient,
	}, nil
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// Complete performs one chat completion, retrying transient failures with
// backoff and honouring one global request interval across goroutines.
func (c *Client) Complete(ctx context.Context, system, user string) (Response, error) {
	body, err := json.Marshal(map[string]any{
		"model":      c.model,
		"max_tokens": c.maxToken,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return Response{}, fmt.Errorf("marshal request: %w", err)
	}

	delay := max(c.interval, time.Second)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return Response{}, fmt.Errorf("model call cancelled: %w", err)
		}
		if err := c.throttle(ctx); err != nil {
			return Response{}, err
		}
		started := time.Now()

		response, retryable, err := c.post(ctx, body)
		if err != nil {
			var overload *OverloadError
			if errors.As(err, &overload) {
				return Response{}, err
			}
			if !retryable || attempt == maxRetries {
				return Response{}, err
			}
			if err := sleepBackoff(ctx, delay, retryAfter(err)); err != nil {
				return Response{}, err
			}
			delay = nextDelay(delay)
			continue
		}

		parsed, err := parseChatResponse(response)
		if err != nil {
			return Response{}, err
		}
		parsed.Model = orDefault(parsed.Model, c.model)
		parsed.LatencySeconds = time.Since(started).Seconds()
		return parsed, nil
	}
	return Response{}, errf("request failed after %d attempts", maxRetries+1)
}

// post sends one request. A retryable true with non-nil error means the
// caller should back off and try again; statusError and netError carry the
// Retry-After hint.
func (c *Client) post(ctx context.Context, body []byte) (payload []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("network error calling model endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		detail := truncate(string(raw), 300)
		if resp.StatusCode == http.StatusRequestEntityTooLarge || mentionsTooLong(detail) {
			return nil, false, &OverloadError{msg: fmt.Sprintf("HTTP %d: request too large for the model", resp.StatusCode)}
		}
		if retryStatus[resp.StatusCode] {
			return nil, true, &statusError{code: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
		}
		return nil, false, errf("HTTP %d from model endpoint: %s", resp.StatusCode, detail)
	}
	if readErr != nil {
		return nil, true, fmt.Errorf("read response body: %w", readErr)
	}
	return raw, false, nil
}

// throttle claims the next global request slot, queueing concurrent callers
// behind each other rather than letting them sleep the same interval.
func (c *Client) throttle(ctx context.Context) error {
	var wait time.Duration
	c.throttleMu.Lock()
	now := time.Now()
	if c.lastSlot.IsZero() {
		c.lastSlot = now
	} else if next := c.lastSlot.Add(c.interval); next.After(now) {
		wait = next.Sub(now)
		c.lastSlot = next
	} else {
		c.lastSlot = now
	}
	c.throttleMu.Unlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("rate gate: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

type statusError struct {
	code       int
	retryAfter string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("HTTP %d from model endpoint", e.code)
}

func retryAfter(err error) string {
	var se *statusError
	if errors.As(err, &se) {
		return se.retryAfter
	}
	return ""
}

func sleepBackoff(ctx context.Context, delay time.Duration, retryAfterHeader string) error {
	pause := delay
	if retryAfterHeader != "" {
		if seconds, err := strconv.ParseFloat(retryAfterHeader, 64); err == nil {
			pause = max(pause, min(time.Duration(seconds*float64(time.Second)), maxBackoff))
		}
	}
	timer := time.NewTimer(min(pause, maxBackoff))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("backoff: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func nextDelay(delay time.Duration) time.Duration {
	next := delay * 2
	if next > maxBackoff || next <= 0 {
		return maxBackoff
	}
	return next
}

// parseChatResponse extracts the fields the pipeline records.
func parseChatResponse(raw []byte) (Response, error) {
	var payload struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Response{}, errf("invalid JSON response from model endpoint")
	}
	if payload.Error != nil {
		return Response{}, errf("API error %s: %s", orDefault(payload.Error.Type, "unknown"), truncate(payload.Error.Message, 300))
	}
	if len(payload.Choices) == 0 {
		return Response{}, errf("response has no choices")
	}
	if payload.Usage == nil {
		return Response{}, errf("response has no usage block")
	}
	return Response{
		Text:         payload.Choices[0].Message.Content,
		Model:        payload.Model,
		InputTokens:  payload.Usage.PromptTokens,
		OutputTokens: payload.Usage.CompletionTokens,
		FinishReason: payload.Choices[0].FinishReason,
	}, nil
}

// mentionsTooLong recognizes provider phrasings for oversized input so the
// run treats it as an overload rather than a transient failure.
func mentionsTooLong(detail string) bool {
	d := strings.ToLower(truncate(detail, 300))
	return strings.Contains(d, "too long") ||
		strings.Contains(d, "context length") ||
		strings.Contains(d, "maximum context")
}

// CostTracker accumulates usage and, when prices are configured, the
// estimated USD cost of a run.
type CostTracker struct {
	PriceInMTok  float64
	PriceOutMTok float64

	Calls        int
	InputTokens  int
	OutputTokens int
	latencies    []float64
}

// Add records one response's usage.
func (t *CostTracker) Add(r Response) {
	t.Calls++
	t.InputTokens += r.InputTokens
	t.OutputTokens += r.OutputTokens
	t.latencies = append(t.latencies, r.LatencySeconds)
}

// CostUSD is the accumulated estimated spend.
func (t *CostTracker) CostUSD() float64 {
	return float64(t.InputTokens)/1e6*t.PriceInMTok + float64(t.OutputTokens)/1e6*t.PriceOutMTok
}

// Priced reports whether prices are configured; an unpriced run falls back
// to unit caps and tracks tokens instead of dollars.
func (t *CostTracker) Priced() bool { return t.PriceInMTok > 0 || t.PriceOutMTok > 0 }

// AvgLatency returns the mean call latency in seconds.
func (t *CostTracker) AvgLatency() float64 {
	if len(t.latencies) == 0 {
		return 0
	}
	sum := 0.0
	for _, l := range t.latencies {
		sum += l
	}
	return sum / float64(len(t.latencies))
}

// MaxLatency returns the worst call latency in seconds.
func (t *CostTracker) MaxLatency() float64 {
	worst := 0.0
	for _, l := range t.latencies {
		if l > worst {
			worst = l
		}
	}
	return worst
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
