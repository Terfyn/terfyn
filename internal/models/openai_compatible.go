package models

import (
	"net/http"

	"github.com/Terfyn/terfyn/internal/spec"
)

// OpenAI-compatible provider base URLs. xAI Grok, Google Gemini, and Moonshot
// Kimi each expose a Chat Completions surface that mirrors OpenAI's, so the same
// [OpenAIClient] backs them with only the endpoint and pricing table changed.
// Gemini's OpenAI-compatible base normally ends in a trailing slash; base()
// trims it before appending "/chat/completions", so the path stays correct.
const (
	defaultGrokBase   = "https://api.x.ai/v1"
	defaultGeminiBase = "https://generativelanguage.googleapis.com/v1beta/openai"
	defaultKimiBase   = "https://api.moonshot.ai/v1"
)

// NewGrokClientFromConfig builds an xAI Grok client using apiKeyFrom
// (e.g. env:XAI_API_KEY). Recommended models: grok-4.6 (flagship), grok-4-fast.
func NewGrokClientFromConfig(cfg spec.ModelProviderConfig) (*OpenAIClient, error) {
	return newOpenAICompatibleClient(cfg, defaultGrokBase, costProviderGrok)
}

// NewGeminiClientFromConfig builds a Google Gemini client using apiKeyFrom
// (e.g. env:GEMINI_API_KEY). Recommended models: gemini-3.1-pro-preview,
// gemini-3.5-flash, gemini-2.5-flash-lite.
func NewGeminiClientFromConfig(cfg spec.ModelProviderConfig) (*OpenAIClient, error) {
	return newOpenAICompatibleClient(cfg, defaultGeminiBase, costProviderGemini)
}

// NewKimiClientFromConfig builds a Moonshot Kimi client using apiKeyFrom
// (e.g. env:MOONSHOT_API_KEY). Recommended models: kimi-k3 (flagship),
// kimi-k2.7-code.
func NewKimiClientFromConfig(cfg spec.ModelProviderConfig) (*OpenAIClient, error) {
	return newOpenAICompatibleClient(cfg, defaultKimiBase, costProviderKimi)
}

func newOpenAICompatibleClient(cfg spec.ModelProviderConfig, baseURL, costProvider string) (*OpenAIClient, error) {
	key, err := ResolveAPIKeyFrom(cfg.APIKeyFrom)
	if err != nil {
		return nil, err
	}
	return &OpenAIClient{
		APIKey:       key,
		BaseURL:      ResolveProviderBaseURL(cfg, baseURL),
		HTTPClient:   http.DefaultClient,
		CostProvider: costProvider,
	}, nil
}
