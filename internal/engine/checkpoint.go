package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/render"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/telemetry"
)

// ErrInterrupted is returned when a run pauses at an approval gate or stub interrupt (issue #105).
// Callers should treat this as a clean exit, not a failure.
var ErrInterrupted = errors.New("engine: run interrupted")

const (
	checkpointPayloadVersion  = 1
	maxCheckpointContextBytes = 4 << 20 // absolute load cap (anti-DoS); write path uses resolved limits (#117)
	maxCheckpointSteps        = 256
)

// checkpointPayload is the engine-owned snapshot stored in run_checkpoints.context_json.
type checkpointPayload struct {
	Version       int                   `json:"version"`
	Input         any                   `json:"input"`
	Steps         map[string]StepResult `json:"steps"`
	Completed     []string              `json:"completed,omitempty"`
	TotalCostUSD  float64               `json:"totalCostUsd"`
	PendingHitl   *PendingHitlState     `json:"pendingHitl,omitempty"`
	OtelInterrupt *telemetry.SpanRef    `json:"otelInterrupt,omitempty"`
	Nested        *NestedRunState       `json:"nested,omitempty"`
	// ExecIR marks a checkpoint written by the execir run path (issue #258), so a
	// resume routes back to that path. ExecMemo/ExecControl carry the interpreter's
	// durable state (completed-leaf memo keyed by execir.CallKey, and pure
	// control-flow records) so replay skips re-invoking completed leaves. All
	// omitempty — a DAG checkpoint never sets them (backward compatible).
	ExecIR      bool           `json:"execIR,omitempty"`
	ExecMemo    map[string]any `json:"execMemo,omitempty"`
	ExecControl map[string]int `json:"execControl,omitempty"`
}

// NestedRunState is stacked in-flight subworkflow progress (issue #194).
type NestedRunState struct {
	StepID   string `json:"stepId"`
	Workflow string `json:"workflow"`
	// Input is the callee's whole input document: any JSON value, not only an
	// object (a single-parameter callee may receive a scalar or array). A null
	// document round-trips as nil, and resume falls back to the call's args.
	Input       any                   `json:"input"`
	Steps       map[string]StepResult `json:"steps"`
	Completed   []string              `json:"completed,omitempty"`
	PendingHitl *PendingHitlState     `json:"pendingHitl,omitempty"`
	Nested      *NestedRunState       `json:"nested,omitempty"`
	// ExecKey/ExecMemo/ExecControl carry a suspended subworkflow's execir durable
	// state on the execir run path (issue #270): ExecKey anchors this frame to the
	// parent's InvokeWorkflow CallSite, and the memo/control seed the callee's
	// interpreter on resume so its completed inner steps replay, never re-run. All
	// omitempty — a DAG nested checkpoint never sets them (backward compatible).
	ExecKey     string         `json:"execKey,omitempty"`
	ExecMemo    map[string]any `json:"execMemo,omitempty"`
	ExecControl map[string]int `json:"execControl,omitempty"`
}

// rootCheckpointInput normalizes the ROOT run's input document for a checkpoint:
// the root input is always a JSON object, so an absent (nil interface or nil map)
// document is written as {}. This must not be applied to a nested frame's Input,
// which may legitimately be any JSON value including null.
func rootCheckpointInput(in any) any {
	if m, ok := in.(map[string]any); ok && m == nil {
		return map[string]any{}
	}
	if in == nil {
		return map[string]any{}
	}
	return in
}

func completedStepIDs(steps map[string]StepResult) []string {
	ids := make([]string, 0, len(steps))
	for id := range steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func marshalCheckpointPayload(ictx Context, totalCost float64) (string, error) {
	payload := checkpointPayload{
		Version:       checkpointPayloadVersion,
		Input:         ictx.Input,
		Steps:         ictx.Steps,
		Completed:     completedStepIDs(ictx.Steps),
		TotalCostUSD:  totalCost,
		PendingHitl:   ictx.PendingHitl,
		OtelInterrupt: ictx.OtelInterrupt,
		Nested:        ictx.Nested,
	}
	payload.Input = rootCheckpointInput(payload.Input)
	if payload.Steps == nil {
		payload.Steps = map[string]StepResult{}
	}
	b, err := render.MarshalStableJSON(payload)
	if err != nil {
		return "", fmt.Errorf("engine: marshal checkpoint: %w", err)
	}
	if len(b) > maxCheckpointContextBytes {
		return "", fmt.Errorf("engine: checkpoint context exceeds absolute maximum %d bytes", maxCheckpointContextBytes)
	}
	return string(b), nil
}

func unmarshalCheckpointPayload(contextJSON string, g *spec.ProjectGraph, wf *spec.WorkflowResource, completedStepIndex int) (Context, float64, error) {
	if len(contextJSON) > maxCheckpointContextBytes {
		return Context{}, 0, fmt.Errorf("engine: checkpoint context exceeds %d bytes", maxCheckpointContextBytes)
	}
	var payload checkpointPayload
	if err := json.Unmarshal([]byte(contextJSON), &payload); err != nil {
		return Context{}, 0, fmt.Errorf("engine: unmarshal checkpoint: %w", err)
	}
	if payload.Version != checkpointPayloadVersion {
		return Context{}, 0, fmt.Errorf("engine: unsupported checkpoint version %d", payload.Version)
	}
	payload.Input = rootCheckpointInput(payload.Input)
	if payload.Steps == nil {
		payload.Steps = map[string]StepResult{}
	}
	if len(payload.Steps) > maxCheckpointSteps {
		return Context{}, 0, fmt.Errorf("engine: checkpoint has too many steps (%d)", len(payload.Steps))
	}
	// validateCheckpointSteps enforces the DAG-era invariant that every completed
	// step id is a known WorkflowStep, ordered by step index. That does not fit the
	// execir path: under control flow a leaf's binding name legitimately differs
	// from its flattened resource step id (lowerControlAssign gives a fresh id), and
	// under a `while` the same binding recurs every iteration (#290). The authoritative
	// durable state of an execir checkpoint is ExecMemo/ExecControl — replayed and
	// determinism-checked by the interpreter — so the (output/interpolation-only)
	// Steps map is not validated against WorkflowStep membership here. The size and
	// count caps above still apply.
	if !payload.ExecIR {
		if err := validateCheckpointSteps(payload.Steps, wf, completedStepIndex, payload.Completed); err != nil {
			return Context{}, 0, err
		}
	}
	maxNest := spec.DefaultMaxWorkflowNesting
	if g != nil && wf != nil {
		maxNest = spec.ResolveMaxWorkflowNesting(&g.Spec, &wf.Spec)
	}
	if err := validateNestedRunState(payload.Nested, g, wf, maxNest); err != nil {
		return Context{}, 0, err
	}
	if payload.TotalCostUSD < 0 {
		return Context{}, 0, fmt.Errorf("engine: negative totalCostUsd in checkpoint")
	}
	return Context{
		Input: payload.Input, Steps: payload.Steps,
		PendingHitl: payload.PendingHitl, OtelInterrupt: payload.OtelInterrupt,
		Nested: payload.Nested,
	}, payload.TotalCostUSD, nil
}

func validateNestedRunState(n *NestedRunState, g *spec.ProjectGraph, parent *spec.WorkflowResource, maxNest int) error {
	depth := 0
	for n != nil {
		depth++
		if maxNest > 0 && depth > maxNest {
			return fmt.Errorf("engine: nested checkpoint exceeds maxWorkflowNesting %d", maxNest)
		}
		if strings.TrimSpace(n.StepID) == "" || strings.TrimSpace(n.Workflow) == "" {
			return fmt.Errorf("engine: nested checkpoint missing stepId or workflow")
		}
		if len(n.Steps) > maxCheckpointSteps {
			return fmt.Errorf("engine: nested checkpoint has too many steps (%d)", len(n.Steps))
		}
		callee, err := nestedCalleeWorkflow(g, parent, n)
		if err != nil {
			return err
		}
		// An execir nested frame (ExecKey set) carries its authoritative durable
		// state in ExecMemo/ExecControl; its Steps map is output/interpolation-only,
		// and under control flow a leaf's binding name legitimately differs from the
		// callee's WorkflowStep ids (and recurs across `while` iterations). The
		// DAG-era membership check therefore does not apply — the same relaxation as
		// the top-level checkpoint (#290). The size cap above and the stepId/workflow
		// and nesting guards still hold. Since #278 every nested subworkflow runs on
		// execir, so a freshly written nested frame always sets ExecKey.
		if n.ExecKey == "" {
			completed := append([]string(nil), n.Completed...)
			completed = append(completed, completedStepIDs(n.Steps)...)
			if err := validateCheckpointSteps(n.Steps, callee, -1, completed); err != nil {
				return fmt.Errorf("engine: nested checkpoint workflow %q: %w", n.Workflow, err)
			}
		}
		parent = callee
		n = n.Nested
	}
	return nil
}

func nestedCalleeWorkflow(g *spec.ProjectGraph, parent *spec.WorkflowResource, n *NestedRunState) (*spec.WorkflowResource, error) {
	if parent == nil {
		return nil, fmt.Errorf("engine: nested checkpoint missing parent workflow")
	}
	stepID := strings.TrimSpace(n.StepID)
	calleeName := strings.TrimSpace(n.Workflow)
	var found *spec.WorkflowStep
	for i := range parent.Spec.Steps {
		st := &parent.Spec.Steps[i]
		if strings.TrimSpace(st.ID) == stepID {
			found = st
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("engine: nested checkpoint step %q is not in workflow %q", stepID, parent.Metadata.Name)
	}
	if strings.TrimSpace(found.Workflow) == "" {
		return nil, fmt.Errorf("engine: nested checkpoint step %q is not a workflow: call", stepID)
	}
	if strings.TrimSpace(found.Workflow) != calleeName {
		return nil, fmt.Errorf("engine: nested checkpoint step %q calls %q, not %q", stepID, strings.TrimSpace(found.Workflow), calleeName)
	}
	if g == nil || g.Workflows == nil {
		return nil, fmt.Errorf("engine: nested checkpoint cannot resolve workflow %q", calleeName)
	}
	callee, ok := g.Workflows[calleeName]
	if !ok || callee == nil {
		return nil, fmt.Errorf("engine: nested checkpoint unknown workflow %q", calleeName)
	}
	return callee, nil
}

func validateCheckpointSteps(steps map[string]StepResult, wf *spec.WorkflowResource, completedStepIndex int, completedIDs []string) error {
	if wf == nil {
		return fmt.Errorf("engine: nil workflow for checkpoint validation")
	}
	known := make(map[string]struct{}, len(wf.Spec.Steps))
	for _, st := range wf.Spec.Steps {
		id := strings.TrimSpace(st.ID)
		if id != "" {
			known[id] = struct{}{}
		}
	}
	allowed := make(map[string]struct{})
	if len(completedIDs) > 0 {
		for _, id := range completedIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := known[id]; !ok {
				return fmt.Errorf("engine: checkpoint completed unknown step %q", id)
			}
			allowed[id] = struct{}{}
		}
	} else {
		for i := 0; i <= completedStepIndex && i < len(wf.Spec.Steps); i++ {
			id := strings.TrimSpace(wf.Spec.Steps[i].ID)
			if id != "" {
				allowed[id] = struct{}{}
			}
		}
	}
	for stepID := range steps {
		if _, ok := known[stepID]; !ok {
			return fmt.Errorf("engine: checkpoint references unknown or future step %q", stepID)
		}
		if _, ok := allowed[stepID]; !ok {
			return fmt.Errorf("engine: checkpoint references unknown or future step %q", stepID)
		}
	}
	return nil
}

func (e *Executor) saveCheckpoint(ctx context.Context, wf *spec.WorkflowResource, runID string, stepIndex int, stepID string, ictx Context, totalCost float64, status string) error {
	if e != nil {
		if e.rootWF != nil {
			wf = e.rootWF
		}
		stepID = e.qualID(stepID)
	}
	ctxJSON, err := marshalCheckpointPayload(ictx, totalCost)
	if err != nil {
		return err
	}
	if err := e.enforceCheckpointSize(ctx, wf, runID, stepID, ctxJSON); err != nil {
		return err
	}
	return e.Store.SaveCheckpoint(ctx, state.RunCheckpoint{
		RunID:       runID,
		StepIndex:   stepIndex,
		StepID:      stepID,
		ContextJSON: ctxJSON,
		Status:      status,
		CreatedAt:   e.now(),
	})
}

// execResumeMeta loads the latest checkpoint for a resume, verifies it was written
// by the execir run path (the only run path since #278 retired the DAG), and
// returns its OTel interrupt link for the resumed run's telemetry span. A legacy
// pre-execir (DAG) checkpoint — one without the ExecIR marker — is NOT resumable:
// the DAG runtime is gone, so resume fails loudly instead of routing to a runtime
// that no longer exists. There is no DAG→execir checkpoint migration; a run
// interrupted before the upgrade must be started anew.
func (e *Executor) execResumeMeta(ctx context.Context, runID string) (*telemetry.SpanRef, error) {
	cp, err := e.Store.GetLatestCheckpoint(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("engine: load checkpoint: %w", err)
	}
	switch cp.Status {
	case state.CheckpointStatusRunning, state.CheckpointStatusInterrupted:
	default:
		return nil, fmt.Errorf("engine: checkpoint status %q is not resumable", cp.Status)
	}
	var payload checkpointPayload
	if err := json.Unmarshal([]byte(cp.ContextJSON), &payload); err != nil {
		return nil, fmt.Errorf("engine: unmarshal checkpoint: %w", err)
	}
	if !payload.ExecIR {
		return nil, fmt.Errorf("engine: run %q has a pre-execir (WorkflowStep DAG) checkpoint; the DAG runtime was retired (#278) and legacy checkpoints are not resumable — start a new run", runID)
	}
	return payload.OtelInterrupt, nil
}

// resumeRunStartedAt returns StartedAt for resumed runs, using the original run row when available.
func resumeRunStartedAt(ctx context.Context, store state.RuntimeStore, in RunInput) time.Time {
	if !in.Resume || store == nil {
		return in.StartedAt
	}
	run, err := store.GetRun(ctx, in.RunID)
	if err != nil || run == nil {
		return in.StartedAt
	}
	return run.StartedAt
}
