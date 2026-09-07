package models

import (
	"context"
	"encoding/json"
)

// ModelClient invokes a chat-capable model (design doc §12.2 F).
type ModelClient interface {
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}

// Tool choice constants for [GenerateRequest.ToolChoice].
// The zero value of ToolChoice behaves as [ToolChoiceAuto].
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
)

// Stop reason constants for [GenerateResponse.StopReason].
const (
	StopReasonEndTurn   = "end_turn"
	StopReasonToolUse   = "tool_use"
	StopReasonMaxTokens = "max_tokens"
)

// ToolDef describes one callable tool exposed to the model (provider-neutral JSON Schema parameters).
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is a model-issued request to invoke a tool before the turn ends.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult carries one tool execution result back to the model on a follow-up turn.
// Attach results on [ChatMessage] via [ChatMessage.ToolResults]; keep [ChatMessage.Role] and
// [ChatMessage.Content] for ordinary text turns. Replay the originating assistant turn with
// [ChatMessage.ToolCalls] so providers can emit native tool-call blocks before tool results
// (issue #156, #157).
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	// IsError marks this result as a failed tool call. Providers that model it (Anthropic's
	// tool_result.is_error) receive the flag so the model treats the content as an error observation
	// to correct from, not a successful output; providers without the concept (OpenAI) ignore it and
	// carry the content alone. A failed or malformed tool call must always be answered by a
	// well-formed tool_result — never left as a dangling tool_use that poisons the next request (#524).
	IsError bool `json:"is_error,omitempty"`
}

// ChatMessage is one turn in the prompt payload.
type ChatMessage struct {
	Role        string       `json:"role"`
	Content     string       `json:"content,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// GenerateRequest is a generation call with optional tool definitions.
type GenerateRequest struct {
	Model      string        `json:"model"`
	Messages   []ChatMessage `json:"messages"`
	Tools      []ToolDef     `json:"tools,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
	// MaxTokens caps the completion's output tokens. Zero leaves it to the adapter's own default
	// (Anthropic requires max_tokens on every request; OpenAI omits it, taking the API default). The
	// engine sets it from the agent's constraints.maxTokens (issue #514).
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature is the sampling temperature to send to the provider. Nil leaves it unset so the
	// provider default applies; a non-nil value (including 0 for deterministic output) is sent
	// verbatim. Adapters translate it to the provider's request field (issue #388).
	Temperature *float64 `json:"temperature,omitempty"`
	// ResponseFormat, when non-nil, asks the provider to constrain the completion to a JSON Schema
	// ("structured outputs"). Nil leaves the output unconstrained. Adapters that support it translate
	// it to the provider request field (Anthropic output_config.format, OpenAI response_format); the
	// engine sets it for an agent whose constraints require structured output (issue #510).
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat requests provider-enforced structured output: the completion must be a JSON value
// conforming to Schema. Schema must be a JSON Schema object and, for providers that require it
// (OpenAI, and Anthropic's strict subset), an object schema with additionalProperties:false and its
// properties listed in required — otherwise the provider rejects the request (issue #510).
type ResponseFormat struct {
	// Name is a short schema identifier some providers require (OpenAI json_schema.name); adapters
	// that do not need it (Anthropic) ignore it. It must match ^[a-zA-Z0-9_-]{1,64}$.
	Name string `json:"name,omitempty"`
	// Schema is the JSON Schema the completion must conform to.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// ToolChoiceOrDefault returns [ToolChoiceAuto] when ToolChoice is unset.
func (r GenerateRequest) ToolChoiceOrDefault() string {
	if r.ToolChoice == "" {
		return ToolChoiceAuto
	}
	return r.ToolChoice
}

// GenerateResponse carries model output, optional tool calls, and accounting metadata.
type GenerateResponse struct {
	// Content is assistant message text when the model replies directly.
	Content    string       `json:"content,omitempty"`
	ToolCalls  []ToolCall   `json:"tool_calls,omitempty"`
	StopReason string       `json:"stop_reason,omitempty"`
	Meta       GenerateMeta `json:"meta"`
}

// GenerateMeta holds duration, token usage, and cost accounting (§13.2 style).
// For OpenAI, Anthropic, and the mock client, CostUSD is a rough estimate from usage ×
// published per-million token rates when the model is recognized (see tokenUSDPerMillion
// in cost.go). The mock leaves an explicit non-zero CostUSD unchanged. Unknown model ids stay at 0.
type GenerateMeta struct {
	DurationMs       int64   `json:"duration_ms"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}
