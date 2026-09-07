package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Terfyn/terfyn/internal/models/anthropic"
)

// defaultAnthropicInputSchema is sent when ToolDef.Parameters is empty.
// Anthropic requires input_schema to be a JSON Schema object.
var defaultAnthropicInputSchema = json.RawMessage(`{"type":"object","properties":{}}`)

func mapToAnthropicRequest(req GenerateRequest) (anthropic.Request, error) {
	system, msgs, err := mapAnthropicMessages(req.Messages)
	if err != nil {
		return anthropic.Request{}, err
	}
	out := anthropic.Request{
		Model:       req.Model,
		System:      system,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.ResponseFormat != nil {
		schema, err := normalizeStructuredOutputSchema(req.ResponseFormat.Schema)
		if err != nil {
			return anthropic.Request{}, err
		}
		out.OutputConfig = &anthropic.OutputConfig{
			Format: &anthropic.OutputFormat{Type: "json_schema", Schema: schema},
		}
	}
	// tool_choice is only valid alongside tools; a stray ToolChoice on a
	// plain completion is ignored so existing two-field call sites stay valid.
	if len(req.Tools) > 0 {
		choice, err := mapAnthropicToolChoice(req.ToolChoiceOrDefault())
		if err != nil {
			return anthropic.Request{}, err
		}
		tools, err := mapAnthropicTools(req.Tools)
		if err != nil {
			return anthropic.Request{}, err
		}
		out.Tools = tools
		out.ToolChoice = choice
	}
	return out, nil
}

func mapFromAnthropicResponse(in anthropic.Response) GenerateResponse {
	var calls []ToolCall
	if len(in.ToolCalls) > 0 {
		calls = make([]ToolCall, len(in.ToolCalls))
		for i, c := range in.ToolCalls {
			calls[i] = ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Input}
		}
	}
	return GenerateResponse{
		Content:    in.Text,
		ToolCalls:  calls,
		StopReason: in.StopReason,
		Meta: GenerateMeta{
			DurationMs:       in.DurationMs,
			PromptTokens:     in.InputTokens,
			CompletionTokens: in.OutputTokens,
		},
	}
}

// unexecutedToolResultContent answers a tool_use the engine never paired with a result. Anthropic
// rejects a request whose assistant tool_use block has no matching tool_result in the next user turn
// (an opaque HTTP 400), so a failed or malformed call left dangling would poison the *next* generate.
// The backstop below synthesizes this well-formed is_error result instead (issue #524).
const unexecutedToolResultContent = `{"error":"tool call was not executed"}`

// ensureToolResultsAnswered guarantees every assistant tool_use is answered by a tool_result in the
// immediately-following user turn, synthesizing a well-formed is_error result for any that the caller
// left unanswered. This is a robustness backstop, not the normal path — the engine already pairs every
// executed call — but it means a malformed/failed tool call can never leave a dangling tool_use that
// the provider rejects on the next request (issue #524). Ordering is preserved: missing results are
// prepended to an existing follow-up tool-result turn, or a fresh user turn is inserted right after
// the tool_use turn when none follows.
func ensureToolResultsAnswered(msgs []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		out = append(out, m)
		if len(m.ToolCalls) == 0 {
			continue
		}
		// Answers may span several consecutive following tool-result turns (OpenAI-style, one result
		// per turn, merged into one Anthropic user turn later), so gather the whole run.
		answered := make(map[string]bool)
		firstResults := -1
		for j := i + 1; j < len(msgs) && len(msgs[j].ToolResults) > 0 && len(msgs[j].ToolCalls) == 0; j++ {
			if firstResults < 0 {
				firstResults = j
			}
			for _, r := range msgs[j].ToolResults {
				answered[strings.TrimSpace(r.ToolCallID)] = true
			}
		}
		var missing []ToolResult
		for _, c := range m.ToolCalls {
			id := strings.TrimSpace(c.ID)
			if id == "" || answered[id] {
				continue
			}
			missing = append(missing, ToolResult{ToolCallID: id, Content: unexecutedToolResultContent, IsError: true})
		}
		if len(missing) == 0 {
			continue
		}
		if firstResults >= 0 {
			next := msgs[firstResults]
			next.ToolResults = append(append([]ToolResult(nil), missing...), next.ToolResults...)
			msgs[firstResults] = next
			continue
		}
		out = append(out, ChatMessage{Role: "user", ToolResults: missing})
	}
	return out
}

func mapAnthropicMessages(msgs []ChatMessage) (system string, out []anthropic.ChatMessage, err error) {
	msgs = ensureToolResultsAnswered(msgs)
	var sys []string
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" {
			if len(m.ToolCalls) > 0 {
				return "", nil, fmt.Errorf("models: anthropic tool_use requires assistant role, got %q", m.Role)
			}
			if len(m.ToolResults) > 0 {
				return "", nil, fmt.Errorf("models: anthropic tool result cannot be sent on a system message")
			}
			sys = append(sys, m.Content)
			continue
		}

		if len(m.ToolCalls) > 0 {
			if err := anthropicAssistantRole(role); err != nil {
				return "", nil, err
			}
			blocks, err := encodeAnthropicToolUse(m.Content, m.ToolCalls)
			if err != nil {
				return "", nil, err
			}
			out = append(out, anthropic.ChatMessage{Role: "assistant", Blocks: blocks})
		}

		if len(m.ToolResults) > 0 {
			blocks, err := encodeAnthropicToolResults(m.ToolResults)
			if err != nil {
				return "", nil, err
			}
			// Anthropic requires user/assistant alternation, so extra text on a
			// tool-result turn stays in the same user message as the tool_result blocks.
			if len(m.ToolCalls) == 0 && m.Content != "" {
				blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: m.Content})
			}
			out = append(out, anthropic.ChatMessage{Role: "user", Blocks: blocks})
			continue
		}

		if len(m.ToolCalls) == 0 {
			textRole, err := anthropicTextRole(role)
			if err != nil {
				return "", nil, err
			}
			out = append(out, anthropic.ChatMessage{Role: textRole, Content: m.Content})
		}
	}
	out = mergeConsecutiveAnthropicMessages(out)
	return strings.Join(sys, "\n\n"), out, nil
}

// mergeConsecutiveAnthropicMessages collapses adjacent same-role turns.
// Anthropic requires user/assistant alternation; OpenAI-style consecutive
// ToolResults (each a user message) would 400 if sent as-is.
func mergeConsecutiveAnthropicMessages(msgs []anthropic.ChatMessage) []anthropic.ChatMessage {
	if len(msgs) < 2 {
		return msgs
	}
	out := []anthropic.ChatMessage{msgs[0]}
	for _, m := range msgs[1:] {
		prev := &out[len(out)-1]
		if prev.Role != m.Role {
			out = append(out, m)
			continue
		}
		mergeAnthropicMessage(prev, m)
	}
	return out
}

func mergeAnthropicMessage(dst *anthropic.ChatMessage, src anthropic.ChatMessage) {
	if len(dst.Blocks) == 0 && dst.Content != "" {
		dst.Blocks = []anthropic.ContentBlock{{Type: "text", Text: dst.Content}}
		dst.Content = ""
	}
	if len(src.Blocks) > 0 {
		dst.Blocks = append(dst.Blocks, src.Blocks...)
		return
	}
	if src.Content != "" {
		dst.Blocks = append(dst.Blocks, anthropic.ContentBlock{Type: "text", Text: src.Content})
	}
}

func mapAnthropicTools(tools []ToolDef) ([]anthropic.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]anthropic.Tool, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return nil, fmt.Errorf("models: anthropic tool definition missing name")
		}
		schema, err := normalizeAnthropicInputSchema(t.Parameters)
		if err != nil {
			return nil, err
		}
		out = append(out, anthropic.Tool{
			Name:        name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

func normalizeAnthropicInputSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return defaultAnthropicInputSchema, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("models: anthropic tool parameters are not JSON")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("models: anthropic tool parameters: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("models: anthropic tool parameters must be a JSON object")
	}
	return raw, nil
}

func mapAnthropicToolChoice(choice string) (*anthropic.ToolChoice, error) {
	if choice == "" {
		choice = ToolChoiceAuto
	}
	switch choice {
	case ToolChoiceAuto:
		return &anthropic.ToolChoice{Type: "auto"}, nil
	case ToolChoiceNone:
		return &anthropic.ToolChoice{Type: "none"}, nil
	case ToolChoiceRequired:
		return &anthropic.ToolChoice{Type: "any"}, nil
	default:
		return nil, fmt.Errorf("models: unsupported tool_choice %q", choice)
	}
}

func anthropicAssistantRole(role string) error {
	if role == "" || role == "assistant" {
		return nil
	}
	return fmt.Errorf("models: anthropic tool_use requires assistant role, got %q", role)
}

func anthropicTextRole(role string) (string, error) {
	if role == "" {
		return "user", nil
	}
	if role == "user" || role == "assistant" {
		return role, nil
	}
	return "", fmt.Errorf("models: anthropic does not support message role %q (use system, user, or assistant)", role)
}

func encodeAnthropicToolUse(preamble string, calls []ToolCall) ([]anthropic.ContentBlock, error) {
	out := make([]anthropic.ContentBlock, 0, len(calls)+1)
	if preamble != "" {
		out = append(out, anthropic.ContentBlock{Type: "text", Text: preamble})
	}
	for _, c := range calls {
		id := strings.TrimSpace(c.ID)
		name := strings.TrimSpace(c.Name)
		if id == "" {
			return nil, fmt.Errorf("models: anthropic tool call missing id")
		}
		if name == "" {
			return nil, fmt.Errorf("models: anthropic tool call %q: empty name", id)
		}
		input := json.RawMessage(`{}`)
		if len(c.Arguments) > 0 {
			if !json.Valid(c.Arguments) {
				return nil, fmt.Errorf("models: anthropic tool call %q: arguments are not JSON", id)
			}
			var v any
			if err := json.Unmarshal(c.Arguments, &v); err != nil {
				return nil, fmt.Errorf("models: anthropic tool call %q: arguments: %w", id, err)
			}
			if _, ok := v.(map[string]any); !ok {
				return nil, fmt.Errorf("models: anthropic tool call %q: arguments must be a JSON object", id)
			}
			var buf bytes.Buffer
			if err := json.Compact(&buf, c.Arguments); err != nil {
				return nil, fmt.Errorf("models: anthropic tool call %q: arguments: %w", id, err)
			}
			input = buf.Bytes()
		}
		out = append(out, anthropic.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  name,
			Input: input,
		})
	}
	return out, nil
}

func encodeAnthropicToolResults(results []ToolResult) ([]anthropic.ContentBlock, error) {
	out := make([]anthropic.ContentBlock, 0, len(results))
	for _, r := range results {
		id := strings.TrimSpace(r.ToolCallID)
		if id == "" {
			return nil, fmt.Errorf("models: anthropic tool result missing tool_call_id")
		}
		out = append(out, anthropic.ContentBlock{
			Type:      "tool_result",
			ToolUseID: id,
			Content:   r.Content,
			IsError:   r.IsError,
		})
	}
	return out, nil
}
