package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang/lower"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/render"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/telemetry"
	"github.com/Terfyn/terfyn/internal/trace"
)

// engineInvoker is the engine-backed [execir.Invoker] (issue #257): it runs each
// execir leaf through the shared per-step executors (runToolStep/runAgentStep),
// so policy (CheckRun admit + CheckToolCall), tool input/output enforcement
// (schema #204, byte limits/truncation #117), cost, and trace are applied in one
// place — this is the sole run path since #278 retired the WorkflowStep DAG.
//
// It reuses the shared HITL machinery (BuildHitlGateWithEvaluator /
// resolvePendingHitl / approvalHitlGate) for suspend/resume (#258). There is no
// interpolation (execir evaluates argument Values itself — the ADR 002 §5 point)
// and no per-node checkpoint: the durable state is the interpreter's completed-leaf
// memo, written once at the suspend point. Concurrent per-branch suspend and
// nested subworkflow re-entry are handled (#270).
//
// Concurrency: a Graph or Fork invokes from several goroutines. The shared
// [liveCost] is already mutex-guarded; ictx (accumulated step results, read only
// after Interp.Run returns) and the run-step writes are guarded by mu.
type engineInvoker struct {
	e            *Executor
	in           RunInput
	wf           *spec.WorkflowResource
	wfPol        policy.PolicyEvaluator
	runHandle    *telemetry.RunHandle
	cost         *liveCost
	runStartedAt time.Time

	mu   sync.Mutex
	ictx Context // Input + accumulated Steps, for buildWorkflowOutput after the run
	// pending and nested are the ONE presented suspension cause this run cycle: a
	// direct gate (first-wins) or a suspended subworkflow, which preempts a direct
	// gate because its frame carries committed work (#275). At most one is set.
	pending     *PendingHitlState // the direct gate this run cycle suspends at
	pendingSeed *PendingHitlState // the gate loaded from the checkpoint that THIS resume resolves (claimed by the matching branch, #275)
	nested      *nestedSuspension // a suspended subworkflow frame; preempts a direct gate (#275/#270)
	nestedSeed  *NestedRunState   // a suspended subworkflow to resume (seeded from the checkpoint on resume)
}

// nestedSuspension is a subworkflow that suspended: the callee's completed steps +
// pending (ictx) and its interpreter durable state (memo/control), anchored to the
// parent's InvokeWorkflow CallSite key.
type nestedSuspension struct {
	key    string
	stepID string // the parent's workflow: step id (NestedRunState.StepID anchor)
	callee string
	ictx   Context
	state  *execir.RunState
	// child is THIS callee's own suspended subworkflow, when the gate lives another
	// level deeper (outer→mid→inner, gate in inner): mid's frame carries inner's
	// frame so resume can seed it instead of re-running inner fresh (issue #380).
	child *nestedSuspension
}

// approvedActions returns the run's approved actions plus, for a resolved resume
// dispatch, the just-approved uses (extra) — so its CheckToolCall passes, mirroring
// the DAG's executeOneStep. extra is passed per-call (not shared state), which is
// what keeps concurrent per-branch dispatch race-free.
func (a *engineInvoker) approvedActions(extra string) []string {
	out := append([]string(nil), a.in.ApprovedActions...)
	if extra != "" {
		out = append(out, extra)
	}
	return out
}

// claimPending atomically returns and clears the SEEDED pending gate iff it is
// anchored to key (the leaf this resume resolves); nil otherwise. It reads the
// resume seed, NOT the fresh-suspend slot (a.pending): under a concurrent
// Graph/Fork resume, a sibling that re-suspends into a.pending must not clobber
// the seed before the matching branch claims it (issue #275). Concurrency-safe:
// branches re-run from several goroutines.
func (a *engineInvoker) claimPending(key string) *PendingHitlState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingSeed != nil && a.pendingSeed.ExecKey == key {
		p := a.pendingSeed
		a.pendingSeed = nil
		return p
	}
	return nil
}

// setPendingIfFirst records a direct gate as the run's one presented suspension
// cause iff none is claimed yet (first-suspend wins), returning whether it won. A
// racing second gate is dropped and re-runs on resume — safe because a direct
// gate carries no committed work (it IS the pre-dispatch pause). A suspended
// subworkflow (setNestedIfFirst) has priority over a direct gate: since siblings
// are no longer cancelled on suspend (#275), a direct gate must yield the slot to
// a nested frame that already committed inner effects, or that work would be
// dropped and re-run (S7).
func (a *engineInvoker) setPendingIfFirst(p *PendingHitlState) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nested != nil || a.pending != nil {
		return false
	}
	a.pending = p
	return true
}

// setNestedIfFirst records a suspended subworkflow as the run's one presented
// suspension cause, returning whether it was recorded. Its frame carries the
// callee's committed inner memo, which would re-run (duplicate side effect, S7)
// if dropped — so it PREEMPTS a direct gate already in the slot (that gate re-runs
// safely on resume). A second nested frame cannot be preserved in one slot and is
// rejected (false); the caller fails closed rather than risk an S7 duplicate.
func (a *engineInvoker) setNestedIfFirst(n *nestedSuspension) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nested != nil {
		return false
	}
	a.pending = nil // preempt a direct gate; it carries no committed work and re-runs safely
	a.nested = n
	return true
}

func (a *engineInvoker) getPending() *PendingHitlState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending
}

func (a *engineInvoker) getNested() *nestedSuspension {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nested
}

// claimNestedSeed returns the seeded subworkflow frame to resume iff it is
// anchored to key, clearing it so a re-run branch does not double-resume.
func (a *engineInvoker) claimNestedSeed(key string) *NestedRunState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nestedSeed != nil && a.nestedSeed.ExecKey == key {
		ns := a.nestedSeed
		a.nestedSeed = nil
		return ns
	}
	return nil
}

// runViaExecIR lowers the workflow to an execir.Program and runs it via
// execir.Interp with an engine-backed Invoker (issue #257), now durable (#258):
// on a resume it seeds the interpreter's completed-leaf memo so replay never
// re-issues a side effect, and on a HITL/approval suspend it checkpoints that
// memo and returns ErrInterrupted. Completion finishes through the shared
// success tail so output/cost bookkeeping matches the DAG path.
func (e *Executor) runViaExecIR(ctx context.Context, in RunInput, wf *spec.WorkflowResource, wfPol policy.PolicyEvaluator, runStartedAt time.Time, runHandle *telemetry.RunHandle) error {
	// Prefer the PINNED program (issue #260): the exact execir.Program captured in
	// the deployment snapshot — for a fresh Invoke from resolved config, for a
	// resume hydrated from the snapshot — so the runtime never re-lowers from
	// source (ADR 001). Fall back to lowering only when no program was pinned
	// (e.g. a store without artifact support).
	prog := e.Executables[in.WorkflowName]
	if prog == nil {
		lowered, diags := lower.LowerWorkflowResource(wf)
		if err := diags.AsError(); err != nil {
			return e.failRun(ctx, in, fmt.Errorf("engine: lower workflow %q to execir: %w", in.WorkflowName, err), 0)
		}
		prog = lowered
	}
	cost := &liveCost{}
	inv := newEngineInvoker(e, in, wf, wfPol, runHandle, cost, runStartedAt)

	var seed *execir.RunState
	if in.Resume {
		ictx, totalCost, state, nested, err := e.loadExecResumeState(ctx, in, wf)
		if err != nil {
			return e.failRun(ctx, in, err, 0)
		}
		inv.ictx = ictx // seed completed steps for output + interpolation
		inv.pendingSeed = ictx.PendingHitl
		inv.nestedSeed = nested // a suspended subworkflow to resume (#270)
		cost.add(totalCost)     // memoized leaves are not re-invoked, so their cost is already counted
		seed = state
	}

	interp := &execir.Interp{Invoker: inv, MaxConcurrency: in.MaxConcurrentSteps}
	returnValue, runState, err := interp.RunResumable(ctx, prog, in.Input, seed)
	if err != nil {
		return e.failRun(ctx, in, err, cost.get())
	}
	if runState.Suspended {
		return e.suspendExecIR(ctx, in, wf, inv, runState, cost.get())
	}
	ictx := inv.snapshotIctx()
	out, err := e.execIROutput(wf, returnValue, ictx)
	if err != nil {
		return e.failRun(ctx, in, err, cost.get())
	}
	return e.finishRunWithOutput(ctx, in, wf, ictx, cost.get(), out)
}

// execIROutput builds the workflow output on the execir path (#259). For a
// control-flow workflow the flattened resource output.value references only the
// last-lowered arm, so it cannot address the taken arm's result; the interpreter's
// Return value is the correct output. The `.agent` convention is a single
// `{value: <token>}` output, which becomes `{value: <return value>}`; a multi-key
// YAML output is built by buildWorkflowOutput (its step ids align with the ictx),
// preserving DAG parity for straight-line/YAML runs.
func (e *Executor) execIROutput(wf *spec.WorkflowResource, returnValue any, ictx Context) (map[string]any, error) {
	if isSingleValueOutput(wf) {
		return map[string]any{"value": returnValue}, nil
	}
	return buildWorkflowOutput(wf, ictx)
}

// isSingleValueOutput reports the `.agent`-return convention: output.value is a
// single `value:` key (see internal/lang/lower workflow.go lowerBody).
func isSingleValueOutput(wf *spec.WorkflowResource) bool {
	if wf == nil || wf.Spec.Output == nil || len(wf.Spec.Output.Value) != 1 {
		return false
	}
	_, ok := wf.Spec.Output.Value["value"]
	return ok
}

func newEngineInvoker(e *Executor, in RunInput, wf *spec.WorkflowResource, wfPol policy.PolicyEvaluator, runHandle *telemetry.RunHandle, cost *liveCost, runStartedAt time.Time) *engineInvoker {
	return &engineInvoker{
		e:            e,
		in:           in,
		wf:           wf,
		wfPol:        wfPol,
		runHandle:    runHandle,
		cost:         cost,
		runStartedAt: runStartedAt,
		ictx:         Context{Input: in.Input, Steps: map[string]StepResult{}},
	}
}

// snapshotIctx returns the accumulated interpolation context for output building
// once the run has finished (no concurrent invokers remain).
func (a *engineInvoker) snapshotIctx() Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Context{Input: a.ictx.Input, Steps: cloneStepResults(a.ictx.Steps)}
}

func (a *engineInvoker) InvokeTool(ctx context.Context, site execir.CallSite, uses string, args map[string]any) (any, error) {
	key := execir.CallKey(site)

	// Resume at the suspended gate: apply the operator decision, then dispatch the
	// resolved call. resolvePendingHitl re-runs CheckToolCall for the resolved
	// uses and emits the decision trace (audit parity with the DAG path). claim
	// is atomic so, under a concurrent Graph/Fork resume, only the matching branch
	// resolves it.
	if p := a.claimPending(key); p != nil {
		step := spec.WorkflowStep{ID: site.Bind, Uses: p.Uses}
		resolvedUses, resolvedWith, rerr := a.e.resolvePendingHitl(ctx, a.in, step, a.wfPol, a.pctx(""), p)
		if rerr != nil {
			return nil, rerr
		}
		return a.dispatchTool(ctx, site, resolvedUses, resolvedWith, resolvedUses)
	}

	// A gated uses: call suspends (or, under auto-approve, records and proceeds) —
	// whether on a fresh run or as a not-yet-resolved gate reached on resume. The
	// one gate anchored by the checkpoint is resolved above (claimPending), and a
	// completed leaf is replayed from the interpreter memo before it ever reaches
	// here, so the only gated call arriving on resume is a SECOND, still-undecided
	// gate. It must re-suspend for its own decision (issue #275), exactly like
	// InvokeApproval — not fall through to CheckToolCall and fail closed (exit 5).
	// One gate is presented per suspend; successive resumes resolve each in turn.
	step := spec.WorkflowStep{ID: site.Bind, Uses: uses}
	// A gate-builder error is advisory only — the authoritative deny is
	// CheckToolCall inside runToolStep (which emits the system_error trace), so on
	// error fall through to dispatch, exactly as the DAG's executeOneStep discards
	// maybeInterruptForHitl's error when it does not interrupt.
	if gate, gerr := a.buildToolGate(step, uses, args); gerr == nil && gate != nil {
		if a.in.Hitl.AutoApprove {
			// Auto-approve records the decision and proceeds, passing the gated uses
			// as an approved action so the dispatch's CheckToolCall admits it (mirrors
			// the DAG's executeOneStep, which appended uses to pctx.ApprovedActions).
			a.e.recordAutoApproveHitl(ctx, a.in.RunID, step, 0, *gate, a.in.Hitl.Actor)
			return a.dispatchTool(ctx, site, uses, args, uses)
		}
		if a.setPendingIfFirst(&PendingHitlState{StepID: step.ID, Uses: gate.Uses, With: gate.With, Review: gate.Review, ExecKey: key}) {
			a.emitHitlRequest(ctx, step, gate)
		}
		return nil, execir.ErrSuspend
	}
	return a.dispatchTool(ctx, site, uses, args, "")
}

// dispatchTool runs a (possibly HITL-resolved) tool call through the shared
// per-step envelope. extraApproved is the just-approved uses on a resume (empty
// for a fresh dispatch).
func (a *engineInvoker) dispatchTool(ctx context.Context, site execir.CallSite, uses string, args map[string]any, extraApproved string) (any, error) {
	step := spec.WorkflowStep{ID: site.Bind, Uses: uses}
	return a.run(ctx, step, args, extraApproved, func(pctx policy.RunContext) (map[string]any, float64, error) {
		out, meta, err := a.e.runToolStep(ctx, a.runHandle, a.wfPol, a.wf, a.in.RunID, step, args, pctx, uses, args)
		return out, meta.CostUSD, err
	})
}

// InvokeApproval runs a workflow-level approval node (#195/#258): a fresh run
// suspends for a decision; a resume applies it and publishes the reviewed
// payload as the node's output.
func (a *engineInvoker) InvokeApproval(ctx context.Context, site execir.CallSite, info execir.ApprovalInfo, args map[string]any) (any, error) {
	key := execir.CallKey(site)
	step := approvalStepFromInfo(site.Bind, info)

	if p := a.claimPending(key); p != nil {
		_, resolved, rerr := a.e.resolvePendingHitl(ctx, a.in, step, a.wfPol, a.pctx(""), p)
		if rerr != nil {
			return nil, rerr
		}
		if resolved == nil {
			resolved = map[string]any{}
		}
		return a.run(ctx, step, resolved, "", func(policy.RunContext) (map[string]any, float64, error) { return resolved, 0, nil })
	}

	// An unclaimed approval node always needs a decision (there is no dispatch
	// fallback as for a tool gate): auto-approve records and proceeds, otherwise it
	// suspends — whether fresh or a not-yet-resolved approval reached on resume.
	gate := approvalHitlGate(step, args)
	if a.in.Hitl.AutoApprove {
		a.e.recordAutoApproveHitl(ctx, a.in.RunID, step, 0, gate, a.in.Hitl.Actor)
		if args == nil {
			args = map[string]any{}
		}
		return a.run(ctx, step, args, "", func(policy.RunContext) (map[string]any, float64, error) { return args, 0, nil })
	}
	if a.setPendingIfFirst(&PendingHitlState{StepID: step.ID, Uses: gate.Uses, With: gate.With, Review: gate.Review, Kind: PendingHitlKindApproval, ExecKey: key}) {
		a.emitHitlRequest(ctx, step, &gate)
	}
	return nil, execir.ErrSuspend
}

// approvalStepFromInfo reconstructs a spec.WorkflowStep carrying the approval
// presentation so the shared approval/HITL helpers (approvalHitlGate,
// resolvePendingHitl, recordAutoApproveHitl) apply unchanged.
func approvalStepFromInfo(bind string, info execir.ApprovalInfo) spec.WorkflowStep {
	return spec.WorkflowStep{
		ID: bind,
		Approval: &spec.WorkflowApprovalValue{
			Enabled: true,
			Config:  &spec.WorkflowApprovalConfig{Description: info.Description, RedactKeys: info.RedactKeys},
		},
	}
}

func (a *engineInvoker) pctx(extraApproved string) policy.RunContext {
	return policy.RunContext{
		StartedAt:          a.runStartedAt,
		Elapsed:            a.e.now().Sub(a.runStartedAt),
		AccumulatedCostUSD: a.cost.get(),
		ApprovedActions:    a.approvedActions(extraApproved),
	}
}

// buildToolGate returns the HITL gate for a uses: call, or nil when the call is
// ungated. Non-nil means the fresh run must suspend (or auto-record).
func (a *engineInvoker) buildToolGate(step spec.WorkflowStep, uses string, args map[string]any) (*policy.HitlGate, error) {
	return policy.BuildHitlGateWithEvaluator(a.e.Graph, a.wfPol, policySpecFromEvaluator(a.wfPol), policy.ToolCallContext{
		Run: a.pctx(""), StepID: step.ID, Uses: uses, With: args,
	})
}

// emitHitlRequest mirrors interruptForHitlGate's request-created trace + approval
// span so the audit chain matches the DAG path across a suspend.
func (a *engineInvoker) emitHitlRequest(ctx context.Context, step spec.WorkflowStep, gate *policy.HitlGate) {
	if a.runHandle != nil {
		endApproval := a.runHandle.StartApproval(telemetry.ApprovalAttrs{RunID: a.in.RunID, Uses: gate.Uses})
		endApproval()
	}
	if a.e.Trace == nil {
		return
	}
	redacted := policy.RedactHitlArgs(gate.With, gate.Review.RedactKeys)
	data := map[string]any{
		"uses":             gate.Uses,
		"with":             redacted,
		"description":      gate.Review.Description,
		"allowedDecisions": gate.Review.AllowedDecisions,
		"allowedSwitchTo":  gate.Review.SwitchTargets,
	}
	_, _ = a.e.Trace.Append(ctx, a.in.RunID, step.ID, trace.EventHitlRequestCreated, trace.ActorSystem, data)
}

func (a *engineInvoker) InvokeAgent(ctx context.Context, site execir.CallSite, agentName string, args map[string]any) (any, error) {
	step := spec.WorkflowStep{ID: site.Bind, Agent: agentName}
	ar, ok := a.e.Graph.Agents[agentName]
	if !ok || ar == nil {
		return nil, fmt.Errorf("engine: unknown agent %q", agentName)
	}
	return a.run(ctx, step, args, "", func(pctx policy.RunContext) (map[string]any, float64, error) {
		out, meta, err := a.e.runAgentStep(ctx, a.runHandle, a.wfPol, a.wf, a.in.RunID, step, args, pctx, ar)
		return out, meta.CostUSD, err
	})
}

// InvokeWorkflow runs a subworkflow as a NESTED execir run (issue #270): the
// callee lowers to its own execir.Program and executes through a child
// engineInvoker that shares the run-wide cost, nests trace/persistence ids, and
// applies the stricter of caller/callee policy — the execir counterpart to the
// DAG's runSubworkflowStep. On the callee suspending, the parent records a nested
// frame (callee memo + pending) and suspends; on resume the child is seeded from
// that frame so its completed inner steps replay, never re-run. A subworkflow that
// COMPLETED is instead replayed via the parent's own memo (invoke wraps this call),
// so it is never re-entered at all.
func (a *engineInvoker) InvokeWorkflow(ctx context.Context, site execir.CallSite, workflow string, args map[string]any) (result any, err error) {
	key := execir.CallKey(site)
	callee, err := lookupWorkflow(a.e.Graph, workflow)
	if err != nil {
		return nil, fmt.Errorf("engine: step %q: %w", site.Bind, err)
	}

	limitWF := a.wf
	if a.e.rootWF != nil {
		limitWF = a.e.rootWF
	}
	maxDepth := spec.ResolveMaxWorkflowNesting(&a.e.Graph.Spec, &limitWF.Spec)
	depth := a.in.WorkflowDepth + 1
	if depth > maxDepth {
		return nil, fmt.Errorf("engine: step %q: workflow nesting depth %d exceeds maxWorkflowNesting %d", site.Bind, depth, maxDepth)
	}
	childInput := unwrapSingleParamWorkflowInput(a.e.Executables[workflow], args)
	if err := a.e.validateWorkflowInputSchema(callee, childInput); err != nil {
		return nil, fmt.Errorf("engine: step %q subworkflow %q input: %w", site.Bind, workflow, err)
	}

	// Record a run-step row for the workflow: step itself (running now, succeeded
	// on completion below), matching the DAG's per-step envelope so a subworkflow
	// call is addressable in run_steps like any other step.
	wfQID := a.e.qualID(site.Bind)
	wfInJSON := a.redactStepJSON(args)
	wfStarted := a.e.now()
	_ = a.e.Store.UpsertRunStep(ctx, state.RunStep{RunID: a.in.RunID, StepID: wfQID, Status: "running", StartedAt: &wfStarted, InputJSON: string(wfInJSON)})

	// The success path (below) overwrites this row to "succeeded", but every early return —
	// evaluator/lowering/output errors, a callee failure from RunResumable, the second-nested
	// refusal, and the ErrSuspend path — used to leave it "running" forever inside a finished run
	// (#395/#401). Ordinary leaves get this finalization from engineInvoker.run; the subworkflow
	// path bypasses that envelope, so finalize the row here: interrupted on suspend, failed on any
	// other error, with a matching run_error trace event so run_steps is faithful for every kind.
	wfRowFinal := false
	defer func() {
		if err == nil || wfRowFinal {
			return
		}
		wfRowFinal = true
		wfFinished := a.e.now()
		if errors.Is(err, execir.ErrSuspend) {
			_ = a.e.Store.UpsertRunStep(ctx, state.RunStep{
				RunID: a.in.RunID, StepID: wfQID, Status: state.RunStatusInterrupted,
				StartedAt: &wfStarted, FinishedAt: &wfFinished, InputJSON: string(wfInJSON),
			})
			return
		}
		_ = a.e.Store.UpsertRunStep(ctx, state.RunStep{
			RunID: a.in.RunID, StepID: wfQID, Status: "failed",
			StartedAt: &wfStarted, FinishedAt: &wfFinished, InputJSON: string(wfInJSON), ErrorText: err.Error(),
		})
		if a.e.Trace != nil {
			_, _ = a.e.Trace.Append(ctx, a.in.RunID, wfQID, trace.EventRunError, trace.ActorSystem, runErrorTraceData(wfQID, err))
		}
	}()

	calleePol, err := compiledWorkflowEvaluator(a.e.ProjectRoot, a.e.Graph, strings.TrimSpace(callee.Spec.Policy), a.e.PinnedGraph)
	if err != nil {
		return nil, err
	}
	wfPol := policy.StricterOf(a.wfPol, calleePol)

	child := *a.e
	prefix, err := qualifyStepID(a.e.stepPrefix, site.Bind)
	if err != nil {
		return nil, err
	}
	child.stepPrefix = prefix
	if a.e.rootWF != nil {
		child.rootWF = a.e.rootWF
	} else {
		child.rootWF = a.wf
	}
	callStack := append(append([]string(nil), a.in.CallStack...), workflow)
	if child.Trace != nil {
		child.Trace = child.Trace.WithCallStack(callStack)
	}

	childIn := a.in
	childIn.WorkflowName = workflow
	childIn.WorkflowDepth = depth
	childIn.CallStack = callStack
	childIn.Resume = false // nested resume is driven by the seeded frame, not RunInput

	childProg := a.e.Executables[workflow]
	if childProg == nil {
		lowered, diags := lower.LowerWorkflowResource(callee)
		if derr := diags.AsError(); derr != nil {
			return nil, fmt.Errorf("engine: lower subworkflow %q to execir: %w", workflow, derr)
		}
		childProg = lowered
		childInput = unwrapSingleParamWorkflowInput(childProg, args)
	}

	childInv := newEngineInvoker(&child, childIn, callee, wfPol, a.runHandle, a.cost, a.runStartedAt)
	childInv.ictx = Context{Input: childInput, Steps: map[string]StepResult{}}

	var childSeed *execir.RunState
	if ns := a.claimNestedSeed(key); ns != nil && strings.TrimSpace(ns.Workflow) == workflow {
		in := ns.Input
		if in == nil {
			in = childInput
		}
		steps := ns.Steps
		if steps == nil {
			steps = map[string]StepResult{}
		}
		childInv.ictx = Context{Input: in, Steps: steps, PendingHitl: ns.PendingHitl}
		childInv.pendingSeed = ns.PendingHitl
		childInv.nestedSeed = ns.Nested // resume a gate nested another level deeper (#380)
		childSeed = &execir.RunState{Memo: ns.ExecMemo, Control: ns.ExecControl}
	} else if a.e.Trace != nil {
		_, _ = a.e.Trace.Append(ctx, a.in.RunID, a.e.qualID(site.Bind), trace.EventWorkflowCallStarted, trace.ActorSystem, map[string]any{
			"workflow": workflow, "depth": depth, "stepId": site.Bind,
		})
	}

	childInterp := &execir.Interp{Invoker: childInv, MaxConcurrency: a.in.MaxConcurrentSteps}
	// Keep the interpreter return value. The flattened WorkflowStep projection is
	// inert for execir control flow, so rebuilding output from it (buildWorkflowOutput)
	// drops identity/branch/loop returns (#551). Root already uses execIROutput.
	returnValue, childState, rerr := childInterp.RunResumable(ctx, childProg, childInv.ictx.Input, childSeed)
	if rerr != nil {
		return nil, rerr
	}
	if childState.Suspended {
		snap := childInv.snapshotIctx()
		// The child suspended with (possibly) committed inner effects in its memo.
		// setNestedIfFirst preempts a direct gate to preserve them, but a SECOND
		// suspended subworkflow in the same concurrent group cannot be preserved in
		// the single nested slot — dropping its frame would re-run its committed
		// inner steps on resume (S7). Fail closed rather than duplicate side effects.
		if !a.setNestedIfFirst(&nestedSuspension{
			key:    key,
			stepID: site.Bind,
			callee: workflow,
			ictx:   Context{Input: snap.Input, Steps: snap.Steps, PendingHitl: childInv.getPending()},
			state:  childState,
			// When the gate is deeper still, childInv suspended because ITS own
			// subworkflow suspended: carry that frame so resume seeds it recursively
			// rather than re-running the inner pre-gate steps (S7, issue #380).
			child: childInv.getNested(),
		}) {
			return nil, fmt.Errorf("engine: step %q: a second subworkflow suspended for human decision in the same concurrent group is not yet supported (issue #275); a single concurrent subworkflow, or independent direct gates, do resume", site.Bind)
		}
		return nil, execir.ErrSuspend
	}

	snap := childInv.snapshotIctx()
	out, oerr := child.execIROutput(callee, returnValue, snap)
	if oerr != nil {
		return nil, fmt.Errorf("engine: step %q subworkflow %q output: %w", site.Bind, workflow, oerr)
	}
	// Caller-visible value is the child's Return (single-value .agent) or the
	// multi-key YAML output map. Wrapping the Return in {value: …} here would
	// double-wrap when the parent also goes through execIROutput.
	caller := any(out)
	if isSingleValueOutput(callee) {
		caller = returnValue
	} else if returnValue != nil {
		// Object-literal returns flatten to multi-key output.value, so
		// execIROutput would interpolate the inert projection. Use the Return.
		if m, ok := returnValue.(map[string]any); ok {
			out = m
			caller = m
		} else {
			caller = returnValue
		}
	}
	if a.e.Trace != nil {
		_, _ = a.e.Trace.Append(ctx, a.in.RunID, a.e.qualID(site.Bind), trace.EventWorkflowCallFinished, trace.ActorSystem, map[string]any{
			"workflow": workflow, "depth": depth, "stepId": site.Bind,
		})
	}
	wfFinished := a.e.now()
	wfOutJSON := a.redactStepJSON(out)
	_ = a.e.Store.UpsertRunStep(ctx, state.RunStep{
		RunID: a.in.RunID, StepID: wfQID, Status: "succeeded",
		StartedAt: &wfStarted, FinishedAt: &wfFinished, InputJSON: string(wfInJSON), OutputJSON: string(wfOutJSON),
	})
	a.mu.Lock()
	a.ictx.Steps[site.Bind] = StepResult{Output: out, Meta: map[string]any{}}
	a.mu.Unlock()
	return caller, nil
}

// unwrapSingleParamWorkflowInput implements whole-document call semantics for a
// single-parameter callee (#552). Positional lowering emits {param: document} (or
// {arg0: document}); paramScope then binds the parameter to that wrapper. When
// the callee has one parameter and the call supplied one map argument, pass the
// argument as the child input document.
func unwrapSingleParamWorkflowInput(prog *execir.Program, args map[string]any) map[string]any {
	if args == nil {
		return args
	}
	if prog == nil || len(prog.Params) != 1 || len(args) != 1 {
		return args
	}
	for _, v := range args {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return args
}

// run wraps one leaf invocation with the admit/persist/cost/commit envelope the
// DAG applies per step (executeOneStep + commitDAGStepSuccess), so the two paths
// emit the same rows, trace, and cost. dispatch performs the actual leaf call and
// returns its output and cost.
func (a *engineInvoker) run(ctx context.Context, step spec.WorkflowStep, args map[string]any, extraApproved string, dispatch func(policy.RunContext) (map[string]any, float64, error)) (any, error) {
	// A bound step id must be a valid, unqualified id (mirrors the DAG's
	// executeOneStep): reject a '/' before it is silently accepted as a step id.
	// An effect-only leaf has an empty bind and is exempt.
	if step.ID != "" {
		if _, err := qualifyStepID(a.e.stepPrefix, step.ID); err != nil {
			return nil, err
		}
	}
	qid := a.e.qualID(step.ID)
	inJSON := a.redactStepJSON(args)

	// Pre-admit against already-accumulated cost/elapsed (mirrors executeOneStep).
	admitCtx := a.pctx(extraApproved)
	if err := a.wfPol.CheckRun(ctx, admitCtx); err != nil {
		a.e.appendCostLimitHit(ctx, a.in.RunID, qid, err)
		a.failStepRow(ctx, qid, inJSON, err, 0)
		return nil, err
	}

	started := a.e.now()
	if err := a.e.Store.UpsertRunStep(ctx, state.RunStep{
		RunID: a.in.RunID, StepID: qid, Status: "running", StartedAt: &started, InputJSON: string(inJSON),
	}); err != nil {
		return nil, fmt.Errorf("engine: upsert step %q: %w", step.ID, err)
	}

	out, stepCost, err := dispatch(admitCtx)
	if err != nil {
		a.failStepRow(ctx, qid, inJSON, err, stepCost)
		return nil, err
	}

	// Commit cost, then re-check the run budget so two in-flight branches cannot
	// jointly exceed maxTotalCostUsd (mirrors commitDAGStepSuccess).
	total := a.cost.add(stepCost)
	commitCtx := a.pctx(extraApproved)
	commitCtx.AccumulatedCostUSD = total
	if err := a.wfPol.CheckRun(ctx, commitCtx); err != nil {
		a.e.appendCostLimitHit(ctx, a.in.RunID, qid, err)
		a.failStepRow(ctx, qid, inJSON, err, stepCost)
		return nil, err
	}

	finished := a.e.now()
	a.mu.Lock()
	// Record step results only under a real binding name. An effect-only call
	// (a bare-expression statement, common in a loop body) has an empty bind; it
	// is not addressable by interpolation and is not a resource step id, so
	// recording it under "" would both alias every such call and break the
	// checkpoint's step validation (issue #259). Its durable replay is the memo,
	// keyed by CallSite, not ictx.Steps.
	if step.ID != "" {
		a.ictx.Steps[step.ID] = StepResult{
			Output: out,
			Meta:   map[string]any{"costUsd": stepCost, "durationMs": finished.Sub(started).Milliseconds()},
		}
	}
	a.mu.Unlock()

	outJSON := a.redactStepJSON(out)
	if err := a.e.Store.UpsertRunStep(ctx, state.RunStep{
		RunID: a.in.RunID, StepID: qid, Status: "succeeded",
		StartedAt: &started, FinishedAt: &finished, InputJSON: string(inJSON), OutputJSON: string(outJSON), CostUSD: stepCost,
	}); err != nil {
		return nil, fmt.Errorf("engine: upsert step %q: %w", step.ID, err)
	}
	return out, nil
}

// failStepRow records a failed run-step row and a run_error trace event, matching
// the DAG's per-step failure bookkeeping (runDAGStep). The tool/agent executors
// already emit their own system_error/limit_hit for denials before returning.
// redactStepJSON marshals a run_steps input/output payload with the same redaction the trace recorder
// applies, so a sensitive key (token/password/authorization/…) is masked instead of persisted in clear.
func (a *engineInvoker) redactStepJSON(v any) []byte {
	return redactPayloadJSON(v, a.e.Trace)
}

// redactPayloadJSON marshals a DISPLAY payload (a run_steps input/output, or the run's final output) with
// the trace recorder's redaction, so a sensitive key (token/password/authorization/…) is masked instead
// of stored in clear and served verbatim by inspect / state show (issue #408). These are display
// surfaces, NOT the replay source — the checkpoint keeps raw args to dispatch on resume — so redacting
// at the write layer is safe. A map payload runs through trace.PrepareEventData; a scalar (no keys to
// mask) is marshaled as is. Falls back to default redaction when no recorder is set, so it never
// persists raw.
func redactPayloadJSON(v any, r *trace.Recorder) []byte {
	opts := trace.NormalizeRedactionOptions(trace.DefaultRedactionOptions())
	if r != nil {
		opts = r.Redaction
	}
	if m, ok := v.(map[string]any); ok {
		b, _ := json.Marshal(trace.PrepareEventData(m, nil, opts))
		return b
	}
	b, _ := json.Marshal(v)
	return b
}

func (a *engineInvoker) failStepRow(ctx context.Context, qid string, inJSON []byte, err error, stepCost float64) {
	finished := a.e.now()
	started := finished
	_ = a.e.Store.UpsertRunStep(ctx, state.RunStep{
		RunID: a.in.RunID, StepID: qid, Status: "failed",
		StartedAt: &started, FinishedAt: &finished, InputJSON: string(inJSON), ErrorText: err.Error(), CostUSD: stepCost,
	})
	if a.e.Trace != nil {
		_, _ = a.e.Trace.Append(ctx, a.in.RunID, qid, trace.EventRunError, trace.ActorSystem, runErrorTraceData(qid, err))
	}
}

// suspendExecIR persists the execir durable state (completed-leaf memo + control
// + pending gate) and marks the run interrupted (issue #258), mirroring the DAG
// interruptForHitlGate's status/otel/trace so the audit chain matches.
func (e *Executor) suspendExecIR(ctx context.Context, in RunInput, wf *spec.WorkflowResource, inv *engineInvoker, runState *execir.RunState, totalCost float64) error {
	pending := inv.getPending()
	nested := inv.getNested()
	if pending == nil && nested == nil {
		return e.failRun(ctx, in, fmt.Errorf("engine: execir run suspended without a pending gate or nested subworkflow"), totalCost)
	}
	ictx := inv.snapshotIctx()
	ictx.PendingHitl = pending // nil when the suspension is inside a subworkflow
	anchorStep := ""
	if pending != nil {
		anchorStep = pending.StepID
	}
	if nested != nil {
		ictx.Nested = nestedRunStateOf(nested)
		anchorStep = nested.stepID
	}
	if inv.runHandle != nil {
		inv.runHandle.MarkInterrupted()
		ref := inv.runHandle.SpanRef()
		ictx.OtelInterrupt = &ref
	}
	stepIndex := execStepIndex(wf, anchorStep)
	if err := e.saveExecCheckpoint(ctx, wf, in, stepIndex, anchorStep, ictx, totalCost, runState); err != nil {
		return e.failRun(ctx, in, fmt.Errorf("engine: save execir checkpoint: %w", err), totalCost)
	}
	if err := e.Store.UpdateRunStatus(ctx, in.RunID, state.RunStatusInterrupted); err != nil {
		return fmt.Errorf("engine: mark run interrupted: %w", err)
	}
	if e.Trace != nil {
		_, _ = e.Trace.Append(ctx, in.RunID, e.qualID(anchorStep), trace.EventRunError, trace.ActorSystem, map[string]any{
			"stepIndex": stepIndex, "stepId": anchorStep, "reason": traceInterruptReasonHITL, "interrupted": true,
		})
	}
	return ErrInterrupted
}

// nestedRunStateOf converts a suspended-subworkflow frame to its persisted
// NestedRunState, recursing through child frames so a gate nested more than one
// level deep (outer→mid→inner) is checkpointed with every intermediate frame's
// memo intact. On resume each level is re-seeded rather than re-run (S7, #380).
func nestedRunStateOf(n *nestedSuspension) *NestedRunState {
	if n == nil {
		return nil
	}
	return &NestedRunState{
		StepID:      n.stepID,
		Workflow:    n.callee,
		Input:       n.ictx.Input,
		Steps:       n.ictx.Steps,
		Completed:   completedStepIDs(n.ictx.Steps),
		PendingHitl: n.ictx.PendingHitl,
		Nested:      nestedRunStateOf(n.child),
		ExecKey:     n.key,
		ExecMemo:    n.state.Memo,
		ExecControl: n.state.Control,
	}
}

// execStepIndex finds the workflow index of a step id (checkpoint metadata only).
func execStepIndex(wf *spec.WorkflowResource, stepID string) int {
	if wf == nil {
		return -1
	}
	for i := range wf.Spec.Steps {
		if strings.TrimSpace(wf.Spec.Steps[i].ID) == strings.TrimSpace(stepID) {
			return i
		}
	}
	return -1
}

// saveExecCheckpoint writes an execir checkpoint (ExecIR marker + memo/control),
// reusing the size enforcement the DAG checkpoint uses.
func (e *Executor) saveExecCheckpoint(ctx context.Context, wf *spec.WorkflowResource, in RunInput, stepIndex int, anchorStep string, ictx Context, totalCost float64, runState *execir.RunState) error {
	payload := checkpointPayload{
		Version:       checkpointPayloadVersion,
		Input:         ictx.Input,
		Steps:         ictx.Steps,
		Completed:     completedStepIDs(ictx.Steps),
		TotalCostUSD:  totalCost,
		PendingHitl:   ictx.PendingHitl,
		OtelInterrupt: ictx.OtelInterrupt,
		Nested:        ictx.Nested,
		ExecIR:        true,
		ExecMemo:      runState.Memo,
		ExecControl:   runState.Control,
	}
	if payload.Input == nil {
		payload.Input = map[string]any{}
	}
	if payload.Steps == nil {
		payload.Steps = map[string]StepResult{}
	}
	ctxJSON, err := render.MarshalStableJSON(payload)
	if err != nil {
		return fmt.Errorf("engine: marshal execir checkpoint: %w", err)
	}
	if len(ctxJSON) > maxCheckpointContextBytes {
		return fmt.Errorf("engine: execir checkpoint context exceeds absolute maximum %d bytes", maxCheckpointContextBytes)
	}
	stepID := e.qualID(strings.TrimSpace(in.WorkflowName))
	if s := strings.TrimSpace(anchorStep); s != "" {
		stepID = e.qualID(s)
	}
	if err := e.enforceCheckpointSize(ctx, wf, in.RunID, stepID, string(ctxJSON)); err != nil {
		return err
	}
	return e.Store.SaveCheckpoint(ctx, state.RunCheckpoint{
		RunID:       in.RunID,
		StepIndex:   stepIndex,
		StepID:      stepID,
		ContextJSON: string(ctxJSON),
		Status:      state.CheckpointStatusInterrupted,
		CreatedAt:   e.now(),
	})
}

// loadExecResumeState hydrates the seeded interpolation context (completed steps
// + pending gate), the accumulated cost, and the interpreter durable state from
// the latest execir checkpoint.
func (e *Executor) loadExecResumeState(ctx context.Context, in RunInput, wf *spec.WorkflowResource) (Context, float64, *execir.RunState, *NestedRunState, error) {
	cp, err := e.Store.GetLatestCheckpoint(ctx, in.RunID)
	if err != nil {
		return Context{}, 0, nil, nil, fmt.Errorf("engine: load execir checkpoint: %w", err)
	}
	switch cp.Status {
	case state.CheckpointStatusRunning, state.CheckpointStatusInterrupted:
	default:
		return Context{}, 0, nil, nil, fmt.Errorf("engine: checkpoint status %q is not resumable", cp.Status)
	}
	ictx, totalCost, err := unmarshalCheckpointPayload(cp.ContextJSON, e.Graph, wf, cp.StepIndex)
	if err != nil {
		return Context{}, 0, nil, nil, err
	}
	var payload checkpointPayload
	if err := json.Unmarshal([]byte(cp.ContextJSON), &payload); err != nil {
		return Context{}, 0, nil, nil, fmt.Errorf("engine: unmarshal execir checkpoint: %w", err)
	}
	if !payload.ExecIR {
		return Context{}, 0, nil, nil, fmt.Errorf("engine: resume routed to execir but checkpoint is not an execir checkpoint")
	}
	return ictx, totalCost, &execir.RunState{Memo: payload.ExecMemo, Control: payload.ExecControl}, payload.Nested, nil
}

var _ execir.Invoker = (*engineInvoker)(nil)
