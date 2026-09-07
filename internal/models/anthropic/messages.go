package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	stopEndTurn   = "end_turn"
	stopToolUse   = "tool_use"
	stopMaxTokens = "max_tokens"
	stopRefusal   = "refusal"
)

// Request is one non-streaming Messages API call.
type Request struct {
	Model      string
	System     string
	Messages   []ChatMessage
	Tools      []Tool
	ToolChoice *ToolChoice
	// MaxTokens is the max_tokens sent; a non-positive value falls back to defaultMaxTok (issue #514).
	MaxTokens int
	// Temperature is sent verbatim when non-nil; nil leaves the Messages API default (issue #388).
	Temperature *float64
	// OutputConfig requests structured output (output_config.format); nil leaves it unset (issue #510).
	OutputConfig *OutputConfig
}

// OutputConfig is the Messages API output_config object. Only the structured-output format is modeled.
type OutputConfig struct {
	Format *OutputFormat `json:"format,omitempty"`
}

// OutputFormat is output_config.format for JSON structured outputs: type "json_schema" plus the
// JSON Schema the completion must conform to. The model returns the JSON in a normal text block.
type OutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

// Response is a decoded Messages API result.
type Response struct {
	Text         string
	ToolCalls    []ToolUse
	StopReason   string
	InputTokens  int
	OutputTokens int
	DurationMs   int64
}

// Tool is an Anthropic custom tool (name + JSON Schema input_schema).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolChoice is the Messages API tool_choice object (type auto|any|none).
type ToolChoice struct {
	Type string `json:"type"`
}

// ToolUse is one parsed tool_use content block.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ChatMessage is one user or assistant turn (roles user|assistant only).
// Plain text uses Content (JSON string). Tool turns set Blocks (JSON array).
type ChatMessage struct {
	Role    string
	Content string
	Blocks  []ContentBlock
}

// ContentBlock is a Messages API content block (text, tool_use, or tool_result).
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	// IsError, on a tool_result block, marks the call as failed (Messages API tool_result.is_error)
	// so the model reads the content as an error observation. It is emitted only for tool_result
	// blocks and only when true (issue #524).
	IsError bool `json:"is_error,omitempty"`
}

// MarshalJSON keeps tool_result.content present even when empty. Anthropic
// requires that field; omitempty would drop a successful empty tool output.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		Content   *string         `json:"content,omitempty"`
		IsError   bool            `json:"is_error,omitempty"`
	}
	w := wire{
		Type:      b.Type,
		Text:      b.Text,
		ID:        b.ID,
		Name:      b.Name,
		Input:     b.Input,
		ToolUseID: b.ToolUseID,
	}
	if b.Type == "tool_result" {
		c := b.Content
		w.Content = &c
		w.IsError = b.IsError
	}
	return json.Marshal(w)
}

// MarshalJSON encodes Content as a string or Blocks as a content-block array.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	var content json.RawMessage
	var err error
	if len(m.Blocks) > 0 {
		content, err = json.Marshal(m.Blocks)
	} else {
		content, err = json.Marshal(m.Content)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Role: role, Content: content})
}

// resolveMaxTokens returns the max_tokens to send: the request's value when positive, else the
// adapter default (issue #514).
func resolveMaxTokens(n int) int {
	if n > 0 {
		return n
	}
	return defaultMaxTok
}

func marshalRequest(req Request) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("anthropic: empty model")
	}
	payload := struct {
		Model        string        `json:"model"`
		MaxTokens    int           `json:"max_tokens"`
		System       string        `json:"system,omitempty"`
		Messages     []ChatMessage `json:"messages"`
		Tools        []Tool        `json:"tools,omitempty"`
		ToolChoice   *ToolChoice   `json:"tool_choice,omitempty"`
		Temperature  *float64      `json:"temperature,omitempty"`
		OutputConfig *OutputConfig `json:"output_config,omitempty"`
	}{
		Model:        req.Model,
		MaxTokens:    resolveMaxTokens(req.MaxTokens),
		System:       strings.TrimSpace(req.System),
		Tools:        req.Tools,
		ToolChoice:   req.ToolChoice,
		Temperature:  req.Temperature,
		OutputConfig: req.OutputConfig,
	}
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("anthropic: message role %q is not user or assistant", m.Role)
		}
		payload.Messages = append(payload.Messages, ChatMessage{
			Role:    role,
			Content: m.Content,
			Blocks:  m.Blocks,
		})
	}
	if len(payload.Messages) == 0 {
		return nil, fmt.Errorf("anthropic: no messages")
	}
	return json.Marshal(payload)
}

func parseResponse(body []byte) (Response, error) {
	var raw struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}

	type block struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}

	var parts []string
	var calls []ToolUse
	var callErr error
	for _, rawBlock := range raw.Content {
		var b block
		if err := json.Unmarshal(rawBlock, &b); err != nil {
			return Response{}, fmt.Errorf("anthropic: decode content block: %w", err)
		}
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "tool_use":
			call, err := parseToolUse(b.ID, b.Name, b.Input)
			if err != nil {
				callErr = err
				continue
			}
			calls = append(calls, call)
		}
	}

	text := strings.Join(parts, "")
	stop := mapStopReason(raw.StopReason, len(calls))
	if callErr != nil {
		if stop == stopMaxTokens || stop == stopRefusal {
			calls = nil
			callErr = nil
		} else {
			return Response{}, callErr
		}
	}
	if stop == stopToolUse && len(calls) == 0 {
		return Response{}, fmt.Errorf("anthropic: tool_use stop without tool_use blocks")
	}
	if stop != stopToolUse {
		calls = nil
	}
	if stop == stopEndTurn && text == "" {
		return Response{}, fmt.Errorf("anthropic: no text content in response")
	}

	out := Response{Text: text, ToolCalls: calls, StopReason: stop}
	if raw.Usage != nil {
		out.InputTokens = raw.Usage.InputTokens
		out.OutputTokens = raw.Usage.OutputTokens
	}
	return out, nil
}

func parseToolUse(id, name string, input json.RawMessage) (ToolUse, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		return ToolUse{}, fmt.Errorf("anthropic: tool_use missing id")
	}
	if name == "" {
		return ToolUse{}, fmt.Errorf("anthropic: tool_use %q: empty name", id)
	}
	norm, err := normalizeInputObject(input)
	if err != nil {
		return ToolUse{}, fmt.Errorf("anthropic: tool_use %q: %w", id, err)
	}
	return ToolUse{ID: id, Name: name, Input: norm}, nil
}

func normalizeInputObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("input is not JSON")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("input must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	return buf.Bytes(), nil
}

func mapStopReason(stop string, nCalls int) string {
	switch stop {
	case stopMaxTokens:
		return stopMaxTokens
	case stopToolUse:
		return stopToolUse
	case stopEndTurn, "":
		if nCalls > 0 {
			return stopToolUse
		}
		return stopEndTurn
	default:
		return stop
	}
}
