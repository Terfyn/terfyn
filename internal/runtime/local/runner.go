package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/config"
	"github.com/Terfyn/terfyn/internal/engine"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/plan"
	"github.com/Terfyn/terfyn/internal/runtime"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/telemetry"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
	"github.com/Terfyn/terfyn/internal/util"
)

// Invoke validates input, persists the run row, and executes the workflow engine.
func (r *Runtime) Invoke(ctx context.Context, cfg *config.ResolvedConfig, opts runtime.InvokeOptions) (runtime.RunResult, error) {
	if r == nil || r.Store == nil {
		return runtime.RunResult{}, fmt.Errorf("local: nil runtime or store")
	}
	prep, err := r.prepareFromConfig(ctx, cfg)
	if err != nil {
		return runtime.RunResult{}, err
	}

	wfName := strings.TrimSpace(opts.WorkflowName)
	if wfName == "" {
		return runtime.RunResult{}, fmt.Errorf("local: empty workflow name")
	}
	wf, ok := prep.graph.Workflows[wfName]
	if !ok || wf == nil {
		return runtime.RunResult{}, fmt.Errorf("local: unknown workflow %q", wfName)
	}

	var input map[string]any
	if len(opts.InputJSON) == 0 {
		input = map[string]any{}
	} else {
		if err := json.Unmarshal(opts.InputJSON, &input); err != nil {
			return runtime.RunResult{}, fmt.Errorf("local: invalid input JSON: %w", err)
		}
	}
	if err := engine.ValidateWorkflowInput(prep.root, wf, input); err != nil {
		return runtime.RunResult{}, err
	}

	// The #118 run-start-vs-resume drift check now commits to the workflow's
	// execution IR too (#277): fold the pinned program's digest into the stored
	// hash so a lowering-only change between run-start and resume is drift, not
	// invisible to the resource-only projection. On the pinned-snapshot path resume
	// recomputes from the SAME hydrated program, so this never false-positives; a
	// workflow with no lowered program folds "" and keeps the historical
	// resource-only hash. This is its own hash space, distinct from the plan/apply
	// applied_resources spec_hash (#260); a run row seeded with a bare hash still
	// resumes (validateResumeWorkflowSpec accepts the bare fallback).
	wfHash, err := plan.WorkflowSpecHashWithExec(wf, workflowExecDigest(prep.executables, wfName))
	if err != nil {
		return runtime.RunResult{}, err
	}

	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		runID = util.NewRunID()
	}

	inputBytes, err := json.Marshal(input)
	if err != nil {
		return runtime.RunResult{}, fmt.Errorf("local: marshal input: %w", err)
	}

	envLabel := strings.TrimSpace(opts.Env)
	if envLabel == "" {
		envLabel = "local"
	}

	// Pin the deployment snapshot (#207): resume enforces this exact configuration and authority,
	// not whatever is deployed at resume time. Uses the effective environment label (cfg.Environment)
	// so an identical config applied and run under the same env dedupes to one snapshot.
	snapshotDigest, snapWarnings, err := r.pinDeploymentSnapshot(ctx, prep.graph, cfg.Environment(), prep.root, prep.executables)
	if err != nil {
		return runtime.RunResult{}, err
	}

	started := r.now()
	attr := state.RunAttribution{
		TenantID:       opts.TenantID,
		ThreadID:       opts.ThreadID,
		ActorID:        opts.ActorID,
		ParentRunID:    opts.ParentRunID,
		RequestID:      opts.RequestID,
		IdempotencyKey: opts.IdempotencyKey,
		Source:         opts.Source,
	}
	if opts.RequireAttribution {
		if err := state.RequireExplicitAttribution(attr); err != nil {
			return runtime.RunResult{RunID: runID}, err
		}
	}
	runRow := state.Run{
		RunID:                    runID,
		WorkflowName:             wfName,
		Env:                      envLabel,
		Status:                   state.RunStatusRunning,
		StartedAt:                started,
		InputJSON:                string(inputBytes),
		TotalCostUSD:             0,
		WorkflowSpecHash:         wfHash,
		EnvironmentName:          strings.TrimSpace(opts.EnvironmentName),
		DeploymentSnapshotDigest: snapshotDigest,
	}
	state.ApplyAttribution(&runRow, attr)
	if err := r.Store.StartRun(ctx, runRow); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: start run: %w", err)
	}

	rec := trace.NewRecorderForGraph(r.Store, prep.graph)
	rec.Sink = opts.EventSink // stream events live when the caller opted in (terfyn run --verbose, #450)
	if opts.TraceDetail {
		// --trace-detail surfaces substance (diffs, reasoning, output); raise the truncation ceilings so
		// the terse audit defaults don't shred it (#525). Redaction (secret masking) is unchanged.
		rec.Redaction = trace.ApplyDetailBudget(rec.Redaction)
	}
	startedData := map[string]any{"workflow": wfName, "environment": cfg.Environment()}
	if snapshotDigest != "" {
		// Cover the pinned deployment identity by the run's tamper-evident audit chain (#207): the
		// event sequence now proves not just what happened but what configuration and authority ran.
		startedData["deploymentSnapshot"] = snapshotDigest
	}
	if _, err := rec.Append(ctx, runID, "", trace.EventRunStarted, trace.ActorAgent, startedData); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: trace run_started: %w", err)
	}

	runCfg := engineRunConfigFromInvoke(opts)
	_, runErr := r.executeEngine(ctx, prep, runID, wfName, envLabel, started, input, runCfg, false, state.AttributionFromRun(&runRow), rec)
	return runtime.RunResult{RunID: runID, Warnings: snapWarnings}, runErr
}

// Resume continues an existing run from its latest checkpoint.
func (r *Runtime) Resume(ctx context.Context, cfg *config.ResolvedConfig, opts runtime.ResumeOptions) (runtime.RunResult, error) {
	if r == nil || r.Store == nil {
		return runtime.RunResult{}, fmt.Errorf("local: nil runtime or store")
	}
	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		return runtime.RunResult{}, fmt.Errorf("local: resume requires run id")
	}

	run, err := r.Store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.RunResult{RunID: runID}, fmt.Errorf("local: run %q not found", runID)
		}
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: get run: %w", err)
	}
	// Only an interrupted run is resumable. A `running` run is either executing in another process or a
	// resume already claimed it — resuming it again would re-execute the remaining (possibly
	// side-effecting) steps once per process (issue #407, S7). The authoritative guard is the
	// compare-and-set claim below; this is the early, friendly rejection.
	switch run.Status {
	case state.RunStatusInterrupted:
	default:
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: run %q status %q is not resumable", runID, run.Status)
	}

	if _, err := r.Store.GetLatestCheckpoint(ctx, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.RunResult{RunID: runID}, fmt.Errorf("local: run %q has no checkpoint", runID)
		}
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: load checkpoint: %w", err)
	}

	if _, err := resolveConfigForResume(run, opts.EnvironmentName); err != nil {
		return runtime.RunResult{RunID: runID}, err
	}

	prep, pinned, err := r.prepareForResume(ctx, run, cfg)
	if err != nil {
		return runtime.RunResult{RunID: runID}, err
	}

	wfName := strings.TrimSpace(run.WorkflowName)
	wf, ok := prep.graph.Workflows[wfName]
	if !ok || wf == nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: unknown workflow %q", wfName)
	}
	if err := validateResumeWorkflowSpec(run, wf, workflowExecDigest(prep.executables, wfName)); err != nil {
		return runtime.RunResult{RunID: runID}, err
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(run.InputJSON), &input); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: invalid stored input JSON: %w", err)
	}
	if input == nil {
		input = map[string]any{}
	}
	// On the pinned-snapshot path the stored input was already validated at run start against the
	// pinned schema; re-validating would re-read possibly-changed schema files, reintroducing the
	// drift the snapshot exists to prevent. Only re-validate on the legacy (unpinned) fallback.
	if !pinned {
		if err := engine.ValidateWorkflowInput(prep.root, wf, input); err != nil {
			return runtime.RunResult{RunID: runID}, err
		}
	}

	// Claim the run with a compare-and-set (interrupted → running): only the first of two concurrent
	// resumes wins, so the second is rejected here instead of both executing the remaining steps once
	// each (issue #407). All the work above is read-only, so racing resumes reaching this point is safe;
	// this is the single serialization point.
	claimed, err := r.Store.ClaimRunForResume(ctx, runID)
	if err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: claim run for resume: %w", err)
	}
	if !claimed {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: run %q is already being resumed by another process (not resumable)", runID)
	}

	rec := trace.NewRecorderForGraph(r.Store, prep.graph)
	rec.Sink = opts.EventSink // stream events live when the caller opted in (terfyn run --verbose, #450)
	if opts.TraceDetail {
		rec.Redaction = trace.ApplyDetailBudget(rec.Redaction) // raise truncation ceilings for detail (#525)
	}
	if _, err := rec.Append(ctx, runID, "", trace.EventRunStarted, trace.ActorAgent, map[string]any{
		"workflow": wfName,
		"resumed":  true,
	}); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("local: trace run_started (resumed): %w", err)
	}

	envLabel := strings.TrimSpace(run.Env)
	if envLabel == "" {
		envLabel = "local"
	}

	runCfg := engineRunConfigFromResume(opts)
	_, runErr := r.executeEngine(ctx, prep, runID, wfName, envLabel, run.StartedAt, input, runCfg, true, state.AttributionFromRun(run), rec)
	return runtime.RunResult{RunID: runID}, runErr
}

func (r *Runtime) executeEngine(
	ctx context.Context,
	prep *preparedProject,
	runID, wfName, envLabel string,
	started time.Time,
	input map[string]any,
	cfg engineRunConfig,
	resume bool,
	attr state.RunAttribution,
	rec *trace.Recorder,
) (string, error) {
	tel := telemetry.NewTracer(telemetry.ConfigFromGraph(prep.graph), r.agentVersion())
	defer tel.Shutdown()

	ex := &engine.Executor{
		Graph:       prep.graph,
		ProjectRoot: prep.root,
		PinnedGraph: prep.pinned,
		Schemas:     prep.schemas,
		Executables: prep.executables,
		Tools:       tools.NewRegistryWithRoot(prep.graph, prep.root),
		Models:      models.NewRegistry(prep.graph),
		Store:       r.Store,
		Trace:       rec,
		Telemetry:   tel,
		Now:         r.Now,
		TraceDetail: cfg.traceDetail,
	}
	hitl, err := buildEngineHitlOptions(cfg)
	if err != nil {
		return runID, err
	}
	state.NormalizeAttribution(&attr)

	runErr := ex.Run(ctx, engine.RunInput{
		RunID:           runID,
		WorkflowName:    wfName,
		Env:             envLabel,
		StartedAt:       started,
		Input:           input,
		ApprovedActions: cfg.approvedActions,
		Resume:          resume,
		Hitl:            hitl,
		TenantID:        attr.TenantID,
		ThreadID:        attr.ThreadID,
		ActorID:         attr.ActorID,
		RequestID:       attr.RequestID,
	})

	finData := map[string]any{}
	if runErr != nil {
		if errors.Is(runErr, engine.ErrInterrupted) {
			return runID, nil
		}
		finData["error"] = runErr.Error()
		if _, terr := rec.Append(ctx, runID, "", trace.EventRunError, trace.ActorSystem, finData); terr != nil && runErr == nil {
			return runID, fmt.Errorf("local: trace run_error: %w", terr)
		}
	}
	if _, terr := rec.Append(ctx, runID, "", trace.EventRunFinished, trace.ActorAgent, finData); terr != nil && runErr == nil {
		return runID, fmt.Errorf("local: trace run_finished: %w", terr)
	}
	return runID, runErr
}
