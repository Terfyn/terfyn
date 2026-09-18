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
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", step, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "s"}, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
	if got["task"] != "placeholder" {
		t.Fatalf("without readOnly, placeholder must stand, got %v", got["task"])
	}
}

func TestRestoreReadOnlyAgentOutput_namedArg0IsData(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "Implementer"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./CodingState.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./CodingState.json": codingStateSchema}}
	// YAML with: {arg0: {task: ...}} is a user-visible field, not positional provenance.
	prior := map[string]any{
		"arg0": map[string]any{
			"task":     "implement issue 533",
			"approved": false,
			"feedback": []any{},
			"summary":  "",
		},
	}
	out := map[string]any{
		"task":     "placeholder",
		"approved": false,
		"feedback": []any{},
		"summary":  "placeholder",
	}
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "implement"}, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
	if got["task"] != "placeholder" {
		t.Fatalf("named arg0 must not be treated as the document, task=%v", got["task"])
	}
}

func TestAgentInputDocument_isIdentity(t *testing.T) {
	inner := map[string]any{"task": "fix the parser"}
	got := agentInputDocument(map[string]any{"arg0": inner})
	if _, ok := got["arg0"]; !ok {
		t.Fatalf("named arg0 must stay a field, got %v", got)
	}
	named := map[string]any{"task": "x", "summary": "y"}
	if agentInputDocument(named)["task"] != "x" {
		t.Fatal("named multi-arg map must stay as-is")
	}
}

const refRootSchema = `{
  "$ref": "#/$defs/state",
  "$defs": {
    "state": {
      "type": "object",
      "properties": {
        "task": {"type": "string", "readOnly": true},
        "summary": {"type": "string"}
      }
    }
  }
}`

const allOfRootSchema = `{
  "allOf": [
    {
      "type": "object",
      "properties": {
        "task": {"type": "string", "readOnly": true}
      }
    },
    {
      "type": "object",
      "properties": {
        "summary": {"type": "string"}
      }
    }
  ]
}`

func TestRestoreReadOnlyAgentOutput_rootRefSchema(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "Implementer"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./CodingState.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./CodingState.json": refRootSchema}}
	prior := map[string]any{"task": "implement issue 533", "summary": "old"}
	out := map[string]any{"task": "placeholder", "summary": "new"}
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "implement"}, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
	if got["task"] != "implement issue 533" {
		t.Fatalf("root $ref readOnly task = %v, want prior identity", got["task"])
	}
	if got["summary"] != "new" {
		t.Fatalf("mutable summary should stay as emitted, got %v", got["summary"])
	}
}

func TestRestoreReadOnlyAgentOutput_allOfSchema(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "Implementer"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./CodingState.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./CodingState.json": allOfRootSchema}}
	prior := map[string]any{"task": "implement issue 533", "summary": "old"}
	out := map[string]any{"task": "placeholder", "summary": "new"}
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "implement"}, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
	if got["task"] != "implement issue 533" {
		t.Fatalf("allOf readOnly task = %v, want prior identity", got["task"])
	}
}

const propertyAllOfReadOnlySchema = `{
  "type": "object",
  "properties": {
    "task": {"allOf": [{"type": "string"}, {"readOnly": true}]},
    "summary": {"type": "string"}
  }
}`

func TestRestoreReadOnlyAgentOutput_propertyAllOfReadOnly(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "Implementer"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./CodingState.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./CodingState.json": propertyAllOfReadOnlySchema}}
	prior := map[string]any{"task": "implement issue 533", "summary": "old"}
	out := map[string]any{"task": "placeholder", "summary": "new"}
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "implement"}, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
	if got["task"] != "implement issue 533" {
		t.Fatalf("property allOf readOnly task = %v, want prior identity", got["task"])
	}
}

func TestEnforceReadOnlyOutput_rejectsRestoredTypeMismatch(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "Implementer"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./CodingState.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./CodingState.json": codingStateSchema}}
	prior := map[string]any{
		"task":     7,
		"approved": false,
		"feedback": []any{},
		"summary":  "",
	}
	out := map[string]any{
		"task":     "placeholder",
		"approved": false,
		"feedback": []any{},
		"summary":  "ok",
	}
	_, err := e.enforceReadOnlyOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "implement"}, agent, prior, out)
	if err == nil {
		t.Fatal("restored number into string readOnly field must fail output schema")
	}
}

func TestRestoreReadOnlyAgentOutput_gradualNoSchema(t *testing.T) {
	agent := &spec.AgentResource{Metadata: spec.Metadata{Name: "a"}}
	e := &Executor{}
	out := map[string]any{"task": "placeholder"}
	prior := map[string]any{"task": "real"}
	got, err := e.restoreReadOnlyAgentOutput(context.Background(), "run-1", spec.WorkflowStep{ID: "s"}, agent, prior, out)
	if err != nil {
		t.Fatal(err)
	}
	if got["task"] != "placeholder" {
		t.Fatalf("gradual agent must not restore, got %v", got["task"])
	}
}
