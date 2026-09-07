package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

// TestRun_traceDetail_surfacesSubstance proves --trace-detail (issue #525) enriches the trace with
// each turn's substance — the agent's reasoning text on llm_completion, the call arguments on
// tool_selection, and the tool output on tool_execution — while still routing every added field
// through the recorder's redaction (a secret-keyed argument is masked, not surfaced).
func TestRun_traceDetail_surfacesSubstance(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{
				Content: "I will edit the file to add the benchmark.",
				ToolCalls: []models.ToolCall{{
					ID:        "c1",
					Name:      "helper",
					Arguments: json.RawMessage(`{"path":"foo.go","old_string":"a","new_string":"b","password":"s3cret"}`),
				}},
			},
			{Content: `{"summary":"done"}`},
		},
	}
	extra := &tools.MockExecutor{Resp: tools.ToolCallResponse{Output: map[string]any{"result": "patched", "lines": float64(3)}}}

	_, events, err := runAgentLoopCfg(t, graph, mock, extra, func(e *Executor) { e.TraceDetail = true })
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	sel, exec := requireToolTracePair(t, events, "helper", "tool.helper.default")

	// tool_selection carries the arguments alongside the digest; the secret key is redacted.
	selData := eventData(t, sel)
	args, ok := selData[trace.FieldToolArgs].(map[string]any)
	if !ok {
		t.Fatalf("tool_selection missing %q: %s", trace.FieldToolArgs, sel.DataJSON)
	}
	if args["path"] != "foo.go" || args["old_string"] != "a" || args["new_string"] != "b" {
		t.Fatalf("args not surfaced: %v", args)
	}
	if _, stillDigest := selData["argumentsDigest"]; !stillDigest {
		t.Fatalf("digest must remain for auditors: %s", sel.DataJSON)
	}
	if args["password"] == "s3cret" || strings.Contains(sel.DataJSON, "s3cret") {
		t.Fatalf("secret argument leaked into detail trace: %s", sel.DataJSON)
	}

	// tool_execution carries the tool output.
	execData := eventData(t, exec)
	out, ok := execData[trace.FieldToolOutput].(map[string]any)
	if !ok || out["result"] != "patched" {
		t.Fatalf("tool_execution missing output: %s", exec.DataJSON)
	}

	// llm_completion carries the agent's reasoning text.
	var sawText bool
	for _, ev := range events {
		if ev.Type != string(trace.EventLLMCompletion) {
			continue
		}
		if txt, _ := eventData(t, ev)[trace.FieldCompletionText].(string); strings.Contains(txt, "edit the file") {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("no llm_completion carried the reasoning text")
	}
}

// TestRun_traceDetail_offKeepsTerseShape proves the default (no --trace-detail) is unchanged: none of
// the substance fields appear, so the audit-event byte shape and privacy posture are preserved.
func TestRun_traceDetail_offKeepsTerseShape(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Tools: []string{"helper"}}, spec.PolicySpec{})
	mock := &models.MockClient{
		Script: []models.MockTurn{
			{
				Content:   "reasoning",
				ToolCalls: []models.ToolCall{{ID: "c1", Name: "helper", Arguments: json.RawMessage(`{"q":"x"}`)}},
			},
			{Content: `{"summary":"done"}`},
		},
	}
	extra := &tools.MockExecutor{Resp: tools.ToolCallResponse{Output: map[string]any{"result": "ok"}}}
	_, events, err := runAgentLoop(t, graph, mock, extra)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, ev := range events {
		for _, k := range []string{trace.FieldToolArgs, trace.FieldToolOutput, trace.FieldCompletionText} {
			if strings.Contains(ev.DataJSON, `"`+k+`"`) {
				t.Fatalf("detail field %q present without --trace-detail: %s", k, ev.DataJSON)
			}
		}
	}
}
