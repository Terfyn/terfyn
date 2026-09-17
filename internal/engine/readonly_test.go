package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

const codingStateSchema = `{
  "type": "object",
  "properties": {
    "task": {"type": "string", "readOnly": true},
    "approved": {"type": "boolean"},
    "feedback": {"type": "array", "items": {"type": "string"}},
    "summary": {"type": "string"}
  },
  "required": ["task", "approved", "feedback", "summary"],
  "additionalProperties": false
}`

const noReadOnlySchema = `{
  "type": "object",
  "properties": {
    "task": {"type": "string"},
    "summary": {"type": "string"}
  }
}`

func TestRestoreReadOnlyFields_overwritesMutation(t *testing.T) {
	out := map[string]any{"task": "placeholder", "summary": "did work"}
	prior := map[string]any{"task": "fix the parser", "summary": "old"}
	got := restoreReadOnlyFields(out, prior, []string{"task"})
	if !reflect.DeepEqual(got, []string{"task"}) {
		t.Fatalf("restored = %v, want [task]", got)
	}
	if out["task"] != "fix the parser" {
		t.Fatalf("task = %v, want prior identity", out["task"])
	}
	if out["summary"] != "did work" {
		t.Fatalf("mutable summary was rewritten: %v", out["summary"])
	}
}

func TestRestoreReadOnlyFields_noopWhenUnchanged(t *testing.T) {
	out := map[string]any{"task": "fix the parser"}
	prior := map[string]any{"task": "fix the parser"}
	got := restoreReadOnlyFields(out, prior, []string{"task"})
	if len(got) != 0 {
		t.Fatalf("restored = %v, want none", got)
	}
}

func TestRestoreReadOnlyFields_skipsMissingPrior(t *testing.T) {
	out := map[string]any{"task": "placeholder"}
	got := restoreReadOnlyFields(out, map[string]any{"summary": "x"}, []string{"task"})
	if len(got) != 0 {
		t.Fatalf("restored = %v, want none", got)
	}
	if out["task"] != "placeholder" {
		t.Fatalf("task = %v, want placeholder (nothing to restore)", out["task"])
	}
}

func TestRestoreReadOnlyAgentOutput_issue533(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "Implementer"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./CodingState.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./CodingState.json": codingStateSchema}}
	step := spec.WorkflowStep{ID: "implement"}
	prior := map[string]any{
		"task":     "implement issue 533",
		"approved": false,
		"feedback": []any{},
		"summary":  "",
	}
	out := map[string]any{
		"task":     "placeholder",
		"approved": false,
		"feedback": []any{},
		"summary":  "placeholder",
	}
	got := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", step, agent, prior, out)
	if got["task"] != "implement issue 533" {
		t.Fatalf("task = %v, want prior identity restored", got["task"])
	}
	if got["summary"] != "placeholder" {
		t.Fatalf("mutable summary should stay as emitted, got %v", got["summary"])
	}
}

func TestRestoreReadOnlyAgentOutput_noReadOnlyIsNoop(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "a"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./out.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./out.json": noReadOnlySchema}}
	out := map[string]any{"task": "placeholder", "summary": "x"}
	prior := map[string]any{"task": "real task"}
	got := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "s"}, agent, prior, out)
	if got["task"] != "placeholder" {
		t.Fatalf("without readOnly, placeholder must stand, got %v", got["task"])
	}
}

func TestRestoreReadOnlyAgentOutput_gradualNoSchema(t *testing.T) {
	agent := &spec.AgentResource{Metadata: spec.Metadata{Name: "a"}}
	e := &Executor{}
	out := map[string]any{"task": "placeholder"}
	prior := map[string]any{"task": "real"}
	got := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "s"}, agent, prior, out)
	if got["task"] != "placeholder" {
		t.Fatalf("gradual agent must not restore, got %v", got["task"])
	}
}
