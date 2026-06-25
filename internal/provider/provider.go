// Package provider defines the adapter interface and a registry. Each adapter
// translates the gateway's OpenAI-compatible schema to and from a specific
// upstream API.
package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"frostgate/internal/schema"
)

// Provider is one upstream API adapter.
type Provider interface {
	// Name returns the configured provider name.
	Name() string

	// Chat performs a non-streaming completion. apiKey is the selected
	// credential; model is the upstream model id (already resolved).
	Chat(ctx context.Context, apiKey, model string, req *schema.ChatRequest) (*schema.ChatResponse, error)

	// Stream performs a streaming completion, writing OpenAI-style SSE
	// ("data: {...}\n\n") chunks to w as they arrive. It flushes via the
	// provided flush callback after each write.
	Stream(ctx context.Context, apiKey, model string, req *schema.ChatRequest, w io.Writer, flush func()) error
}

// HTTPError carries an upstream status code so the router can decide whether
// a failure is retryable (5xx, 429) or terminal (4xx).
type HTTPError struct {
	Status     int
	Body       string
	RetryAfter time.Duration // non-zero when the server sent a Retry-After header
}

func (e *HTTPError) Error() string {
	if e.Status == 429 {
		if e.RetryAfter > 0 {
			return fmt.Sprintf("rate limited (429) — retry after %s", e.RetryAfter.Round(time.Second))
		}
		return "rate limited (429)"
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// Retryable reports whether the router should try the next fallback target.
func (e *HTTPError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// HTTPErrorFromResp reads a non-2xx response into an HTTPError, extracting
// Retry-After when present.
func HTTPErrorFromResp(resp *http.Response) *HTTPError {
	raw, _ := io.ReadAll(resp.Body)
	e := &HTTPError{Status: resp.StatusCode, Body: string(raw)}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		// Retry-After is either an integer (seconds) or an HTTP-date.
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}
