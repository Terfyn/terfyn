package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

func graphWithLimits(t *testing.T, projectLimits, toolLimits *spec.ExecutionLimits) *spec.ProjectGraph {
	t.Helper()
	g := echoOnlyWorkflow(t)
	g.Spec.Limits = projectLimits
	if tr := g.Tools["helper"]; tr != nil && toolLimits != nil {
		tr.Spec.Limits = toolLimits
	}
	return g
}

func echoOnlyWorkflow(t *testing.T) *spec.ProjectGraph {
	t.Helper()
	g := demoWorkflowGraph(t)
	g.Workflows["demo"].Spec.Steps = []spec.WorkflowStep{{
		ID:   "fetch",
		Uses: "tool.helper.echo",
		With: map[string]any{
			"topic": "${input.topic}",
			"extra": strings.Repeat("x", 500),
		},
	}}
	g.Workflows["demo"].Spec.Output = &spec.WorkflowOutput{
		Value: map[string]any{
			"topic": "${input.topic}",
			"echo":  "${steps.fetch.output.echo.topic}",
		},
	}
	return g
}

func startDemoRun(t *testing.T, st state.RuntimeStore, runID string, started time.Time) map[string]any {
	t.Helper()
	inJSON := `{"topic":"agents"}`
	if err := st.StartRun(context.Background(), state.Run{
		RunID: runID, WorkflowName: "demo", Env: "dev", Status: "running",
		StartedAt: started, InputJSON: inJSON,
	}); err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(inJSON), &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestRun_toolOutputTruncatedAtLimit(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := graphWithLimits(t, &spec.ExecutionLimits{
		MaxToolOutputBytes:     80,
		ToolOutputExceedPolicy: spec.LimitExceedTruncate,
	}, nil)

	runID := "run-truncate"
	started := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	input := startDemoRun(t, st, runID, started)

	ex := &Executor{
		Graph:       graph,
		ProjectRoot: testProjectRoot(t),
		Tools:       tools.NewRegistry(graph),
		Store:       st,
		Trace:       trace.NewRecorder(st),
	}
	if err := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true},
	}); err != nil {
		t.Fatal(err)
	}

	events, err := st.ListTraceEventsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var limitHits int
	for _, ev := range events {
		if ev.Type == trace.EventLimitHit.String() {
			limitHits++
			if !strings.Contains(ev.DataJSON, `"kind":"tool_output"`) {
				t.Fatalf("unexpected limit_hit payload: %s", ev.DataJSON)
			}
			if !strings.Contains(ev.DataJSON, `"truncated":true`) {
				t.Fatalf("expected truncated annotation: %s", ev.DataJSON)
			}
		}
	}
	if limitHits == 0 {
		t.Fatal("expected limit_hit trace event")
	}

	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.OutputJSON), &out); err != nil {
		t.Fatal(err)
	}
	if out["topic"] != "agents" {
		t.Fatalf("topic = %v", out["topic"])
	}
	if out["echo"] != "agents" {
		t.Fatalf("truncated output should preserve echo.topic, got %v", out["echo"])
	}
}

func countLimitHitEvents(events []state.TraceEvent, kind string) int {
	n := 0
	for _, ev := range events {
		if ev.Type != trace.EventLimitHit.String() {
			continue
		}
		if kind == "" || strings.Contains(ev.DataJSON, `"kind":"`+kind+`"`) {
			n++
		}
	}
	return n
}

func TestRun_toolOutputFailPolicy_emitsLimitHit(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "limits-fail-trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := graphWithLimits(t, &spec.ExecutionLimits{
		MaxToolOutputBytes:     80,
		ToolOutputExceedPolicy: spec.LimitExceedFail,
	}, nil)

	runID := "run-fail-trace"
	started := time.Date(2026, 6, 7, 12, 30, 0, 0, time.UTC)
	input := startDemoRun(t, st, runID, started)

	ex := &Executor{
		Graph: graph, ProjectRoot: testProjectRoot(t),
		Tools: tools.NewRegistry(graph), Store: st, Trace: trace.NewRecorder(st),
	}
	err = ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "tool_output exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	events, err := st.ListTraceEventsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if countLimitHitEvents(events, "tool_output") == 0 {
		t.Fatal("expected limit_hit for tool_output fail policy")
	}
}

func TestRun_toolInputTruncate(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "limits-in-trunc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := echoOnlyWorkflow(t)
	graph.Spec.Limits = &spec.ExecutionLimits{
		MaxToolInputBytes:     60,
		ToolInputExceedPolicy: spec.LimitExceedTruncate,
	}

	runID := "run-in-trunc"
	started := time.Date(2026, 6, 7, 15, 0, 0, 0, time.UTC)
	input := startDemoRun(t, st, runID, started)

	ex := &Executor{
		Graph: graph, ProjectRoot: testProjectRoot(t),
		Tools: tools.NewRegistry(graph), Store: st, Trace: trace.NewRecorder(st),
	}
	if err := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListTraceEventsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if countLimitHitEvents(events, "tool_input") == 0 {
		t.Fatal("expected limit_hit for tool_input truncation")
	}
}

func TestRun_toolInputFailPolicy(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "limits-in-fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := echoOnlyWorkflow(t)
	graph.Spec.Limits = &spec.ExecutionLimits{
		MaxToolInputBytes:     60,
		ToolInputExceedPolicy: spec.LimitExceedFail,
	}

	runID := "run-in-fail"
	started := time.Date(2026, 6, 7, 15, 30, 0, 0, time.UTC)
	input := startDemoRun(t, st, runID, started)

	ex := &Executor{
		Graph: graph, ProjectRoot: testProjectRoot(t),
		Tools: tools.NewRegistry(graph), Store: st, Trace: trace.NewRecorder(st),
	}
	err = ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "tool_input exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_checkpointExceedsLimit_emitsLimitHit(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "limits-cp-trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := echoOnlyWorkflow(t)
	graph.Spec.Limits = &spec.ExecutionLimits{MaxCheckpointBytes: 200}

	runID := "run-cp-trace"
	started := time.Date(2026, 6, 7, 16, 0, 0, 0, time.UTC)
	input := startDemoRun(t, st, runID, started)

	ex := &Executor{
		Graph: graph, ProjectRoot: testProjectRoot(t),
		Tools: tools.NewRegistry(graph), Store: st, Trace: trace.NewRecorder(st),
	}
	err = ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true},
	})
	if err == nil {
		t.Fatal("expected checkpoint limit failure")
	}
	if !strings.Contains(err.Error(), "checkpoint context exceeds") {
		t.Fatalf("unexpected error: %v", err)
	}
	events, err := st.ListTraceEventsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if countLimitHitEvents(events, "checkpoint") == 0 {
		t.Fatal("expected limit_hit for checkpoint exceed")
	}
}

func TestRun_workflowLimitOverride(t *testing.T) {
	graph := echoOnlyWorkflow(t)
	graph.Spec.Limits = &spec.ExecutionLimits{MaxToolOutputBytes: 10000}
	graph.Workflows["demo"].Spec.Limits = &spec.ExecutionLimits{
		MaxToolOutputBytes:     80,
		ToolOutputExceedPolicy: spec.LimitExceedFail,
	}
	ex := &Executor{Graph: graph}
	got := ex.resolveToolLimits(graph.Workflows["demo"], "tool.helper.echo")
	if got.MaxToolOutputBytes != 80 {
		t.Fatalf("workflow override = %d", got.MaxToolOutputBytes)
	}
}

func TestRun_toolOutputAtBoundary(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "limits-boundary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph := demoWorkflowGraph(t)
	graph.Workflows["demo"].Spec.Steps = []spec.WorkflowStep{{
		ID: "fetch", Uses: "tool.helper.echo",
		With: map[string]any{"payload": strings.Repeat("a", 20)},
	}}
	graph.Workflows["demo"].Spec.Output = &spec.WorkflowOutput{
		Value: map[string]any{"out": "${steps.fetch.output.echo}"},
	}

	payload := map[string]any{"payload": strings.Repeat("a", 20)}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	limit := len(b) + 40
	graph.Spec.Limits = &spec.ExecutionLimits{
		MaxToolOutputBytes:     limit,
		ToolOutputExceedPolicy: spec.LimitExceedFail,
	}

	runID := "run-boundary"
	started := time.Date(2026, 6, 7, 13, 0, 0, 0, time.UTC)
	if err := st.StartRun(ctx, state.Run{
		RunID: runID, WorkflowName: "demo", Env: "dev", Status: "running",
		StartedAt: started, InputJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}

	ex := &Executor{
		Graph: graph, ProjectRoot: testProjectRoot(t),
		Tools: tools.NewRegistry(graph), Store: st, Trace: trace.NewRecorder(st),
	}
	if err := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started,
		Input: map[string]any{}, Hitl: HitlRunOptions{AutoApprove: true},
	}); err != nil {
		t.Fatalf("boundary should pass: %v", err)
	}
}

func TestResolveToolLimits_toolOverrideWins(t *testing.T) {
	graph := graphWithLimits(t, &spec.ExecutionLimits{MaxToolInputBytes: 1000}, &spec.ExecutionLimits{MaxToolInputBytes: 100})
	ex := &Executor{Graph: graph}
	wf := graph.Workflows["demo"]
	got := ex.resolveToolLimits(wf, "tool.helper.echo")
	if got.MaxToolInputBytes != 100 {
		t.Fatalf("tool override = %d", got.MaxToolInputBytes)
	}
}

// TestToolOutputBudget_clampedToCheckpoint: the budget runToolStep advertises is the resolved
// tool-output limit, clamped to half the checkpoint limit resolved at the ROOT workflow (the one
// saveCheckpoint enforces), so a subworkflow's own maxCheckpointBytes cannot loosen it.
func TestToolOutputBudget_clampedToCheckpoint(t *testing.T) {
	const uses = "tool.helper.echo"
	cases := []struct {
		name           string
		project, tool  *spec.ExecutionLimits
		wfCP, rootWFCP int // 0 = unset; rootWFCP > 0 runs as a subworkflow of a root with that limit
		want           int
	}{
		{name: "defaults: tool limit is below half the checkpoint", want: spec.DefaultMaxToolOutputBytes},
		{name: "raised tool limit clamps to half the default checkpoint", tool: &spec.ExecutionLimits{MaxToolOutputBytes: 2 << 20}, want: spec.DefaultMaxCheckpointBytes / 2},
		{name: "tool limit exactly half the checkpoint", tool: &spec.ExecutionLimits{MaxToolOutputBytes: 512 << 10}, want: 512 << 10},
		{name: "raised checkpoint lets the tool limit through", project: &spec.ExecutionLimits{MaxCheckpointBytes: 8 << 20}, tool: &spec.ExecutionLimits{MaxToolOutputBytes: 2 << 20}, want: 2 << 20},
		{name: "workflow-lowered checkpoint clamps the default tool limit", wfCP: 256 << 10, want: 128 << 10},
		{name: "subworkflow uses the root checkpoint limit", tool: &spec.ExecutionLimits{MaxToolOutputBytes: 2 << 20}, wfCP: 8 << 20, rootWFCP: 200 << 10, want: 100 << 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := graphWithLimits(t, tc.project, tc.tool)
			wf := graph.Workflows["demo"]
			if tc.wfCP > 0 {
				wf.Spec.Limits = &spec.ExecutionLimits{MaxCheckpointBytes: tc.wfCP}
			}
			ex := &Executor{Graph: graph}
			if tc.rootWFCP > 0 {
				ex.rootWF = &spec.WorkflowResource{Spec: spec.WorkflowSpec{Limits: &spec.ExecutionLimits{MaxCheckpointBytes: tc.rootWFCP}}}
			}
			if got := ex.toolOutputBudget(wf, uses); got != tc.want {
				t.Fatalf("toolOutputBudget = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEnforceMapLimit_concurrentSafety(t *testing.T) {
	t.Parallel()
	ex := &Executor{Trace: nil}
	ctx := context.Background()
	v := map[string]any{"x": strings.Repeat("z", 4000)}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := ex.enforceMapLimit(ctx, "r", "s", "tool.helper.echo", spec.LimitKindToolInput, v, 200, spec.LimitExceedTruncate)
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
