package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Shared trace-event data builders (issue #341). These are the single source of truth for the
// payload shape of the agent-loop events, so a run driven by the internal engine loop and one
// driven by an external runtime (internal/runtime/claudecode) emit structurally identical events —
// a completed external run's audit chain is indistinguishable from a local run's.

// ToolCallFailedReason is the stable tool_execution error value. Raw Error() strings are never
// persisted: tool failures often embed URLs, bodies, or secrets, so the chain records only that
// the call failed, not why.
const ToolCallFailedReason = "tool_call_failed"

// ArgumentsDigest is a stable SHA-256 hex digest of a tool call's arguments. The raw arguments are
// not persisted in tool_selection; the digest lets an auditor confirm the same inputs without
// exposing them.
func ArgumentsDigest(args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%v", args)))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ToolSelectionData is the tool_selection payload: the uses string, a digest of the arguments, and
// (when parseable) the tool name.
func ToolSelectionData(uses, toolName string, args map[string]any) map[string]any {
	data := map[string]any{
		"uses":            uses,
		"argumentsDigest": ArgumentsDigest(args),
	}
	if toolName != "" {
		data["tool"] = toolName
	}
	return data
}

// ToolExecutionData is the tool_execution payload: the uses string, timing/cost, and a success bit.
// On failure it records the redacted ToolCallFailedReason, never the raw error.
func ToolExecutionData(uses, toolName string, durationMs int64, costUSD float64, failed bool) map[string]any {
	data := map[string]any{
		"uses":       uses,
		"durationMs": durationMs,
		"costUsd":    costUSD,
		"success":    !failed,
	}
	if toolName != "" {
		data["tool"] = toolName
	}
	if failed {
		data["error"] = ToolCallFailedReason
	}
	return data
}

// LLMCompletionData is the llm_completion payload: the agent, the model reference, and the turn
// cost. The agent and model keys are always present (matching the internal loop's emission), so an
// external turn is byte-shape-identical to an internal Generate turn.
func LLMCompletionData(agent, model string, costUSD float64) map[string]any {
	return map[string]any{"agent": agent, "model": model, "costUsd": costUSD}
}

// Detail-mode field keys (issue #525). These carry the SUBSTANCE of a turn — reasoning text, call
// arguments, tool output — and are added only when terfyn run --trace-detail is set. Every value still
// passes through the recorder's redaction (by key) and truncation (per-field + payload cap) before
// storage, so a secret-bearing argument or an unbounded output cannot leak or balloon the DB.
const (
	FieldCompletionText = "text"
	FieldToolArgs       = "args"
	FieldToolOutput     = "output"
)

// AddCompletionText attaches the agent's natural-language narration to an llm_completion payload
// (issue #525). An empty/whitespace completion (a pure tool_use turn) adds nothing, so the key is
// present only when there is something to show.
func AddCompletionText(data map[string]any, text string) {
	if data == nil {
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	data[FieldCompletionText] = text
}

// AddToolArgs attaches the resolved call arguments to a tool_selection payload (issue #525),
// alongside the existing digest. Empty args add nothing.
func AddToolArgs(data map[string]any, args map[string]any) {
	if data == nil || len(args) == 0 {
		return
	}
	data[FieldToolArgs] = args
}

// AddToolOutput attaches the tool's structured result to a tool_execution payload (issue #525). Empty
// output adds nothing.
func AddToolOutput(data map[string]any, output map[string]any) {
	if data == nil || len(output) == 0 {
		return
	}
	data[FieldToolOutput] = output
}

// LimitHitData is the limit_hit payload for a budget/iteration breach (issues #163, #341): the
// kind (e.g. "max_cost"), the step, and the denial's extra fields (ceiling, accumulated). This is
// distinct from LimitHitTraceData, which is the byte-limit variant (#117).
func LimitHitData(kind, stepID string, extra map[string]any) map[string]any {
	data := map[string]any{"kind": kind, "stepId": stepID}
	for k, v := range extra {
		data[k] = v
	}
	return data
}
