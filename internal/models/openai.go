package models

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/spec"
)

const defaultOpenAIBase = "https://api.openai.com/v1"

// OpenAIClient is an OpenAI-compatible Chat Completions client (design doc §12.2 F).
// It maps the provider-neutral tool contract to `tools` / `tool_calls` / `role: "tool"` (issue #157).
//
// The same client backs other providers that expose an OpenAI-compatible
// `/chat/completions` surface — xAI Grok, Google Gemini, and Moonshot Kimi —
// by pointing BaseURL at their endpoint and setting CostProvider so cost
// estimation uses that provider's price table (see [NewGrokClientFromConfig],
// [NewGeminiClientFromConfig], [NewKimiClientFromConfig]).
type OpenAIClient struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	// CostProvider selects the pricing table used to estimate CostUSD.
	// Empty means the OpenAI table (costProviderOpenAI).
	CostProvider string
}

// NewOpenAIClientFromConfig builds a client using apiKeyFrom (e.g. env:OPENAI_API_KEY) from project YAML.
func NewOpenAIClientFromConfig(cfg spec.ModelProviderConfig) (*OpenAIClient, error) {
	key, err := ResolveAPIKeyFrom(cfg.APIKeyFrom)
	if err != nil {
		return nil, err
	}
	return &OpenAIClient{APIKey: key, BaseURL: ResolveProviderBaseURL(cfg, defaultOpenAIBase), HTTPClient: http.DefaultClient}, nil
}

func (c *OpenAIClient) base() string {
	if c != nil && strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	}
	return defaultOpenAIBase
}

func (c *OpenAIClient) http() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// costProvider returns the pricing table key for cost estimation.
func (c *OpenAIClient) costProvider() string {
	if c != nil {
		if p := strings.TrimSpace(c.CostProvider); p != "" {
			return p
		}
	}
	return costProviderOpenAI
}

// Generate calls POST /v1/chat/completions on the configured base URL.
func (c *OpenAIClient) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	if c == nil || c.APIKey == "" {
		return GenerateResponse{}, fmt.Errorf("models: openai client not configured")
	}
	start := time.Now()

	body, err := buildOpenAIChatPayload(req)
	if err != nil {
		return GenerateResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return GenerateResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.http().Do(httpReq)
	if err != nil {
		return GenerateResponse{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return GenerateResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GenerateResponse{}, fmt.Errorf("models: openai HTTP %d: %s", resp.StatusCode, truncateErrBody(b))
	}
	content, calls, stop, pt, ct, err := parseOpenAIChatResponse(b)
	if err != nil {
		return GenerateResponse{}, err
	}
	cost := estimateTokenCostUSD(c.costProvider(), req.Model, pt, ct)
	return GenerateResponse{
		Content:    content,
		ToolCalls:  calls,
		StopReason: stop,
		Meta: GenerateMeta{
			DurationMs:       time.Since(start).Milliseconds(),
			PromptTokens:     pt,
			CompletionTokens: ct,
			CostUSD:          cost,
		},
	}, nil
}

func truncateErrBody(b []byte) string {
	const n = 500
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
