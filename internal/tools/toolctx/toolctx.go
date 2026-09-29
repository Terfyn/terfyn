// Package toolctx carries per-call facts from the caller that enforces a tool call's contract (the
// engine's runToolStep, the MCP server's PolicyDispatcher) down to the tool implementation. It is a
// leaf package (stdlib only) so executors such as internal/tools/native can read it without a cycle:
// internal/tools imports internal/tools/native, so the helpers cannot live in internal/tools itself
// (internal/tools re-exports them as tools.WithOutputBudget / tools.OutputBudget for callers).
package toolctx

import "context"

type outputBudgetKey struct{}

// WithOutputBudget returns ctx carrying the resolved tool-output byte limit
// (spec.ResolvedExecutionLimits.MaxToolOutputBytes) that the caller will enforce on this call's
// output. n <= 0 is stored as-is and means "no limit", matching how the engine and the MCP server
// treat a non-positive maxBytes.
func WithOutputBudget(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, outputBudgetKey{}, n)
}

// OutputBudget reports the tool-output byte limit set by WithOutputBudget. ok is false when the
// caller did not set one (a direct Registry.Call, a test); the tool then falls back to its own
// default. n <= 0 with ok true means the caller enforces no output limit.
func OutputBudget(ctx context.Context) (n int, ok bool) {
	if ctx == nil {
		return 0, false
	}
	n, ok = ctx.Value(outputBudgetKey{}).(int)
	return n, ok
}
