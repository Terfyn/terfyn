package tools

import (
	"context"

	"github.com/Terfyn/terfyn/internal/tools/toolctx"
)

// WithOutputBudget returns ctx carrying the resolved tool-output byte limit the caller will enforce
// on this call's output (spec.ResolvedExecutionLimits.MaxToolOutputBytes). Callers that enforce the
// output limit — the engine's runToolStep and mcpserver.PolicyDispatcher — set it right before
// ToolExecutor.Call so a tool that bounds its own result (the native GitHub list ops) sizes it to
// the limit actually applied rather than the default. n <= 0 means no limit.
func WithOutputBudget(ctx context.Context, n int) context.Context {
	return toolctx.WithOutputBudget(ctx, n)
}

// OutputBudget reports the budget set by WithOutputBudget; ok is false when none was set.
func OutputBudget(ctx context.Context) (n int, ok bool) {
	return toolctx.OutputBudget(ctx)
}

// ToolExecutor runs one tool operation (design doc §12.2 G).
type ToolExecutor interface {
	Call(ctx context.Context, req ToolCallRequest) (ToolCallResponse, error)
}

// ToolCallRequest is a resolved workflow tool step (uses + with).
type ToolCallRequest struct {
	Uses string
	With map[string]any
}

// ToolCallResponse matches the MVP step result envelope (§13.2): output + meta.
type ToolCallResponse struct {
	Output map[string]any
	Meta   ToolCallMeta
}

// ToolCallMeta holds placeholder timing and cost (§13.2).
type ToolCallMeta struct {
	DurationMs int64
	CostUSD    float64
}
