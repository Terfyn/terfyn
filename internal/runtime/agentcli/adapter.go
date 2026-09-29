package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Terfyn/terfyn/internal/config"
	"github.com/Terfyn/terfyn/internal/engine"
	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/jsonnum"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/runtime"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
	"github.com/Terfyn/terfyn/internal/util"
)

// RuntimeAdapter is the CLI-agnostic runtime.Runtime that every external target shares. It resolves
// the single agent a workflow drives, stands up the per-run MCP server, spawns the external agent
// through its driver (the only per-CLI seam), folds turns into the audit chain, and keeps Terfyn the
// enforcer of record for the budget. A concrete adapter (claudecode, gemini, …) is just this type
// wired with its runtime Name and its AgentRuntime driver.
type RuntimeAdapter struct {
	name   string
	driver AgentRuntime
	deps   runtime.Deps
}

// NewRuntimeAdapter builds the shared adapter for a named external runtime and its driver.
func NewRuntimeAdapter(name string, driver AgentRuntime, deps runtime.Deps) *RuntimeAdapter {
	return &RuntimeAdapter{name: name, driver: driver, deps: deps}
}

func (a *RuntimeAdapter) now() time.Time {
	if a != nil && a.deps.Now != nil {
		return a.deps.Now()
	}
	return time.Now().UTC()
}

// ErrResumePending reports that resuming an external-runtime run is not yet wired (a #367
// follow-up); a fresh Invoke is supported.
var ErrResumePending = errors.New("agentcli: resume of an external-runtime run is not implemented yet (#367 follow-up)")

// Invoke drives a single-agent workflow through the external agent, constrained to the grant.
func (a *RuntimeAdapter) Invoke(ctx context.Context, cfg *config.ResolvedConfig, opts runtime.InvokeOptions) (runtime.RunResult, error) {
	if a == nil || a.deps.Store == nil {
		return runtime.RunResult{}, fmt.Errorf("agentcli: nil runtime or store")
	}
	if cfg == nil {
		return runtime.RunResult{}, fmt.Errorf("agentcli: nil resolved config")
	}
	graph := cfg.Graph()
	if graph == nil {
		return runtime.RunResult{}, fmt.Errorf("agentcli: resolved config has no graph")
	}
	root := cfg.ProjectRoot()

	wfName := strings.TrimSpace(opts.WorkflowName)
	wf, ok := graph.Workflows[wfName]
	if !ok || wf == nil {
		return runtime.RunResult{}, fmt.Errorf("agentcli: unknown workflow %q", wfName)
	}

	input := map[string]any{}
	if len(opts.InputJSON) > 0 {
		// Lossless decode (internal/jsonnum): the input is re-marshaled into the run
		// row below, and a float64 decode would round integers past 2^53 there.
		if err := jsonnum.Unmarshal(opts.InputJSON, &input); err != nil {
			return runtime.RunResult{}, fmt.Errorf("agentcli: invalid input JSON: %w", err)
		}
	}
	if err := engine.ValidateWorkflowInput(root, wf, input); err != nil {
		return runtime.RunResult{}, err
	}

	agent, err := resolveDrivenAgent(graph, wf, cfg.Executables())
	if err != nil {
		return runtime.RunResult{}, err
	}
	eval, err := workflowEvaluator(root, graph, wf.Spec.Policy)
	if err != nil {
		return runtime.RunResult{}, err
	}
	exec := tools.NewRegistryWithRoot(graph, root)

	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		runID = util.NewRunID()
	}
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return runtime.RunResult{}, fmt.Errorf("agentcli: marshal input: %w", err)
	}
	envLabel := strings.TrimSpace(opts.Env)
	if envLabel == "" {
		envLabel = "local"
	}

	started := a.now()
	attr := state.RunAttribution{
		TenantID: opts.TenantID, ThreadID: opts.ThreadID, ActorID: opts.ActorID,
		ParentRunID: opts.ParentRunID, RequestID: opts.RequestID,
		IdempotencyKey: opts.IdempotencyKey, Source: opts.Source,
	}
	if opts.RequireAttribution {
		if err := state.RequireExplicitAttribution(attr); err != nil {
			return runtime.RunResult{RunID: runID}, err
		}
	}
	// A fresh external run is not (yet) resumable, so no deployment snapshot is pinned (the S1
	// carve-out for an empty snapshot digest, docs/SOUNDNESS.md); authority comes from cfg's graph
	// at run start. Resume + snapshot pinning is a #367 follow-up.
	runRow := state.Run{
		RunID: runID, WorkflowName: wfName, Env: envLabel, Status: state.RunStatusRunning,
		StartedAt: started, InputJSON: string(inputBytes), EnvironmentName: strings.TrimSpace(opts.EnvironmentName),
	}
	state.ApplyAttribution(&runRow, attr)
	if err := a.deps.Store.StartRun(ctx, runRow); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("agentcli: start run: %w", err)
	}

	rec := trace.NewRecorderForGraph(a.deps.Store, graph)
	rec.Sink = opts.EventSink // stream events live when the caller opted in (terfyn run --verbose, #450)
	if _, err := rec.Append(ctx, runID, "", trace.EventRunStarted, trace.ActorAgent,
		map[string]any{"workflow": wfName, "environment": cfg.Environment(), "runtime": a.name, "agent": agent.Metadata.Name}); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("agentcli: trace run_started: %w", err)
	}

	cfgDir, err := os.MkdirTemp("", "terfyn-mcp-*")
	if err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("agentcli: temp dir: %w", err)
	}
	defer os.RemoveAll(cfgDir)

	session, runCtx, runErr := RunExternalAgent(ctx, a.driver, ExternalAgentRun{
		Graph:     graph,
		Agent:     agent,
		Eval:      eval,
		Exec:      exec,
		Recorder:  rec,
		RunID:     runID,
		Prompt:    string(inputBytes),
		Run:       policy.RunContext{StartedAt: started, ApprovedActions: opts.ApprovedActions},
		Limits:    MapLimits(agent.Spec.Constraints, policyExecution(eval)),
		ConfigDir: cfgDir,
	})

	finishedAt := a.now()
	if runErr != nil {
		_, _ = rec.Append(ctx, runID, "", trace.EventRunError, trace.ActorSystem, map[string]any{"error": runErr.Error()})
		_ = a.deps.Store.FinishRun(ctx, runID, state.RunStatusFailed, finishedAt, "", runErr.Error(), runCtx.AccumulatedCostUSD)
		return runtime.RunResult{RunID: runID}, runErr
	}
	outBytes, _ := json.Marshal(map[string]any{"text": session.FinalText})
	if _, err := rec.Append(ctx, runID, "", trace.EventRunFinished, trace.ActorAgent, map[string]any{"costUsd": runCtx.AccumulatedCostUSD}); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("agentcli: trace run_finished: %w", err)
	}
	if err := a.deps.Store.FinishRun(ctx, runID, state.RunStatusSucceeded, finishedAt, string(outBytes), "", runCtx.AccumulatedCostUSD); err != nil {
		return runtime.RunResult{RunID: runID}, fmt.Errorf("agentcli: finish run: %w", err)
	}
	return runtime.RunResult{RunID: runID}, nil
}

// Resume continues an external-runtime run from its checkpoint — a #367 follow-up.
func (a *RuntimeAdapter) Resume(_ context.Context, _ *config.ResolvedConfig, _ runtime.ResumeOptions) (runtime.RunResult, error) {
	return runtime.RunResult{}, ErrResumePending
}

// Health reports the adapter as healthy: it is wired to run. A missing CLI binary surfaces as a loud
// spawn error at run time rather than by rejecting the selector.
func (a *RuntimeAdapter) Health(_ context.Context) runtime.HealthStatus {
	return runtime.HealthStatus{State: runtime.HealthOK, Details: a.name + " external runtime (resume pending, #367)"}
}

// resolveDrivenAgent returns the one agent a workflow drives externally, gating on the workflow's
// EXECUTABLE (execir.Program), not a count of distinct agent names. The external agent runs its OWN
// bounded loop (the CLI's --max-turns); Terfyn does not orchestrate a workflow's steps through it. So
// the only faithful shape is exactly ONE unconditional agent invocation: any control flow
// (if/for/while/retry/parallel), a second agent, or a tool / subworkflow / approval step would be
// silently dropped by spawning the agent once — changing the run's success/failure outcome versus
// the internal engine (e.g. a `retry until … limit N` whose fail-on-exhaustion never runs). Those
// are refused loudly. The whitelist is InvokeAgent plus binding/return plumbing (Let, Return);
// every other node kind — including any that requires the interpreter — is rejected.
func resolveDrivenAgent(graph *spec.ProjectGraph, wf *spec.WorkflowResource, execs map[string]*execir.Program) (*spec.AgentResource, error) {
	name := wf.Metadata.Name
	prog := execs[name]
	if prog == nil {
		return nil, fmt.Errorf("agentcli: workflow %q has no executable program; the external runtime supports only a single-agent workflow (one unconditional agent invocation)", name)
	}
	agentName := ""
	agentInvokes := 0
	for _, n := range prog.Body {
		switch v := n.(type) {
		case *execir.InvokeAgent:
			agentInvokes++
			agentName = v.Agent
		case *execir.Let, *execir.Return:
			// Alias binding / workflow return — no invocation, no orchestration.
		default:
			return nil, fmt.Errorf("agentcli: workflow %q is not a single-agent workflow — it contains a %T step; the external runtime supports exactly one unconditional agent invocation (no tools, subworkflows, approvals, or control flow)", name, n)
		}
	}
	if agentInvokes != 1 {
		return nil, fmt.Errorf("agentcli: workflow %q drives %d agent invocations; the external runtime supports a single-agent workflow (exactly one unconditional agent invocation)", name, agentInvokes)
	}
	if ar := graph.Agents[agentName]; ar != nil {
		return ar, nil
	}
	return nil, fmt.Errorf("agentcli: workflow %q references unknown agent %q", name, agentName)
}

// policyExecution returns the compiled policy's execution block (maxTotalCostUsd, maxWallClockSeconds)
// so [MapLimits] can derive the external run's --max-budget-usd belt from the governing policy, not
// only the agent constraints (issue #389). Every evaluator in this codebase exposes PolicySpec(),
// but it is not part of the [policy.PolicyEvaluator] interface, so this reads it structurally and
// falls back to nil (no policy-derived budget) when unavailable.
func policyExecution(eval policy.PolicyEvaluator) *spec.PolicyExecution {
	ps, ok := eval.(interface {
		PolicySpec() *spec.PolicySpec
	})
	if !ok {
		return nil
	}
	polSpec := ps.PolicySpec()
	if polSpec == nil {
		return nil
	}
	return polSpec.Execution
}

// workflowEvaluator builds the policy evaluator that governs the run's tool calls, mirroring the
// engine's non-pinned path: the workflow's policy (or the project default), compiled from the
// on-disk snapshot when present else from the graph. A run with no default policy still gets a
// closed-world / safety evaluator.
func workflowEvaluator(projectRoot string, graph *spec.ProjectGraph, policyName string) (policy.PolicyEvaluator, error) {
	policyName = strings.TrimSpace(policyName)
	if policyName == "" {
		policyName = strings.TrimSpace(policy.DefaultPolicyName(graph))
	}
	if policyName == "" {
		return policy.NewEvaluator(graph, nil), nil
	}
	if root := strings.TrimSpace(projectRoot); root != "" {
		if stored, err := policy.ReadSnapshotSet(root); err == nil && stored != nil {
			cp, err := policy.CompiledPolicyForName(root, graph, policyName)
			if err != nil {
				return nil, err
			}
			return policy.NewCompiledEvaluator(graph, cp), nil
		}
	}
	cp, err := policy.Compile(graph, policyName)
	if err != nil {
		return nil, err
	}
	return policy.NewCompiledEvaluator(graph, cp), nil
}
