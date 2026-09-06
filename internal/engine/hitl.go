package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/trace"
)

const traceInterruptReasonHITL = "hitl"

// PendingHitlKindApproval marks a workflow-level approval step in checkpoint context (issue #195).
const PendingHitlKindApproval = "approval"

// PendingHitlState is persisted in checkpoint context while awaiting operator input.
type PendingHitlState struct {
	StepID string                    `json:"stepId"`
	Uses   string                    `json:"uses"`
	With   map[string]any            `json:"with"`
	Review policy.ResolvedHitlReview `json:"review"`
	Kind   string                    `json:"kind,omitempty"`
	// ExecKey anchors the pending gate to the suspended execir leaf's CallSite key
	// (issue #258). Always set on the execir run path — the only run path (#278).
	ExecKey string `json:"execKey,omitempty"`
}

// HitlRunOptions configures human-in-the-loop resolution for a run (issue #106).
type HitlRunOptions struct {
	AutoApprove bool
	Actor       string
	Decision    *policy.HitlDecisionInput
}

func (e *Executor) resolvePendingHitl(
	ctx context.Context,
	in RunInput,
	step spec.WorkflowStep,
	pol policy.PolicyEvaluator,
	pctx policy.RunContext,
	pending *PendingHitlState,
) (uses string, with map[string]any, err error) {
	if pending == nil {
		return strings.TrimSpace(step.Uses), nil, nil
	}
	actor := strings.TrimSpace(in.Hitl.Actor)
	if actor == "" {
		actor = policy.DefaultHitlActor
	}
	var decision policy.HitlDecisionInput
	switch {
	case in.Hitl.AutoApprove:
		decision = policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: actor}
	case in.Hitl.Decision != nil:
		decision = *in.Hitl.Decision
		if strings.TrimSpace(decision.Actor) == "" {
			decision.Actor = actor
		}
	default:
		return "", nil, fmt.Errorf("engine: run %q awaiting hitl decision; resume with --decision or --auto-approve", in.RunID)
	}
	gate := policy.HitlGate{Uses: pending.Uses, With: pending.With, Review: pending.Review}
	uses, with, err = policy.ApplyHitlDecision(gate, decision)
	if err != nil {
		if decision.Kind == spec.HitlDecisionReject {
			if e.Trace != nil {
				_, _ = e.Trace.Append(ctx, in.RunID, step.ID, trace.EventHitlDecisionSubmitted, trace.ActorUser, map[string]any{
					"decision": spec.HitlDecisionReject,
					"actor":    decision.Actor,
					"uses":     pending.Uses,
				})
			}
			return "", nil, &policy.HitlRejectedError{Actor: decision.Actor, Uses: pending.Uses}
		}
		return "", nil, err
	}
	traceData := map[string]any{
		"decision":     decision.Kind,
		"actor":        decision.Actor,
		"uses":         pending.Uses,
		"resolvedUses": uses,
	}
	if decision.Kind == spec.HitlDecisionEdit {
		diff := policy.HitlArgsDiff(pending.With, with)
		if e.Trace != nil {
			traceData["argsDiff"] = trace.RedactArgsDiff(diff, gate.Review.RedactKeys, e.Trace.Redaction)
		} else {
			traceData["argsDiff"] = diff
		}
	}
	if decision.Kind == spec.HitlDecisionSwitch {
		traceData["switchTarget"] = decision.SwitchTarget
	}
	if e.Trace != nil {
		_, _ = e.Trace.Append(ctx, in.RunID, step.ID, trace.EventHitlDecisionSubmitted, trace.ActorUser, traceData)
		_, _ = e.Trace.Append(ctx, in.RunID, step.ID, trace.EventHitlResolutionApplied, trace.ActorSystem, traceData)
	}
	if pending.Kind == PendingHitlKindApproval || pending.Uses == spec.ApprovalStepUses {
		return uses, with, nil
	}
	pctx2 := pctx
	pctx2.ApprovedActions = append(append([]string(nil), pctx.ApprovedActions...), uses)
	if err := pol.CheckToolCall(ctx, policy.ToolCallContext{
		Run: pctx2, StepID: step.ID, Uses: uses, With: with,
	}); err != nil {
		return "", nil, err
	}
	return uses, with, nil
}

func (e *Executor) recordAutoApproveHitl(ctx context.Context, runID string, step spec.WorkflowStep, stepIndex int, gate policy.HitlGate, actor string) {
	if e.Trace == nil {
		return
	}
	if strings.TrimSpace(actor) == "" {
		actor = policy.DefaultHitlActor
	}
	redacted := policy.RedactHitlArgs(gate.With, gate.Review.RedactKeys)
	_, _ = e.Trace.Append(ctx, runID, step.ID, trace.EventHitlRequestCreated, trace.ActorSystem, map[string]any{
		"uses":             gate.Uses,
		"with":             redacted,
		"description":      gate.Review.Description,
		"allowedDecisions": gate.Review.AllowedDecisions,
		"allowedSwitchTo":  gate.Review.SwitchTargets,
		"stepIndex":        stepIndex,
		"auto":             true,
	})
	_, _ = e.Trace.Append(ctx, runID, step.ID, trace.EventHitlDecisionSubmitted, trace.ActorAgent, map[string]any{
		"decision":     spec.HitlDecisionApprove,
		"actor":        actor,
		"uses":         gate.Uses,
		"resolvedUses": gate.Uses,
		"auto":         true,
	})
	_, _ = e.Trace.Append(ctx, runID, step.ID, trace.EventHitlResolutionApplied, trace.ActorSystem, map[string]any{
		"decision":     spec.HitlDecisionApprove,
		"actor":        actor,
		"uses":         gate.Uses,
		"resolvedUses": gate.Uses,
		"auto":         true,
	})
}

func policySpecFromEvaluator(pol policy.PolicyEvaluator) *spec.PolicySpec {
	type specCarrier interface {
		PolicySpec() *spec.PolicySpec
	}
	if c, ok := pol.(specCarrier); ok {
		return c.PolicySpec()
	}
	return nil
}

// policyMaxIterationsCeiling returns the governing policy's execution.maxIterations ceiling (0 when
// no policy or no ceiling is set), so the agent loop clamps to the policy's bound rather than the
// frozen global cap (issue #522).
func policyMaxIterationsCeiling(pol policy.PolicyEvaluator) int {
	ps := policySpecFromEvaluator(pol)
	if ps == nil {
		return 0
	}
	return spec.PolicyMaxIterations(ps.Execution)
}
