package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/audit"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/tools/toolerr"
	"github.com/Terfyn/terfyn/internal/trace"
)

func agentLoopGraph(t *testing.T, agent spec.AgentSpec, pol spec.PolicySpec) *spec.ProjectGraph {
	t.Helper()
	if agent.Model == "" {
		agent.Model = "mock/gpt-4"
	}
	if agent.Output == nil {
		agent.Output = &spec.AgentIO{Schema: "./schemas/agent-out.schema.json"}
	}
	return &spec.ProjectGraph{
		Spec: spec.ProjectSpec{
			Providers: &spec.ProjectProviders{
				Models: map[string]spec.ModelProviderConfig{"mock": {Type: "mock"}},
			},
		},
		Tools: map[string]*spec.ToolResource{
			"helper": {
				APIVersion: spec.APIVersionV0,
				Kind:       spec.KindTool,
				Metadata:   spec.Metadata{Name: "helper"},
				Spec: spec.ToolSpec{
					Type:   "mock",
					Safety: &spec.ToolSafety{SideEffects: spec.BoolPtr(false)},
				},
			},
		},
		Agents: map[string]*spec.AgentResource{
			"reviewer": {
				APIVersion: spec.APIVersionV0,
				Kind:       spec.KindAgent,
				Metadata:   spec.Metadata{Name: "reviewer"},
				Spec:       agent,
			},
		},
		Policies: map[string]*spec.PolicyResource{
			"default": {
				APIVersion: spec.APIVersionV0,
				Kind:       spec.KindPolicy,
				Metadata:   spec.Metadata{Name: "default"},
				Spec:       pol,
			},
		},
		Workflows: map[string]*spec.WorkflowResource{
			"demo": {
				APIVersion: spec.APIVersionV0,
				Kind:       spec.KindWorkflow,
				Metadata:   spec.Metadata{Name: "demo"},
				Spec: spec.WorkflowSpec{
					Policy: "default",
					Steps: []spec.WorkflowStep{
						{ID: "act", Agent: "reviewer", With: map[string]any{"topic": "agents"}},
					},
					Output: &spec.WorkflowOutput{
						Value: map[string]any{"summary": "${steps.act.output.summary}"},
					},
				},
			},
		},
	}
}

func runAgentLoop(t *testing.T, graph *spec.ProjectGraph, mock *models.MockClient, extra tools.ToolExecutor) (*state.Run, []trace.Event, error) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "agent-loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runID := "run-loop"
	started := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := st.StartRun(ctx, state.Run{
		RunID: runID, WorkflowName: "demo", Env: "dev", Status: "running",
		StartedAt: started, InputJSON: `{"topic":"agents"}`,
	}); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(graph)
	if extra != nil {
		reg.Mock = extra
	}
	ex := &Executor{
		Graph:        graph,
		ProjectRoot:  testProjectRoot(t),
		Tools:        reg,
		ModelResolve: func(string) (models.ModelClient, string, error) { return mock, "gpt-4", nil },
		Store:        st,
		Trace:        trace.NewRecorder(st),
	}
	runErr := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: "demo", Env: "dev", StartedAt: started,
		Input: map[string]any{"topic": "agents"},
	})
	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := trace.NewReader(st).ListByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return got, events, runErr
}

func TestRun_agentToolLoop_happyPath(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{
		Instructions: "Use helper then summarize.",
		Tools:        []string{"helper"},
	}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{
				ToolCalls: []models.ToolCall{{
					ID:        "call_1",
					Name:      "helper",
					Arguments: json.RawMessage(`{"q":"weather"}`),
				}},
				Meta: &models.GenerateMeta{DurationMs: 2, PromptTokens: 10, CompletionTokens: 4, CostUSD: 0.02},
			},
			{
				Content: `{"summary":"used helper"}`,
				Meta:    &models.GenerateMeta{DurationMs: 3, PromptTokens: 12, CompletionTokens: 6, CostUSD: 0.03},
			},
		},
	}
	got, events, err := runAgentLoop(t, graph, mock, nil)
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
	if out["summary"] != "used helper" {
		t.Fatalf("output %+v", out)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("generates %d", mock.CallCount())
	}
	first := mock.Requests()[0]
	if first.ToolChoice != models.ToolChoiceAuto || len(first.Tools) != 1 || first.Tools[0].Name != "helper" {
		t.Fatalf("first request %+v", first)
	}
	second := mock.Requests()[1]
	if len(second.Messages) < 4 {
		t.Fatalf("messages %+v", second.Messages)
	}
	var sawResult bool
	for _, msg := range second.Messages {
		for _, r := range msg.ToolResults {
			if r.ToolCallID == "call_1" && strings.Contains(r.Content, "tool.helper.default") {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Fatalf("missing tool_result in %+v", second.Messages)
	}
	if got.TotalCostUSD < 0.05 {
		t.Fatalf("cost %v, want model+tool loop", got.TotalCostUSD)
	}
	var sawLLM int
	for _, ev := range events {
		if ev.Type == string(trace.EventLLMCompletion) {
			sawLLM++
		}
	}
	if sawLLM < 2 {
		t.Fatalf("trace llm=%d events=%d", sawLLM, len(events))
	}
	sel, exec := requireToolTracePair(t, events, "helper", "tool.helper.default")
	assertNoRawToolArgs(t, sel, `"q"`, "weather")
	wantDigest := argumentsDigest(map[string]any{"q": "weather"})
	if got := eventData(t, sel)["argumentsDigest"]; got != wantDigest {
		t.Fatalf("argumentsDigest = %v want %s", got, wantDigest)
	}
	assertToolExecutionPayload(t, exec, true, "")
	assertAuditChain(t, "run-loop", events)
}

// The governing policy's execution.maxIterations is the ceiling, not the frozen global cap: here the
// agent asks for 5 turns but the policy caps at 2, so the loop hits the cap at turn 2 (limit_hit) and
// finalizes — proving the policy ceiling flowed through the evaluator to the loop bound (issue #522).
func TestRun_agentToolLoop_policyMaxIterationsCeilingClamps(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{
		Tools:       []string{"helper"},
		Constraints: &spec.AgentConstraints{MaxIterations: 5},
	}, spec.PolicySpec{Execution: &spec.PolicyExecution{MaxIterations: 2}})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
			{ToolCalls: []models.ToolCall{{ID: "c2", Name: "helper", Arguments: json.RawMessage(`{}`)}}}, // turn 2 is the policy cap
			{Content: `{"summary":"capped by policy"}`},                                                  // forced finalize turn
		},
	}
	got, events, err := runAgentLoop(t, graph, mock, nil)
	if err != nil {
		t.Fatalf("expected finalize at the policy cap, got err %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	// 2 loop turns (capped by the policy's 2, not the agent's 5) + 1 finalize.
	if mock.CallCount() != 3 {
		t.Fatalf("generates %d, want 3 (policy cap 2 + finalize); the agent's maxIterations 5 must not win", mock.CallCount())
	}
	var sawLimit bool
	for _, ev := range events {
		if ev.Type == string(trace.EventLimitHit) && strings.Contains(ev.DataJSON, "max_iterations") {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Fatalf("expected limit_hit max_iterations at the policy cap (2); the ceiling did not clamp, events=%+v", events)
	}
}

// At maxIterations the run no longer dies: the engine forces one final tool-free completion so the
// agent returns its best output, records the limit_hit, and the run succeeds (issue #518).
func TestRun_agentToolLoop_maxIterationsFinalizes(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{
		Tools:       []string{"helper"},
		Constraints: &spec.AgentConstraints{MaxIterations: 2},
	}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
			{ToolCalls: []models.ToolCall{{ID: "c2", Name: "helper", Arguments: json.RawMessage(`{}`)}}}, // 2nd turn is the cap, still tool_use
			{Content: `{"summary":"finalized at cap"}`},                                                  // forced tool-free finalize turn
		},
	}
	got, events, err := runAgentLoop(t, graph, mock, nil)
	if err != nil {
		t.Fatalf("expected graceful finalize, got err %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.OutputJSON), &out); err != nil {
		t.Fatal(err)
	}
	if out["summary"] != "finalized at cap" {
		t.Fatalf("output %+v, want the finalize turn's result", out)
	}
	if mock.CallCount() != 3 {
		t.Fatalf("generates %d, want 3 (2 loop turns + 1 finalize)", mock.CallCount())
	}
	// The forced final turn is tool-free: no tools advertised, so the agent must answer.
	if last := mock.Requests()[2]; len(last.Tools) != 0 {
		t.Fatalf("finalize turn must be tool-free, got %d tools", len(last.Tools))
	}
	var sawLimit bool
	for _, ev := range events {
		if ev.Type == string(trace.EventLimitHit) && strings.Contains(ev.DataJSON, "max_iterations") {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Fatalf("expected limit_hit max_iterations still recorded, events=%+v", events)
	}
}

func TestRun_agentToolLoop_policyDenied(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{
		Approvals: &spec.PolicyApprovals{RequiredFor: []string{"tool.helper.default"}},
	})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
			{Content: `{"summary":"nope"}`},
		},
	}
	got, events, err := runAgentLoop(t, graph, mock, nil)
	if err == nil {
		t.Fatal("expected policy denial")
	}
	d, ok := policy.AsDenied(err)
	if !ok || d.Reason != policy.ReasonApprovalRequired {
		t.Fatalf("denied %+v err=%v", d, err)
	}
	if got.Status != "failed" {
		t.Fatalf("status %q", got.Status)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("generates %d, want 1 (denied before second turn)", mock.CallCount())
	}
	var sawDeny bool
	for _, ev := range events {
		if ev.Type == string(trace.EventSystemError) && strings.Contains(ev.DataJSON, policy.ReasonApprovalRequired) {
			sawDeny = true
		}
	}
	if !sawDeny {
		t.Fatalf("expected system_error approval_required, events=%+v", events)
	}
	assertNoToolTraceEvents(t, events)
	assertAuditChain(t, "run-loop", events)
}

func TestRun_agentToolLoop_noToolsRegression(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "Just answer."}, spec.PolicySpec{})
	mock := &models.MockClient{Content: `{"summary":"plain"}`}
	got, _, err := runAgentLoop(t, graph, mock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("generates %d", mock.CallCount())
	}
	req := mock.LastRequest()
	if len(req.Tools) != 0 || req.ToolChoice != "" {
		t.Fatalf("no-tools request should omit tools: %+v", req)
	}
	if req.Model == "" || len(req.Messages) != 2 {
		t.Fatalf("request %+v", req)
	}
}

func TestRun_agentToolLoop_undeclaredTool(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "ghost", Arguments: json.RawMessage(`{}`)}}},
		},
	}
	_, _, err := runAgentLoop(t, graph, mock, nil)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_agentToolLoop_rejectsUnadvertisedOperation(t *testing.T) {
	names := []string{"helper.echo", "tool.helper.echo", "helper.command.run", "helper.delete.users"}
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{})
			mock := &models.MockClient{
				Script: []models.MockTurn{
					{ToolCalls: []models.ToolCall{{ID: "c1", Name: name, Arguments: json.RawMessage(`{}`)}}},
				},
			}
			calls := 0
			extra := &tools.MockExecutor{Fn: func(ctx context.Context, req tools.ToolCallRequest) (tools.ToolCallResponse, error) {
				calls++
				return tools.ToolCallResponse{Output: map[string]any{"used": req.Uses}}, nil
			}}
			_, _, err := runAgentLoop(t, graph, mock, extra)
			if err == nil || !strings.Contains(err.Error(), "not declared") {
				t.Fatalf("err = %v", err)
			}
			if calls != 0 {
				t.Fatalf("Tools.Call ran %d times; unadvertised ops must fail before Call", calls)
			}
		})
	}
}

// If the forced final completion still cannot produce a valid output, the run fails — finalization is
// a best-effort wrap-up, not a way to accept an unusable result (issue #518).
func TestRun_agentToolLoop_maxIterationsFinalizeInvalidOutputFails(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{
		Tools:       []string{"helper"},
		Constraints: &spec.AgentConstraints{MaxIterations: 1},
	}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
			{Content: "sorry, I could not finish"}, // finalize turn returns prose, not the output object
		},
	}
	got, _, err := runAgentLoop(t, graph, mock, nil)
	if err == nil {
		t.Fatalf("expected failure when the finalize turn returns no valid output, status %q", got.Status)
	}
	if got.Status != "failed" {
		t.Fatalf("status %q", got.Status)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("generates %d, want 2 (cap turn + finalize attempt)", mock.CallCount())
	}
}

// The pending tool_use on the capped turn is never executed — finalization forces a tool-free answer
// instead. With maxIterations 1 the first turn IS the cap, so no tool ever runs (issue #518).
func TestRun_agentToolLoop_maxIterationsDoesNotExecuteLastToolUse(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{
		Tools:       []string{"helper"},
		Constraints: &spec.AgentConstraints{MaxIterations: 1},
	}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}}, // the cap turn (tool_use, never executed)
			{Content: `{"summary":"finalized"}`}, // forced tool-free finalize turn
		},
	}
	calls := 0
	extra := &tools.MockExecutor{Fn: func(ctx context.Context, req tools.ToolCallRequest) (tools.ToolCallResponse, error) {
		calls++
		return tools.ToolCallResponse{Output: map[string]any{"used": req.Uses}}, nil
	}}
	got, events, err := runAgentLoop(t, graph, mock, extra)
	if err != nil {
		t.Fatalf("expected graceful finalize, got err %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("generates %d, want 2 (cap turn + finalize)", mock.CallCount())
	}
	if calls != 0 {
		t.Fatalf("Tools.Call ran %d times; the capped turn's tool_use must not execute", calls)
	}
	var sawLimit bool
	for _, ev := range events {
		if ev.Type == string(trace.EventLimitHit) && strings.Contains(ev.DataJSON, "max_iterations") {
			sawLimit = true
		}
	}
	if !sawLimit {
		t.Fatalf("expected limit_hit max_iterations, events=%+v", events)
	}
	assertNoToolTraceEvents(t, events)
}

func TestRun_agentToolLoop_pinnedUses(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"tool.helper.echo"}}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
			{Content: `{"summary":"pinned"}`},
		},
	}
	got, _, err := runAgentLoop(t, graph, mock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	second := mock.Requests()[1]
	var sawResult bool
	for _, msg := range second.Messages {
		for _, r := range msg.ToolResults {
			if r.ToolCallID == "c1" && strings.Contains(r.Content, "tool.helper.echo") {
				sawResult = true
			}
		}
	}
	if !sawResult {
		t.Fatalf("expected pinned uses in tool_result, messages=%+v", second.Messages)
	}
}

func TestRun_agentToolLoop_httpRequiresPinnedUses(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"api"}}, spec.PolicySpec{})
	graph.Tools["api"] = &spec.ToolResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindTool,
		Metadata:   spec.Metadata{Name: "api"},
		Spec:       spec.ToolSpec{Type: "http"},
	}
	mock := &models.MockClient{Content: `{"summary":"nope"}`}
	_, _, err := runAgentLoop(t, graph, mock, nil)
	if err == nil || !strings.Contains(err.Error(), "no default operation") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_agentToolLoop_httpPinnedDefaultRejected(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"tool.api.default"}}, spec.PolicySpec{})
	graph.Tools["api"] = &spec.ToolResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindTool,
		Metadata:   spec.Metadata{Name: "api"},
		Spec:       spec.ToolSpec{Type: "http"},
	}
	mock := &models.MockClient{Content: `{"summary":"nope"}`}
	_, _, err := runAgentLoop(t, graph, mock, nil)
	if err == nil || !strings.Contains(err.Error(), "no default operation") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_agentToolLoop_toolCallHonorsTimeout(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{
		Tools:       []string{"helper"},
		Constraints: &spec.AgentConstraints{TimeoutSeconds: 9},
	}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
			{Content: `{"summary":"ok"}`},
		},
	}
	var sawDeadline bool
	extra := &tools.MockExecutor{Fn: func(ctx context.Context, req tools.ToolCallRequest) (tools.ToolCallResponse, error) {
		_, sawDeadline = ctx.Deadline()
		return tools.ToolCallResponse{Output: map[string]any{"used": req.Uses}}, nil
	}}
	got, _, err := runAgentLoop(t, graph, mock, extra)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	if !sawDeadline {
		t.Fatal("inner tool call context must inherit constraints.timeoutSeconds")
	}
}

func TestRun_agentToolLoop_budgetCheckedEachTurn(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{
		Execution: &spec.PolicyExecution{MaxTotalCostUsd: 0.04},
	})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{
				ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}},
				Meta:      &models.GenerateMeta{CostUSD: 0.02},
			},
			{
				Content: `{"summary":"over"}`,
				Meta:    &models.GenerateMeta{CostUSD: 0.03},
			},
		},
	}
	got, events, err := runAgentLoop(t, graph, mock, nil)
	if err == nil {
		t.Fatal("expected cost ceiling denial")
	}
	d, ok := policy.AsDenied(err)
	if !ok || d.Reason != policy.ReasonMaxCost {
		t.Fatalf("denied %+v err=%v", d, err)
	}
	if got.Status != "failed" {
		t.Fatalf("status %q", got.Status)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("generates %d, want 2 (deny after second turn)", mock.CallCount())
	}
	var sawDeny, sawLimit, sawRunErr bool
	for _, ev := range events {
		switch ev.Type {
		case string(trace.EventSystemError):
			if strings.Contains(ev.DataJSON, policy.ReasonMaxCost) {
				sawDeny = true
			}
		case string(trace.EventLimitHit):
			if strings.Contains(ev.DataJSON, `"kind":"max_cost"`) {
				sawLimit = true
				assertCostLimitHitPayload(t, ev, 0.04, 0.05)
			}
		case string(trace.EventRunError):
			sawRunErr = true
			data := eventData(t, ev)
			if data["reason"] != policy.ReasonMaxCost {
				t.Fatalf("run_error reason=%v want %s (%s)", data["reason"], policy.ReasonMaxCost, ev.DataJSON)
			}
		}
	}
	if !sawDeny {
		t.Fatalf("expected system_error max_cost, events=%+v", events)
	}
	if !sawLimit {
		t.Fatalf("expected limit_hit max_cost, events=%+v", events)
	}
	if !sawRunErr {
		t.Fatalf("expected run_error, events=%+v", events)
	}
}

func TestRun_agentNoTools_budgetAfterGenerate(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "Just answer."}, spec.PolicySpec{
		Execution: &spec.PolicyExecution{MaxTotalCostUsd: 0.04},
	})
	mock := &models.MockClient{
		Content: `{"summary":"plain"}`,
		Meta:    &models.GenerateMeta{CostUSD: 0.05},
	}
	got, events, err := runAgentLoop(t, graph, mock, nil)
	if err == nil {
		t.Fatal("expected cost ceiling denial")
	}
	d, ok := policy.AsDenied(err)
	if !ok || d.Reason != policy.ReasonMaxCost {
		t.Fatalf("denied %+v err=%v", d, err)
	}
	if got.Status != "failed" {
		t.Fatalf("status %q", got.Status)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("generates %d, want 1 (deny after the only turn)", mock.CallCount())
	}
	assertMaxCostTrace(t, events, 0.04, 0.05)
}

func TestRun_multiStep_priorStepCostBlocksNext(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "Just answer."}, spec.PolicySpec{
		Execution: &spec.PolicyExecution{MaxTotalCostUsd: 0.04},
	})
	graph.Workflows["demo"].Spec.Steps = []spec.WorkflowStep{
		{ID: "prep", Uses: "tool.helper.default", With: map[string]any{"x": 1}},
		{ID: "act", Agent: "reviewer", With: map[string]any{"topic": "agents"}},
	}
	graph.Workflows["demo"].Spec.Output = &spec.WorkflowOutput{
		Value: map[string]any{"ok": true},
	}
	mock := &models.MockClient{Content: `{"summary":"should not run"}`}
	extra := &tools.MockExecutor{Resp: tools.ToolCallResponse{
		Output: map[string]any{"ok": true},
		Meta:   tools.ToolCallMeta{CostUSD: 0.05, DurationMs: 1},
	}}
	got, events, err := runAgentLoop(t, graph, mock, extra)
	if err == nil {
		t.Fatal("expected cost ceiling denial")
	}
	d, ok := policy.AsDenied(err)
	if !ok || d.Reason != policy.ReasonMaxCost {
		t.Fatalf("denied %+v err=%v", d, err)
	}
	if got.Status != "failed" {
		t.Fatalf("status %q", got.Status)
	}
	if mock.CallCount() != 0 {
		t.Fatalf("generates %d, want 0 (blocked before step 2)", mock.CallCount())
	}
	var sawLimit, sawRunErr bool
	for _, ev := range events {
		switch ev.Type {
		case string(trace.EventLimitHit):
			if strings.Contains(ev.DataJSON, `"kind":"max_cost"`) {
				sawLimit = true
				assertCostLimitHitPayload(t, ev, 0.04, 0.05)
			}
		case string(trace.EventRunError):
			sawRunErr = true
			if eventData(t, ev)["reason"] != policy.ReasonMaxCost {
				t.Fatalf("run_error %s", ev.DataJSON)
			}
		}
	}
	if !sawLimit || !sawRunErr {
		t.Fatalf("expected limit_hit+run_error, events=%+v", events)
	}
}

// The secret-bearing string an adapter might put in a tool error's underlying cause (an HTTP 401
// body, a URL with an api_key). The security contract is that NONE of these reach the model or the
// audit trace, whether the error is recoverable or fatal.
var toolErrorSecrets = []string{
	"sk-live-SECRET99", "tok_abc", "hunter2",
	"api_key=sk-live-SECRET99", "Bearer tok_abc", "password=hunter2",
	"https://api.example.com/v1?api_key=",
}

func secretBearingToolError() error {
	return errors.New("http 401 GET https://api.example.com/v1?api_key=sk-live-SECRET99 Authorization: Bearer tok_abc password=hunter2")
}

// TestRun_agentToolLoop_toolCallErrorEmitsExecution proves a RECOVERABLE tool error (issue #451):
// the run continues past the miss, the model receives a constructed {"error": …} observation, and
// the secret-bearing underlying cause reaches NEITHER the model (second Generate) NOR the trace.
func TestRun_agentToolLoop_toolCallErrorEmitsExecution(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{ToolCalls: []models.ToolCall{{
				ID:        "c1",
				Name:      "helper",
				Arguments: json.RawMessage(`{"password":"s3cret","q":"weather"}`),
			}}},
			{Content: `{"summary":"recovered"}`},
		},
	}
	// Recoverable, but its underlying cause carries secrets: the adapter's model-safe observation is
	// what the agent may see, never the raw Error().
	const observation = `helper: "framework/ablation_bench_test.go" does not exist in the workspace`
	extra := &tools.MockExecutor{Err: toolerr.Recoverable(observation, secretBearingToolError())}
	got, events, err := runAgentLoop(t, graph, mock, extra)
	// A recoverable tool error does not die on the miss: the loop hands the observation back and
	// proceeds to the next turn, and the run succeeds.
	if err != nil {
		t.Fatalf("a recoverable tool error must not be fatal: %v", err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q, want succeeded", got.Status)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("generates %d, want 2 (the loop recovered and continued)", mock.CallCount())
	}

	// (1) Recovery is REAL: the second Generate carries a tool result for c1 that is the constructed
	// {"error": …} observation — not an empty payload, not the raw error.
	second := mock.Requests()[1]
	obs := findToolResultContent(second, "c1")
	if obs == "" {
		t.Fatalf("second request has no tool result for c1: %+v", second.Messages)
	}
	if !strings.Contains(obs, `"error"`) || !strings.Contains(obs, "does not exist") {
		t.Fatalf("recovery observation is not the constructed {\"error\":…} object: %q", obs)
	}
	// The recoverable failure is answered by a well-formed is_error tool_result, so the model treats
	// it as an error observation and a failed call can never leave a dangling tool_use (issue #524).
	if r := findToolResult(second, "c1"); r == nil || !r.IsError {
		t.Fatalf("recovered tool result for c1 must be is_error: %+v", r)
	}

	// (2) The security contract: none of the secrets from the underlying error appear anywhere in the
	// second Generate the provider sees (the channel the old test never inspected).
	assertNoSecretsInRequest(t, second)

	// (3) The audit trace still records the failure by the stable redacted token, never the raw error.
	sel, exec := requireToolTracePair(t, events, "helper", "tool.helper.default")
	assertNoRawToolArgs(t, sel, "s3cret", "weather", `"password"`)
	wantDigest := argumentsDigest(map[string]any{"password": "s3cret", "q": "weather"})
	if got := eventData(t, sel)["argumentsDigest"]; got != wantDigest {
		t.Fatalf("argumentsDigest = %v want %s", got, wantDigest)
	}
	assertToolExecutionPayload(t, exec, false, toolCallFailedReason)
	for _, secret := range toolErrorSecrets {
		if strings.Contains(exec.DataJSON, secret) {
			t.Fatalf("secret %q leaked into tool_execution: %s", secret, exec.DataJSON)
		}
	}
	assertAuditChain(t, "run-loop", events)
}

// TestRun_agentToolLoop_unmarkedToolErrorAborts proves the fail-closed default (issue #451): a tool
// error the adapter did NOT mark recoverable (toolerr.ErrRecoverable) aborts the run on the first
// turn instead of being handed back as an observation. Because the run never reaches a second turn,
// the secret-bearing diagnostic never reaches the model — the reason unmarked-is-fatal is the safe
// default. (Replaces the old native.ErrFatalTool-sentinel test.)
func TestRun_agentToolLoop_unmarkedToolErrorAborts(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{})
	mock := &models.MockClient{Script: []models.MockTurn{
		{ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{}`)}}},
		{Content: `{"summary":"unreachable"}`},
	}}
	extra := &tools.MockExecutor{Err: secretBearingToolError()}
	got, _, err := runAgentLoop(t, graph, mock, extra)
	if err == nil {
		t.Fatal("an unmarked tool error must abort the run, not be recovered")
	}
	if got.Status != "failed" {
		t.Fatalf("status %q, want failed", got.Status)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("generates %d, want 1 (aborted on the fatal error, did not continue)", mock.CallCount())
	}
}

// findToolResultContent returns the tool-result content for callID in req, or "" if absent.
func findToolResultContent(req models.GenerateRequest, callID string) string {
	if r := findToolResult(req, callID); r != nil {
		return r.Content
	}
	return ""
}

func findToolResult(req models.GenerateRequest, callID string) *models.ToolResult {
	for _, msg := range req.Messages {
		for i := range msg.ToolResults {
			if msg.ToolResults[i].ToolCallID == callID {
				return &msg.ToolResults[i]
			}
		}
	}
	return nil
}

// assertNoSecretsInRequest fails if any known tool-error secret appears anywhere in req — a message
// content or a tool-result payload the provider would see.
func assertNoSecretsInRequest(t *testing.T, req models.GenerateRequest) {
	t.Helper()
	for _, msg := range req.Messages {
		for _, secret := range toolErrorSecrets {
			if strings.Contains(msg.Content, secret) {
				t.Fatalf("secret %q leaked to the model in message content: %s", secret, msg.Content)
			}
			for _, r := range msg.ToolResults {
				if strings.Contains(r.Content, secret) {
					t.Fatalf("secret %q leaked to the model in a tool result: %s", secret, r.Content)
				}
			}
		}
	}
}

func eventData(t *testing.T, ev trace.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(ev.DataJSON), &m); err != nil {
		t.Fatalf("data json: %v (%s)", err, ev.DataJSON)
	}
	return m
}

func requireToolTracePair(t *testing.T, events []trace.Event, tool, uses string) (selection, execution trace.Event) {
	t.Helper()
	var types []string
	var sel, exec *trace.Event
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case string(trace.EventToolSelection), string(trace.EventToolExecution):
			types = append(types, ev.Type)
			if ev.ActorType != string(trace.ActorAgent) {
				t.Fatalf("%s actor = %q want %s", ev.Type, ev.ActorType, trace.ActorAgent)
			}
			data := eventData(t, ev)
			if data["tool"] != tool || data["uses"] != uses {
				t.Fatalf("%s payload tool=%v uses=%v want %s / %s (%s)", ev.Type, data["tool"], data["uses"], tool, uses, ev.DataJSON)
			}
			switch ev.Type {
			case string(trace.EventToolSelection):
				if sel != nil {
					t.Fatalf("duplicate tool_selection: %v", types)
				}
				cp := ev
				sel = &cp
			case string(trace.EventToolExecution):
				if exec != nil {
					t.Fatalf("duplicate tool_execution: %v", types)
				}
				cp := ev
				exec = &cp
			}
		}
	}
	if sel == nil || exec == nil {
		t.Fatalf("missing tool_selection/tool_execution pair, types=%v events=%+v", types, events)
	}
	if types[0] != string(trace.EventToolSelection) || types[1] != string(trace.EventToolExecution) {
		t.Fatalf("tool events order %v, want tool_selection then tool_execution", types)
	}
	return *sel, *exec
}

func assertToolExecutionPayload(t *testing.T, ev trace.Event, wantSuccess bool, errSubstr string) {
	t.Helper()
	data := eventData(t, ev)
	if _, ok := data["durationMs"]; !ok {
		t.Fatalf("missing durationMs: %s", ev.DataJSON)
	}
	if _, ok := data["costUsd"]; !ok {
		t.Fatalf("missing costUsd: %s", ev.DataJSON)
	}
	got, ok := data["success"].(bool)
	if !ok || got != wantSuccess {
		t.Fatalf("success = %v (%T), want %v (%s)", data["success"], data["success"], wantSuccess, ev.DataJSON)
	}
	if errSubstr == "" {
		if _, exists := data["error"]; exists {
			t.Fatalf("unexpected error field: %s", ev.DataJSON)
		}
		return
	}
	msg, _ := data["error"].(string)
	if !strings.Contains(msg, errSubstr) {
		t.Fatalf("error = %q, want substring %q (%s)", msg, errSubstr, ev.DataJSON)
	}
}

func assertNoRawToolArgs(t *testing.T, ev trace.Event, forbidden ...string) {
	t.Helper()
	for _, s := range forbidden {
		if strings.Contains(ev.DataJSON, s) {
			t.Fatalf("raw tool argument %q leaked into %s: %s", s, ev.Type, ev.DataJSON)
		}
	}
}

func assertNoToolTraceEvents(t *testing.T, events []trace.Event) {
	t.Helper()
	for _, ev := range events {
		if ev.Type == string(trace.EventToolSelection) || ev.Type == string(trace.EventToolExecution) {
			t.Fatalf("unexpected %s on fail-closed path: %s", ev.Type, ev.DataJSON)
		}
	}
}

func assertAuditChain(t *testing.T, runID string, events []trace.Event) {
	t.Helper()
	if err := audit.VerifyRunChainError(runID, events); err != nil {
		t.Fatal(err)
	}
}

func assertMaxCostTrace(t *testing.T, events []trace.Event, ceiling, accumulated float64) {
	t.Helper()
	var sawLimit, sawRunErr, sawDeny bool
	for _, ev := range events {
		switch ev.Type {
		case string(trace.EventSystemError):
			if strings.Contains(ev.DataJSON, policy.ReasonMaxCost) {
				sawDeny = true
			}
		case string(trace.EventLimitHit):
			if strings.Contains(ev.DataJSON, `"kind":"max_cost"`) {
				sawLimit = true
				assertCostLimitHitPayload(t, ev, ceiling, accumulated)
			}
		case string(trace.EventRunError):
			sawRunErr = true
			if eventData(t, ev)["reason"] != policy.ReasonMaxCost {
				t.Fatalf("run_error %s", ev.DataJSON)
			}
		}
	}
	if !sawDeny || !sawLimit || !sawRunErr {
		t.Fatalf("max_cost traces deny=%v limit=%v run_error=%v events=%+v", sawDeny, sawLimit, sawRunErr, events)
	}
}

func assertCostLimitHitPayload(t *testing.T, ev trace.Event, ceiling, accumulated float64) {
	t.Helper()
	if ev.ActorType != string(trace.ActorSystem) {
		t.Fatalf("limit_hit actor=%q want %s", ev.ActorType, trace.ActorSystem)
	}
	data := eventData(t, ev)
	if data["kind"] != "max_cost" {
		t.Fatalf("kind=%v (%s)", data["kind"], ev.DataJSON)
	}
	if _, ok := data["maxBytes"]; ok {
		t.Fatalf("cost limit_hit must not use byte LimitHitTraceData: %s", ev.DataJSON)
	}
	gotCeil, _ := data["maxTotalCostUsd"].(float64)
	gotAcc, _ := data["accumulatedUsd"].(float64)
	if math.Abs(gotCeil-ceiling) > 1e-9 || math.Abs(gotAcc-accumulated) > 1e-9 {
		t.Fatalf("ceiling=%v accumulated=%v want %v / %v (%s)", gotCeil, gotAcc, ceiling, accumulated, ev.DataJSON)
	}
}

func TestArgumentsDigest_canonicalKeyOrder(t *testing.T) {
	a := argumentsDigest(map[string]any{"z": 1, "a": 2})
	b := argumentsDigest(map[string]any{"a": 2, "z": 1})
	if a == "" || a != b {
		t.Fatalf("digests %q vs %q", a, b)
	}
}

func TestToolExecutionData_omitsRawError(t *testing.T) {
	data := toolExecutionData("tool.helper.default", tools.ToolCallMeta{DurationMs: 3, CostUSD: 0.01}, errors.New("api_key=sk-live-SECRET99"))
	if data["success"] != false {
		t.Fatalf("success = %v", data["success"])
	}
	if data["error"] != toolCallFailedReason {
		t.Fatalf("error = %v want %s", data["error"], toolCallFailedReason)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-live-SECRET99") || strings.Contains(string(raw), "api_key=") {
		t.Fatalf("raw error leaked: %s", raw)
	}
}
