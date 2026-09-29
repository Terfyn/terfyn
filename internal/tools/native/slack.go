package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/jsonnum"
)

// Slack adapter: post and edit messages via the Slack Web API, config from the environment
// (matching the github adapter's GITHUB_TOKEN / GITHUB_API_URL):
//   - SLACK_BOT_TOKEN — the bot token (xoxb-…), sent as a bearer token.
//   - SLACK_API_URL   — overrides the API base (default https://slack.com/api), e.g. for tests.
//
// Slack returns HTTP 200 even for logical failures, with `{"ok":false,"error":"…"}` in the body, so
// the client checks the `ok` field, not just the status code.
const (
	defaultSlackAPIBase = "https://slack.com/api"
	slackUserAgent      = "terfyn/terfyn (native-slack)"
	maxSlackRespBody    = 1 << 20 // 1 MiB
)

func slackAPIBase() string {
	u := strings.TrimSpace(os.Getenv("SLACK_API_URL"))
	if u == "" {
		return defaultSlackAPIBase
	}
	return strings.TrimSuffix(u, "/")
}

func slackToken() (string, error) {
	t := strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))
	if t == "" {
		return "", fmt.Errorf("native: SLACK_BOT_TOKEN is not set (required for Slack operations)")
	}
	return t, nil
}

// slackCall POSTs a JSON payload to a Slack Web API method and returns the decoded response,
// failing when the transport errors, the status is non-2xx, or the Slack envelope has ok=false.
func slackCall(ctx context.Context, method string, payload map[string]any) (map[string]any, error) {
	token, err := slackToken()
	if err != nil {
		return nil, err
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("native: slack encode body: %w", err)
	}
	fullURL := slackAPIBase() + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", slackUserAgent)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("native: slack request: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSlackRespBody+1))
	if err != nil {
		return nil, fmt.Errorf("native: slack read body: %w", err)
	}
	if int64(len(b)) > maxSlackRespBody {
		return nil, fmt.Errorf("native: slack response body exceeds limit (%d bytes)", maxSlackRespBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("native: slack HTTP %s: %s", resp.Status, truncateRunes(string(b), 512))
	}
	var out map[string]any
	if err := jsonnum.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("native: slack %s decode: %w", method, err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		errMsg, _ := out["error"].(string)
		if errMsg == "" {
			errMsg = "unknown error"
		}
		return nil, fmt.Errorf("native: slack %s failed: %s", method, errMsg)
	}
	return out, nil
}

// slackMessageSend posts a message: chat.postMessage (channel, text).
func slackMessageSend(ctx context.Context, with map[string]any) (map[string]any, error) {
	channel, err := stringFromWith(with, "channel")
	if err != nil {
		return nil, fmt.Errorf("native: message.send %w", err)
	}
	text, err := stringFromWith(with, "text")
	if err != nil {
		return nil, fmt.Errorf("native: message.send %w", err)
	}
	payload := map[string]any{"channel": channel, "text": text}
	if ts, ok := tryStringFromWith(with, "thread_ts"); ok {
		payload["thread_ts"] = ts
	}
	out, err := slackCall(ctx, "chat.postMessage", payload)
	if err != nil {
		return nil, err
	}
	return pickResultFields(out, "ok", "ts", "channel"), nil
}

// slackMessageUpdate edits an existing message: chat.update (channel, ts, text).
func slackMessageUpdate(ctx context.Context, with map[string]any) (map[string]any, error) {
	channel, err := stringFromWith(with, "channel")
	if err != nil {
		return nil, fmt.Errorf("native: message.update %w", err)
	}
	ts, err := stringFromWith(with, "ts")
	if err != nil {
		return nil, fmt.Errorf("native: message.update %w", err)
	}
	text, err := stringFromWith(with, "text")
	if err != nil {
		return nil, fmt.Errorf("native: message.update %w", err)
	}
	out, err := slackCall(ctx, "chat.update", map[string]any{"channel": channel, "ts": ts, "text": text})
	if err != nil {
		return nil, err
	}
	return pickResultFields(out, "ok", "ts", "channel"), nil
}

// pickResultFields returns the requested keys that are present, so a tool result is a small,
// predictable subset of the API response rather than the whole envelope.
func pickResultFields(obj map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	return out
}

func dispatchSlackMessageSend(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := slackMessageSend(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func dispatchSlackMessageUpdate(ctx context.Context, with map[string]any, start time.Time) (map[string]any, ExecMeta, error) {
	out, err := slackMessageUpdate(ctx, with)
	meta := ExecMeta{DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}
