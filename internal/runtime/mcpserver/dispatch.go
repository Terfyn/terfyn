package mcpserver

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

// ToolEnforcer supplies the per-call contract the external-runtime path shares with the internal
// engine (#390): operation input-schema validation (#204) and tool I/O byte-limit resolution (#117).
// tools.Registry implements it. It is injected EXPLICITLY (WithEnforcement) rather than type-asserted
// off the executor, so wrapping or swapping the executor cannot silently drop enforcement — the
// production wiring (agentcli.RunExternalAgent) fails closed when its executor is not a ToolEnforcer.
type ToolEnforcer interface {
	ValidateInputSchema(uses string, with map[string]any) error
	ResolveToolExecutionLimits(uses string) spec.ResolvedExecutionLimits
}

// Dispatcher runs one granted MCP tool call. The Server has already enforced the closed
// world (the uses string corresponds to an advertised grant); the Dispatcher's job is to run
// that call through the rest of Terfyn's authority path and return the tool output.
type Dispatcher interface {
	Call(ctx context.Context, uses string, args map[string]any) (map[string]any, error)
}

// PolicyDispatcher routes each call through the same inner path as Terfyn's own engine loop:
// policy CheckToolCall (which enforces the closed-world manifest, forbidUnknownTools, and
// approvals) and then tools.ToolExecutor.Call. It holds the run-level context so budget and
// approval decisions see the live run state.
//
// Interactive HITL prompting for an out-of-process agent is not resolved here: CheckToolCall
// enforces approvals as policy (an unapproved required action is denied, surfaced to the agent
// as an isError result), and wiring an interactive approval round-trip through the external
// runtime is left to the live runtime integration (#341).
type PolicyDispatcher struct {
	eval    policy.PolicyEvaluator
	exec    tools.ToolExecutor
	run     policy.RunContext
	seq     atomic.Int64
	rec     *trace.Recorder
	runID   string
	enforce ToolEnforcer // nil disables schema/limit enforcement (test-only; production wires it)
}

// NewPolicyDispatcher builds a dispatcher bound to a policy evaluator, a tool executor, and
// the run context that approval/budget checks read.
func NewPolicyDispatcher(eval policy.PolicyEvaluator, exec tools.ToolExecutor, run policy.RunContext) *PolicyDispatcher {
	return &PolicyDispatcher{eval: eval, exec: exec, run: run}
}

// WithTrace makes the dispatcher fold each external tool call into the run's hash-linked trace
// (issue #341): a tool_selection before the policy check and a tool_execution after the attempt,
// with the same payload shape as the internal loop, so the audit chain is indistinguishable from a
// local run. A nil recorder or empty runID disables emission. Returns the dispatcher for chaining.
func (d *PolicyDispatcher) WithTrace(rec *trace.Recorder, runID string) *PolicyDispatcher {
	d.rec = rec
	d.runID = runID
	return d
}

// WithEnforcement injects the schema/byte-limit enforcer (#390). Production callers MUST set it (and
// fail closed otherwise); it is a separate dependency from the executor so a wrapped/swapped executor
// cannot drop enforcement. Returns the dispatcher for chaining.
func (d *PolicyDispatcher) WithEnforcement(e ToolEnforcer) *PolicyDispatcher {
	d.enforce = e
	return d
}

// Call enforces policy then executes. A policy denial or executor error is returned as-is so
// the Server can surface it to the agent while Terfyn's trace records the denial.
func (d *PolicyDispatcher) Call(ctx context.Context, uses string, args map[string]any) (map[string]any, error) {
	if d.eval == nil || d.exec == nil {
		return nil, fmt.Errorf("mcpserver: dispatcher not fully configured")
	}
	stepID := fmt.Sprintf("mcp-%d", d.seq.Add(1))
	toolName, _, _ := tools.ParseUses(uses)

	// Enforce the per-call contract the internal engine applies in enforceToolInput BEFORE the
	// policy check and dispatch (#390): first the input byte limit (#117) — which may truncate the
	// payload actually dispatched — then the operation input schema (#204) against that same payload.
	// A limits resolver / schema validator is present only when the executor is a real tools.Registry.
	limits, hasLimits := d.toolLimits(uses)
	if hasLimits {
		enforced, err := d.enforceBytes(ctx, stepID, uses, spec.LimitKindToolInput, args,
			limits.MaxToolInputBytes, limits.ToolInputExceedPolicy)
		if err != nil {
			d.traceDenied(ctx, stepID, err)
			return nil, err
		}
		args = enforced
	}
	if d.enforce != nil {
		if err := d.enforce.ValidateInputSchema(uses, args); err != nil {
			// Fail closed like the engine: the tool never runs on a schema violation.
			d.traceDenied(ctx, stepID, err)
			return nil, err
		}
	}

	if err := d.eval.CheckToolCall(ctx, policy.ToolCallContext{
		Run:    d.run,
		StepID: stepID,
		Uses:   uses,
		With:   args,
	}); err != nil {
		// Fail-closed, exactly as the internal loop: the tool is never invoked, so we skip
		// tool_selection/tool_execution and record a system_error instead — keeping the external
		// run's chain structurally identical to a local run's on a denial.
		d.traceDenied(ctx, stepID, err)
		return nil, err
	}
	d.traceSelection(ctx, stepID, uses, toolName, args)
	callCtx := ctx
	if hasLimits {
		// Tell the tool the output limit enforceBytes will apply below, so a tool that bounds its
		// own result sizes it to the resolved limit, not the default. Unlike the engine's
		// runToolStep, this budget is NOT clamped to maxCheckpointBytes: an MCP-served result goes
		// back to the external agent process over MCP and is never written to a Terfyn checkpoint
		// (RunExternalAgent keeps only the trace), so the tool-output limit is the only one it must fit.
		callCtx = tools.WithOutputBudget(ctx, limits.MaxToolOutputBytes)
	}
	resp, err := d.exec.Call(callCtx, tools.ToolCallRequest{Uses: uses, With: args})
	d.traceExecution(ctx, stepID, uses, toolName, resp.Meta, err)
	if err != nil {
		return nil, err
	}
	out := resp.Output
	if hasLimits {
		enforced, oerr := d.enforceBytes(ctx, stepID, uses, spec.LimitKindToolOutput, out,
			limits.MaxToolOutputBytes, limits.ToolOutputExceedPolicy)
		if oerr != nil {
			return nil, oerr
		}
		out = enforced
	}
	return out, nil
}

// toolLimits resolves the tool's byte/policy limits from the injected enforcer.
func (d *PolicyDispatcher) toolLimits(uses string) (spec.ResolvedExecutionLimits, bool) {
	if d.enforce == nil {
		return spec.ResolvedExecutionLimits{}, false
	}
	return d.enforce.ResolveToolExecutionLimits(uses), true
}

// enforceBytes applies a maxBytes limit to a tool I/O map, mirroring engine.enforceMapLimit: emit a
// limit_hit event, then fail closed or truncate per policy. Truncation uses the recorder's redaction
// so a truncated value never leaks a secret (#117). A nil recorder simply skips the event.
func (d *PolicyDispatcher) enforceBytes(ctx context.Context, stepID, uses string, kind spec.LimitKind, v map[string]any, maxBytes int, pol spec.LimitExceedPolicy) (map[string]any, error) {
	if maxBytes <= 0 || v == nil {
		return v, nil
	}
	orig, err := trace.JSONByteLen(v)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: measure %s bytes: %w", kind, err)
	}
	if orig <= maxBytes {
		return v, nil
	}
	truncated := pol == spec.LimitExceedTruncate
	if d.rec != nil && d.runID != "" {
		_, _ = d.rec.Append(ctx, d.runID, stepID, trace.EventLimitHit, trace.ActorSystem,
			trace.LimitHitTraceData(kind, maxBytes, orig, pol, truncated, stepID, uses))
	}
	if pol == spec.LimitExceedFail {
		return nil, fmt.Errorf("mcpserver: %s exceeds limit (%d > %d bytes)", kind, orig, maxBytes)
	}
	out, _, _, err := trace.TruncateMapValue(v, maxBytes, d.redactionOpts())
	if err != nil {
		return nil, fmt.Errorf("mcpserver: truncate %s: %w", kind, err)
	}
	return out, nil
}

func (d *PolicyDispatcher) redactionOpts() trace.RedactionOptions {
	if d.rec != nil {
		return d.rec.Redaction
	}
	return trace.DefaultRedactionOptions()
}

// traceSelection / traceExecution / traceDenied emit the inner-call events when a recorder is
// configured. Like the engine, emission is best-effort: a trace-store error never fails the call.
func (d *PolicyDispatcher) traceSelection(ctx context.Context, stepID, uses, toolName string, args map[string]any) {
	if d.rec == nil || d.runID == "" {
		return
	}
	_, _ = d.rec.Append(ctx, d.runID, stepID, trace.EventToolSelection, trace.ActorAgent,
		trace.ToolSelectionData(uses, toolName, args))
}

// traceDenied records a policy denial as a system_error, matching the internal loop's fail-closed
// path (the tool never ran, so no tool_selection/tool_execution is emitted).
func (d *PolicyDispatcher) traceDenied(ctx context.Context, stepID string, err error) {
	if d.rec == nil || d.runID == "" {
		return
	}
	if de, ok := policy.AsDenied(err); ok {
		_, _ = d.rec.Append(ctx, d.runID, stepID, trace.EventSystemError, trace.ActorSystem, de.TraceData())
	}
}

func (d *PolicyDispatcher) traceExecution(ctx context.Context, stepID, uses, toolName string, meta tools.ToolCallMeta, callErr error) {
	if d.rec == nil || d.runID == "" {
		return
	}
	_, _ = d.rec.Append(ctx, d.runID, stepID, trace.EventToolExecution, trace.ActorAgent,
		trace.ToolExecutionData(uses, toolName, meta.DurationMs, meta.CostUSD, callErr != nil))
}
