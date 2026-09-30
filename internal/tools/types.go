package tools

import (
	"context"

	"github.com/Terfyn/terfyn/internal/tools/toolctx"
)

// WithOutputBudget returns ctx carrying the largest output the caller can keep from this call: the
// resolved tool-output byte limit it will enforce (spec.ResolvedExecutionLimits.MaxToolOutputBytes),
// clamped by any other limit the output must fit (the engine's runToolStep clamps it to half the
// run's maxCheckpointBytes; mcpserver.PolicyDispatcher, whose results are not checkpointed, passes
// the tool-output limit). Both set it right before ToolExecutor.Call so a tool that bounds its own
// result (the native GitHub list ops) sizes it to the limits actually applied rather than the
// default. n <= 0 means no limit.
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
