package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/telemetry"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

// Executor runs a workflow on the execir interpreter (design doc §12.2/§13; the
// WorkflowStep DAG runtime was retired in #278). A `needs:` graph lowers to an
// execir.Graph; workflows with no `needs:` keep implicit sequential YAML order.
type Executor struct {
	Graph       *spec.ProjectGraph
	ProjectRoot string
	// PinnedGraph marks that Graph was hydrated from a run's pinned deployment snapshot (issue
	// #207). When set, policy authority is compiled from Graph (not the on-disk policy snapshot) and
	// schema validation does not re-read live files under ProjectRoot, so a run resumes under the
	// exact authority it started with even after a widening apply.
	PinnedGraph bool
	// Schemas maps a schema ref to the JSON Schema content captured in the run's deployment snapshot
	// (issue #207 follow-up). On a pinned resume, input/output validation uses these instead of
	// re-reading files under ProjectRoot. Empty on fresh runs (disk-backed validation).
	Schemas map[string]string
	// Executables is the pinned execution IR per workflow (issue #260). The execir run path executes
	// the pinned program (fresh run: from resolved config; resume: hydrated from the snapshot) rather
	// than re-lowering. Nil falls back to lowering the resolved graph.
	Executables map[string]*execir.Program
	Tools       tools.ToolExecutor
	Models      *models.Registry
	// ModelResolve, if set, is used instead of Models.ClientFor (tests inject mocks).
	ModelResolve func(modelRef string) (models.ModelClient, string, error)
	Store        state.RuntimeStore
	Trace        *trace.Recorder
	Telemetry    *telemetry.Tracer
	Now          func() time.Time
	// TraceDetail, when true, enriches the streamed/stored trace with the SUBSTANCE of each turn —
	// the agent's reasoning text on llm_completion, the tool arguments on tool_selection, and a
	// bounded tool output on tool_execution (issue #525). It is opt-in (terfyn run --trace-detail) so
	// the default stays terse and the default audit-event byte shape is unchanged; when on, every
	// added field still passes through the recorder's redaction+truncation pipeline before storage.
	TraceDetail bool

	stepPrefix string
	rootWF     *spec.WorkflowResource
}

// RunInput identifies the workflow run and parsed input map (already JSON-valid).
type RunInput struct {
	RunID           string
	WorkflowName    string
	Env             string
	StartedAt       time.Time
	Input           map[string]any
	ApprovedActions []string
	// Resume loads the latest checkpoint and continues from the next step (issue #105).
	Resume bool
	// Hitl carries operator decisions for approval gates (issue #106).
	Hitl HitlRunOptions
	// MaxConcurrentSteps bounds goroutine fan-out for a Graph/Fork/parallel Loop
	// (issue #192). Zero uses the interpreter's default concurrency.
	MaxConcurrentSteps int
	// WorkflowDepth is 0 for the entry run and increments for each workflow: step (issue #194).
	WorkflowDepth int
	// CallStack is callee workflow names from the root (issue #194 traces).
	CallStack []string
	// Attribution for OTel gen_ai attributes (issue #111).
	TenantID  string
	ThreadID  string
	ActorID   string
	RequestID string
}

func (e *Executor) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Executor) modelClient(modelRef string) (models.ModelClient, string, error) {
	if e.ModelResolve != nil {
		return e.ModelResolve(modelRef)
	}
	if e.Models == nil {
		return nil, "", fmt.Errorf("engine: Models registry is nil")
	}
	return e.Models.ClientFor(modelRef)
}

// Run executes a workflow: interpolate step inputs, policy checks, tool/agent calls,
// optional JSON Schema validation for agent output, persisted run_steps and trace events.
// Independent steps (graph mode via `needs:`) run concurrently with a concurrency bound;
// workflows with no `needs:` keep YAML-order sequential execution (issue #192).
// The run row must already exist in [state.RuntimeStore] (e.g. via [state.RuntimeStore.StartRun]).
func (e *Executor) Run(ctx context.Context, in RunInput) (err error) {
	if e == nil || e.Store == nil {
		return fmt.Errorf("engine: nil executor or store")
	}
	if e.Graph == nil {
		return fmt.Errorf("engine: nil project graph")
	}
	wf, err := lookupWorkflow(e.Graph, in.WorkflowName)
	if err != nil {
		return err
	}
	// Validates against the pinned schema bundle on resume, or the on-disk schema on a fresh run.
	if err := e.validateWorkflowInputSchema(wf, in.Input); err != nil {
		return e.failRun(ctx, in, err, 0)
	}

	wfPol, err := compiledWorkflowEvaluator(e.ProjectRoot, e.Graph, strings.TrimSpace(wf.Spec.Policy), e.PinnedGraph)
	if err != nil {
		return e.failRun(ctx, in, err, 0)
	}

	// Every run executes on the execir interpreter: issue #278 retired the
	// WorkflowStep DAG runtime, so both ingress paths (YAML and `.agent`) converge
	// on one interpreter (ADR 002 §5). A resume requires an execir checkpoint; a
	// pre-execir (DAG) checkpoint is not resumable — the DAG runtime is gone — so
	// it fails loudly rather than routing to a runtime that no longer exists.
	var resumeOtel *telemetry.SpanRef
	if in.Resume {
		otel, merr := e.execResumeMeta(ctx, in.RunID)
		if merr != nil {
			return merr
		}
		resumeOtel = otel
	}

	var runHandle *telemetry.RunHandle
	if e.Telemetry != nil && e.Telemetry.Enabled() {
		var link *telemetry.SpanRef
		if in.Resume {
			link = resumeOtel
		}
		actorID := strings.TrimSpace(in.ActorID)
		if actorID == "" {
			actorID = strings.TrimSpace(in.Hitl.Actor)
		}
		runHandle = e.Telemetry.BeginRun(ctx, telemetry.RunStartAttrs{
			RunID:     in.RunID,
			Workflow:  in.WorkflowName,
			AgentName: primaryAgentName(wf),
			TenantID:  in.TenantID,
			ThreadID:  in.ThreadID,
			ActorID:   actorID,
			RequestID: in.RequestID,
			Resume:    in.Resume,
			LinkFrom:  link,
		})
		if runHandle != nil {
			ctx = runHandle.Context()
		}
	}
	defer func() {
		if runHandle == nil {
			return
		}
		if errors.Is(err, ErrInterrupted) {
			runHandle.EndInterrupted()
			return
		}
		runHandle.End(err)
	}()

	runStartedAt := resumeRunStartedAt(ctx, e.Store, in)
	return e.runViaExecIR(ctx, in, wf, wfPol, runStartedAt, runHandle)
}

// finishRunWithOutput writes the final completed checkpoint and marks the run
// succeeded with a prebuilt output object. The execir path (control flow) builds
// its output from the interpreter's Return value (execIROutput) because the
// flattened resource output.value cannot address a taken arm (#259).
func (e *Executor) finishRunWithOutput(ctx context.Context, in RunInput, wf *spec.WorkflowResource, ictx Context, totalCost float64, finalOut map[string]any) error {
	// The runs table's output_json is a display value (served by inspect / state show), not a replay
	// source — the final checkpoint below keeps the raw context for any resume. Redact it so a sensitive
	// value flowing into output.value is not served in clear, the same leak class as run_steps (#408).
	outBytes := redactPayloadJSON(finalOut, e.Trace)
	if err := e.saveCheckpoint(ctx, wf, in.RunID, len(wf.Spec.Steps)-1, "", ictx, totalCost, state.CheckpointStatusCompleted); err != nil {
		return e.failRun(ctx, in, fmt.Errorf("engine: final checkpoint: %w", err), totalCost)
	}
	return e.Store.FinishRun(ctx, in.RunID, state.RunStatusSucceeded, e.now(), string(outBytes), "", totalCost)
}

func primaryAgentName(wf *spec.WorkflowResource) string {
	if wf == nil {
		return ""
	}
	for _, step := range wf.Spec.Steps {
		if n := strings.TrimSpace(step.Agent); n != "" {
			return n
		}
	}
	return ""
}

func (e *Executor) failRun(ctx context.Context, in RunInput, runErr error, totalCost float64) error {
	wf, _ := lookupWorkflow(e.Graph, in.WorkflowName)
	ictx := Context{Input: in.Input, Steps: map[string]StepResult{}}
	_ = e.saveCheckpoint(ctx, wf, in.RunID, -1, "", ictx, totalCost, state.CheckpointStatusFailed)
	finishAt := e.now()
	_ = e.Store.FinishRun(ctx, in.RunID, state.RunStatusFailed, finishAt, "", runErr.Error(), totalCost)
	return runErr
}

func (e *Executor) failRunStep(ctx context.Context, in RunInput, stepID string, with map[string]any, runErr error, totalCost float64) error {
	inJSON, _ := json.Marshal(with)
	now := e.now()
	_ = e.Store.UpsertRunStep(ctx, state.RunStep{
		RunID:      in.RunID,
		StepID:     stepID,
		Status:     "failed",
		StartedAt:  &now,
		FinishedAt: &now,
		InputJSON:  string(inJSON),
		ErrorText:  runErr.Error(),
	})
	if e.Trace != nil {
		_, _ = e.Trace.Append(ctx, in.RunID, stepID, trace.EventRunError, trace.ActorSystem, runErrorTraceData(stepID, runErr))
	}
	return e.failRun(ctx, in, runErr, totalCost)
}

func runErrorTraceData(stepID string, err error) map[string]any {
	data := map[string]any{"stepId": stepID}
	if err != nil {
		data["error"] = err.Error()
	}
	if d, ok := policy.AsDenied(err); ok {
		data["reason"] = d.Reason
	}
	return data
}
