package httptool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/jsonnum"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/spec"
)

// ExecMeta is timing/cost metadata for an HTTP tool call (§13.2 placeholders).
type ExecMeta struct {
	DurationMs int64
	CostUSD    float64
}

// clientError is a 4xx response (not retried).
type clientError struct {
	code int
	msg  string
}

func (e *clientError) Error() string {
	return fmt.Sprintf("httptool: HTTP %d %s", e.code, e.msg)
}

// serverHTTPError is a 5xx response (retried when policy allows).
type serverHTTPError struct {
	code int
	body string
}

func (e *serverHTTPError) Error() string {
	return fmt.Sprintf("httptool: HTTP %d", e.code)
}

// Execute performs one logical HTTP tool call, including optional retries on transport/5xx errors.
// client may be nil to use http.DefaultClient (tests should pass srv.Client()).
func Execute(ctx context.Context, cfg *spec.ToolHTTP, retry *spec.ToolRetry, operation string, with map[string]any, client *http.Client) (map[string]any, ExecMeta, error) {
	if cfg == nil {
		return nil, ExecMeta{}, errors.New("httptool: nil http config")
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, ExecMeta{}, errors.New("httptool: empty baseUrl")
	}
	method, path, err := parseOperation(operation)
	if err != nil {
		return nil, ExecMeta{}, err
	}
	urlStr := joinURL(base, path)

	attempts := 1
	if retry != nil && retry.MaxAttempts > 0 {
		attempts = retry.MaxAttempts
	}
	backoff := ""
	if retry != nil {
		backoff = retry.Backoff
	}
	if client == nil {
		client = http.DefaultClient
	}

	start := time.Now()
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			sleepBackoff(ctx, attempt, backoff)
		}
		out, err := doRequest(ctx, client, method, urlStr, cfg.Headers, with)
		if err == nil {
			return out, ExecMeta{DurationMs: time.Since(start).Milliseconds(), CostUSD: 0}, nil
		}
		lastErr = err
		if !retryableHTTP(err) {
			break
		}
	}
	return nil, ExecMeta{DurationMs: time.Since(start).Milliseconds(), CostUSD: 0}, lastErr
}

func parseOperation(operation string) (method, path string, err error) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return "", "", fmt.Errorf("httptool: empty operation")
	}
	parts := strings.Split(operation, ".")
	verbs := map[string]string{
		"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE", "patch": "PATCH",
	}
	if m, ok := verbs[strings.ToLower(parts[0])]; ok {
		if len(parts) == 1 {
			return m, "/", nil
		}
		return m, "/" + strings.Join(parts[1:], "/"), nil
	}
	return "GET", "/" + strings.Join(parts, "/"), nil
}

func joinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if path == "" {
		return base + "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func resolveHeaders(h map[string]string) (http.Header, error) {
	hdr := make(http.Header)
	if h == nil {
		return hdr, nil
	}
	for k, v := range h {
		resolved, err := resolveHeaderValue(v)
		if err != nil {
			return nil, fmt.Errorf("httptool: header %q: %w", k, err)
		}
		hdr.Set(k, resolved)
	}
	return hdr, nil
}

func resolveHeaderValue(v string) (string, error) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "env:") {
		return models.ResolveAPIKeyFrom(v)
	}
	return v, nil
}

func doRequest(ctx context.Context, cli *http.Client, method, urlStr string, headers map[string]string, with map[string]any) (map[string]any, error) {
	hdr, err := resolveHeaders(headers)
	if err != nil {
		return nil, err
	}

	var body io.Reader
	switch method {
	case "POST", "PUT", "PATCH":
		if with == nil {
			with = map[string]any{}
		}
		b, err := json.Marshal(with)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
		if hdr.Get("Content-Type") == "" {
			hdr.Set("Content-Type", "application/json")
		}
	default:
		// GET, DELETE, and any other verb carry no request body — many servers and
		// proxies ignore or reject a body on these — so with-args become query
		// parameters. Dropping them silently (the previous behavior) sent the wrong
		// request and still reported success (#384). Nested values have no scalar
		// query representation and are rejected rather than silently flattened.
		q, err := encodeQueryParams(with)
		if err != nil {
			return nil, err
		}
		if q != "" {
			if strings.Contains(urlStr, "?") {
				urlStr += "&" + q
			} else {
				urlStr += "?" + q
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	req.Header = hdr

	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 500 {
		return nil, &serverHTTPError{code: resp.StatusCode, body: string(b)}
	}
	if resp.StatusCode >= 400 {
		return nil, &clientError{code: resp.StatusCode, msg: truncateBody(b, 512)}
	}

	return decodeResponseBody(b, resp.Header.Get("Content-Type"))
}

// encodeQueryParams renders with-args as a URL query string for body-less methods
// (GET/DELETE). Only scalar values have a query representation; a nested map or
// slice is rejected so the call fails loudly instead of dropping data (#384). The
// output key order is deterministic (url.Values.Encode sorts by key).
func encodeQueryParams(with map[string]any) (string, error) {
	if len(with) == 0 {
		return "", nil
	}
	vals := url.Values{}
	for k, v := range with {
		s, err := scalarToQuery(v)
		if err != nil {
			return "", fmt.Errorf("httptool: with[%q]: %w", k, err)
		}
		if s == nil {
			continue // a nil value carries nothing — omit rather than send "key="
		}
		vals.Set(k, *s)
	}
	return vals.Encode(), nil
}

// scalarToQuery formats a scalar with-arg as a query value, returning nil to omit a
// null. A map or slice has no scalar form and is an error.
func scalarToQuery(v any) (*string, error) {
	var s string
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		s = x
	case bool:
		s = strconv.FormatBool(x)
	case float64: // JSON numbers decode to float64; -1 precision avoids a trailing .0
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		s = strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case json.Number:
		s = x.String()
	default:
		return nil, fmt.Errorf("value of type %T is not a scalar and cannot be a query parameter", v)
	}
	return &s, nil
}

func decodeResponseBody(b []byte, contentType string) (map[string]any, error) {
	ct := strings.ToLower(contentType)
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	if strings.Contains(ct, "application/json") || b[0] == '{' || b[0] == '[' {
		var obj map[string]any
		if jsonnum.Unmarshal(b, &obj) == nil {
			return obj, nil
		}
		var arr []any
		if jsonnum.Unmarshal(b, &arr) == nil {
			return map[string]any{"items": arr}, nil
		}
	}
	return map[string]any{"body": string(b)}, nil
}

func truncateBody(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func retryableHTTP(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ce *clientError
	if errors.As(err, &ce) {
		return false
	}
	var se *serverHTTPError
	if errors.As(err, &se) {
		return true
	}
	return true
}

func sleepBackoff(ctx context.Context, attempt int, kind string) {
	if attempt <= 0 {
		return
	}
	var d time.Duration
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "exponential":
		shift := attempt
		if shift > 8 {
			shift = 8
		}
		d = time.Millisecond * time.Duration(50*(1<<shift))
	case "fixed":
		d = 100 * time.Millisecond
	default:
		d = 50 * time.Millisecond
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
