// Package anthropic implements the Anthropic Messages API client (design doc §7.1, issue #69).
package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	// defaultMaxTok is the max_tokens sent when a request does not set one. Raised from the chat-era
	// 4096 to a realistic agent default that stays under non-streaming HTTP timeouts (issue #514); the
	// engine normally sets Request.MaxTokens from the agent's constraints, so this only applies to a
	// direct adapter call that leaves it unset.
	defaultMaxTok = 16384
	// retryable 429/529 attempts (1 initial + extras). Matches official SDK posture without
	// stalling a run past a bounded wait (issue #532).
	maxGenerateAttempts = 6
	maxRetryWait        = 30 * time.Second
)

// Client calls POST /v1/messages.
type Client struct {
	APIKey  string
	BaseURL string
	// WorkspaceID, when set, is sent as the anthropic-workspace-id header. It is
	// required when authenticating with an identity-linked API key (which is not
	// scoped to a single workspace); a plain workspace-scoped key leaves it empty.
	WorkspaceID string
	HTTPClient  *http.Client
}

func (c *Client) base() string {
	if c != nil && strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	}
	return defaultBaseURL
}

func (c *Client) http() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// Generate performs one non-streaming Messages request. System may be empty.
// Tools / tool_choice are omitted when unset. ToolCalls are populated only when
// StopReason is tool_use.
//
// HTTP 429 and 529 are retried with Retry-After (when present) or exponential
// backoff; genuine 4xx (400/401/403/404/422) stay fatal (issue #532).
func (c *Client) Generate(ctx context.Context, req Request) (Response, error) {
	if c == nil || strings.TrimSpace(c.APIKey) == "" {
		return Response{}, fmt.Errorf("anthropic: client not configured")
	}
	start := time.Now()

	body, err := marshalRequest(req)
	if err != nil {
		return Response{}, err
	}

	var lastRetryable *APIError
	for attempt := 0; attempt < maxGenerateAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, retryWait(attempt, lastRetryable)); err != nil {
				return Response{DurationMs: time.Since(start).Milliseconds()}, err
			}
		}
		out, err := c.doGenerate(ctx, body)
		durationMs := time.Since(start).Milliseconds()
		if err == nil {
			out.DurationMs = durationMs
			return out, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.IsRetryable() {
			if out.DurationMs == 0 {
				out.DurationMs = durationMs
			}
			return out, err
		}
		lastRetryable = apiErr
		if attempt == maxGenerateAttempts-1 {
			out.DurationMs = durationMs
			return out, err
		}
	}
	return Response{DurationMs: time.Since(start).Milliseconds()}, lastRetryable
}

func (c *Client) doGenerate(ctx context.Context, body []byte) (Response, error) {
	url := c.base() + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	if ws := strings.TrimSpace(c.WorkspaceID); ws != "" {
		httpReq.Header.Set("anthropic-workspace-id", ws)
	}

	resp, err := c.http().Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		wait, hasWait := parseRetryAfter(resp.Header)
		return Response{}, &APIError{
			StatusCode:    resp.StatusCode,
			Body:          truncateErrBody(b),
			RetryAfter:    wait,
			HasRetryAfter: hasWait,
		}
	}

	out, err := parseResponse(b)
	if err != nil {
		return out, err
	}
	return out, nil
}

// APIError is a non-2xx Messages API response. It preserves the status code so a caller can attach
// diagnostic context on a 4xx (a request the provider rejected — issue #524) while keeping the same
// "anthropic: HTTP <code>: <body>" Error() string the adapter has always surfaced.
type APIError struct {
	StatusCode int
	Body       string
	// RetryAfter is the wait parsed from Retry-After. HasRetryAfter is true when the header was
	// present (including "0", meaning retry immediately) so a missing header can still take the
	// exponential path (issue #532).
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: HTTP %d: %s", e.StatusCode, e.Body)
}

// IsRetryable reports whether the status is a transient rate-limit / overload that should be
// retried (429 and Anthropic/Cloudflare 529), rather than treated as a fatal client error (issue #532).
func (e *APIError) IsRetryable() bool {
	return e != nil && (e.StatusCode == http.StatusTooManyRequests || e.StatusCode == 529)
}

// IsClientError reports whether the status is a non-retryable 4xx — a request the provider rejected
// as malformed or unauthorized, as opposed to a 429/5xx/transport failure that a retry might clear.
func (e *APIError) IsClientError() bool {
	return e != nil && e.StatusCode >= 400 && e.StatusCode < 500 && !e.IsRetryable()
}

func parseRetryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := time.Until(t)
	if d < 0 {
		return 0, true
	}
	return d, true
}

func retryWait(attempt int, last *APIError) time.Duration {
	if last != nil && last.HasRetryAfter {
		if last.RetryAfter > maxRetryWait {
			return maxRetryWait
		}
		if last.RetryAfter < 0 {
			return 0
		}
		return last.RetryAfter
	}
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 5 {
		shift = 5
	}
	d := time.Second * time.Duration(1<<shift)
	if d > maxRetryWait {
		d = maxRetryWait
	}
	j := time.Duration(0)
	if d > 0 {
		j = time.Duration(rand.Int64N(int64(d/4) + 1))
	}
	return d + j
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncateErrBody(b []byte) string {
	const n = 500
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
