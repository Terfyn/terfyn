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
