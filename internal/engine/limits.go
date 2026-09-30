package engine

import (
	"context"
	"fmt"

	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/trace"
)

// agentMaxIterations resolves the loop's iteration bound (issue #160), clamped to the governing
// policy's execution.maxIterations ceiling (policyCeiling; 0 = unset → the default). The default/cap
// semantics live in spec.ResolveMaxIterations, the single source of truth shared with the
// external-runtime turn mapping (issue #340), so the ceiling is identical across runtimes (#522).
func agentMaxIterations(agent *spec.AgentResource, policyCeiling int) int {
	if agent == nil {
		return spec.ResolveMaxIterations(nil, policyCeiling)
	}
	return spec.ResolveMaxIterations(agent.Spec.Constraints, policyCeiling)
}

func (e *Executor) redactionOpts() trace.RedactionOptions {
	if e != nil && e.Trace != nil {
		return e.Trace.Redaction
	}
	return trace.DefaultRedactionOptions()
}

func (e *Executor) resolveToolLimits(wf *spec.WorkflowResource, uses string) spec.ResolvedExecutionLimits {
	var project *spec.ProjectSpec
	var wfSpec *spec.WorkflowSpec
	var toolSpec *spec.ToolSpec
	if e != nil && e.Graph != nil {
		project = &e.Graph.Spec
	}
	if wf != nil {
		wfSpec = &wf.Spec
	}
	if tn, ok := spec.ParseToolUses(uses); ok && e != nil && e.Graph != nil {
		if tr, found := e.Graph.Tools[tn]; found && tr != nil {
			toolSpec = &tr.Spec
		}
	}
	return spec.ResolveExecutionLimits(project, wfSpec, toolSpec)
}

func (e *Executor) resolveCheckpointLimits(wf *spec.WorkflowResource) spec.ResolvedExecutionLimits {
	var project *spec.ProjectSpec
	var wfSpec *spec.WorkflowSpec
	if e != nil && e.Graph != nil {
		project = &e.Graph.Spec
	}
	if wf != nil {
		wfSpec = &wf.Spec
	}
	return spec.ResolveExecutionLimits(project, wfSpec, nil)
}

// checkpointOutputShare is the divisor applied to maxCheckpointBytes when clamping the output
// budget runToolStep advertises. It is a floor on the copy count, not the count: a completed step's
// output is stored at least twice in a suspension checkpoint (once under steps, once in the execir
// completed-leaf memo that lets a resume replay it), so an output above half the checkpoint limit can
// never survive a later approval gate. Other shapes store it more often, and no constant divisor can
// cover them: an approval (or HITL-gated call) whose `with` carries the output stores another copy
// as the pending gate, and a step inside `for` / `parallel for` leaves one memo copy per iteration
// (steps keeps only the last). Those runs can still exceed maxCheckpointBytes with a single large
// output; see toolOutputBudget.
const checkpointOutputShare = 2

// toolOutputBudget is the output byte budget runToolStep advertises to the tool
// (tools.WithOutputBudget) for a step using uses in wf. It starts from the resolved tool-output
// limit that enforceToolOutput applies, and is clamped to half the run's resolved checkpoint limit
// (maxCheckpointBytes, resolved at the ROOT workflow as saveCheckpoint does, fail-only, and not
// raisable by a tool's limits block): every step output also lands in the checkpoint context, at
// least twice in a suspension checkpoint (checkpointOutputShare), so a budget above that clamp would
// invite the tool to return an output the run can never checkpoint. Both resolved limits are
// always positive (config ignores non-positive overrides); the guards are defensive.
//
// The clamp is per output and assumes the minimum two copies. Everything else that shares the
// checkpoint is the operator's to size (raising maxCheckpointBytes, or lowering the tools'
// maxToolOutputBytes): other steps' outputs, an approval whose `with` carries the output (one more
// copy), and a step in a loop (one more memo copy per iteration). A run whose checkpoint exceeds the
// limit fails at the checkpoint (`checkpoint context exceeds ... bytes`), not at the tool.
func (e *Executor) toolOutputBudget(wf *spec.WorkflowResource, uses string) int {
	budget := e.resolveToolLimits(wf, uses).MaxToolOutputBytes
	cpWF := wf
	if e != nil && e.rootWF != nil {
		cpWF = e.rootWF
	}
	if cp := e.resolveCheckpointLimits(cpWF).MaxCheckpointBytes / checkpointOutputShare; cp > 0 && (budget <= 0 || cp < budget) {
		budget = cp
	}
	return budget
}

func (e *Executor) enforceMapLimit(
	ctx context.Context,
	runID, stepID, uses string,
	kind spec.LimitKind,
	v map[string]any,
	maxBytes int,
	policy spec.LimitExceedPolicy,
) (map[string]any, error) {
	orig, err := trace.JSONByteLen(v)
	if err != nil {
		return nil, fmt.Errorf("engine: measure %s bytes: %w", kind, err)
	}
	if maxBytes <= 0 || orig <= maxBytes {
		return v, nil
	}
	truncated := policy == spec.LimitExceedTruncate
	if e.Trace != nil {
		_, _ = e.Trace.Append(ctx, runID, stepID, trace.EventLimitHit, trace.ActorSystem,
			trace.LimitHitTraceData(kind, maxBytes, orig, policy, truncated, stepID, uses))
	}
	if policy == spec.LimitExceedFail {
		return nil, fmt.Errorf("engine: %s exceeds limit (%d > %d bytes)", kind, orig, maxBytes)
	}
	out, _, _, err := truncateMapInPlace(v, maxBytes, e.redactionOpts())
	if err != nil {
		return nil, fmt.Errorf("engine: truncate %s: %w", kind, err)
	}
	return out, nil
}

func (e *Executor) enforceToolInput(
	ctx context.Context,
	wf *spec.WorkflowResource,
	runID, stepID, uses string,
	with map[string]any,
) (map[string]any, error) {
	limits := e.resolveToolLimits(wf, uses)
	out, err := e.enforceMapLimit(ctx, runID, stepID, uses, spec.LimitKindToolInput, with,
		limits.MaxToolInputBytes, limits.ToolInputExceedPolicy)
	if err != nil {
		return nil, err
	}
	// Validate the operation's input schema (#204 manifest completion) against the payload that is
	// actually dispatched — i.e. AFTER byte-limit enforcement. Under the default `truncate` policy
	// enforceMapLimit may splice "..." into strings or drop keys, so validating the pre-truncation
	// map would let a schema-violating payload reach CheckToolCall/Tools.Call. Fail closed: if what
	// the tool would receive does not satisfy the schema (bad input, or truncation broke it), reject.
	if err := e.validateToolInputSchema(uses, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Executor) enforceToolOutput(
	ctx context.Context,
	wf *spec.WorkflowResource,
	runID, stepID, uses string,
	out map[string]any,
) (map[string]any, error) {
	limits := e.resolveToolLimits(wf, uses)
	return e.enforceMapLimit(ctx, runID, stepID, uses, spec.LimitKindToolOutput, out,
		limits.MaxToolOutputBytes, limits.ToolOutputExceedPolicy)
}

func (e *Executor) enforceCheckpointSize(
	ctx context.Context,
	wf *spec.WorkflowResource,
	runID, stepID string,
	contextJSON string,
) error {
	limits := e.resolveCheckpointLimits(wf)
	orig := len(contextJSON)
	if orig <= limits.MaxCheckpointBytes {
		return nil
	}
	if e.Trace != nil {
		_, _ = e.Trace.Append(ctx, runID, stepID, trace.EventLimitHit, trace.ActorSystem,
			trace.LimitHitTraceData(spec.LimitKindCheckpoint, limits.MaxCheckpointBytes, orig,
				spec.LimitExceedFail, false, stepID, ""))
	}
	return fmt.Errorf("engine: checkpoint context exceeds %d bytes (got %d)", limits.MaxCheckpointBytes, orig)
}
