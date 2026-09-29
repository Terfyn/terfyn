package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
)

// The whole-document rule (#552): a single-parameter callee called with one
// argument receives that argument as its ENTIRE input document, whatever JSON
// value it is. These tests pin that the document is carried as `any` through the
// invoker, interpreter start, schema validation, and checkpoint/resume rather
// than being asserted to be an object.

// The call shape comes only from the node's explicit WholeDocument bit: with it,
// the single argument is the document whatever JSON value it is; without it, the
// argument map is the document whatever its keys — a YAML `with: {input: …}` or
// `{arg0: …}` is never unwrapped.
func TestWorkflowInputDocument(t *testing.T) {
	t.Parallel()
	whole := execir.CallSite{Bind: "c", WholeDocument: true}
	plain := execir.CallSite{Bind: "c"}
	tests := []struct {
		name string
		site execir.CallSite
		args map[string]any
		want any
	}{
		{"string", whole, map[string]any{"value": "hello"}, "hello"},
		{"number", whole, map[string]any{"value": 1.5}, 1.5},
		{"bool", whole, map[string]any{"value": false}, false},
		{"array", whole, map[string]any{"value": []any{"a", 2.0}}, []any{"a", 2.0}},
		{"null", whole, map[string]any{"value": nil}, nil},
		{"object", whole, map[string]any{"value": map[string]any{"x": "y"}}, map[string]any{"x": "y"}},
		{"YAML input key stays a map", plain, map[string]any{"input": map[string]any{"x": "y"}}, map[string]any{"input": map[string]any{"x": "y"}}},
		{"YAML arg0 key stays a map", plain, map[string]any{"arg0": "hello"}, map[string]any{"arg0": "hello"}},
		{"several args stay a map", plain, map[string]any{"a": 1, "b": 2}, map[string]any{"a": 1, "b": 2}},
	}
	for _, tc := range tests {
		got, err := workflowInputDocument(tc.site, "child", tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
	for _, bad := range []map[string]any{nil, {"a": 1, "b": 2}} {
		if _, err := workflowInputDocument(whole, "child", bad); err == nil {
			t.Errorf("whole-document site with %d args must be refused", len(bad))
		}
	}
}

// TestExecIR_nestedSingleParamWholeDocument runs real .agent source through the
// engine: the reviewed `Identity(value: String) -> String { return value }` case
// and its array / null / number / bool / object siblings must all return the
// argument itself, not the {"value": ...} call wrapper.
func TestExecIR_nestedSingleParamWholeDocument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		src   string
		input map[string]any
		want  string // JSON
	}{
		{
			name: "string literal",
			src: `
workflow Identity(value: String) -> String {
    return value
}

workflow Main(input: Anything) -> String {
    return Identity("hello")
}
`,
			input: map[string]any{},
			want:  `"hello"`,
		},
		{
			name: "string from input",
			src: `
workflow Identity(value: String) -> String {
    return value
}

workflow Main(input: Anything) -> String {
    return Identity(input.doc)
}
`,
			input: map[string]any{"doc": "hello"},
			want:  `"hello"`,
		},
		{
			name: "array",
			src: `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    return Identity(input.doc)
}
`,
			input: map[string]any{"doc": []any{"a", "b", 3.0}},
			want:  `["a","b",3]`,
		},
		{
			name: "null",
			src: `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    return Identity(input.doc)
}
`,
			input: map[string]any{"doc": nil},
			want:  `null`,
		},
		{
			name: "number",
			src: `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    return Identity(input.doc)
}
`,
			input: map[string]any{"doc": 42.5},
			want:  `42.5`,
		},
		{
			name: "bool",
			src: `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    return Identity(input.doc)
}
`,
			input: map[string]any{"doc": false},
			want:  `false`,
		},
		{
			name: "object",
			src: `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    return Identity(input.doc)
}
`,
			input: map[string]any{"doc": map[string]any{"x": "y"}},
			want:  `{"x":"y"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeAgentProject(t, tc.src)
			_, _, run := runAgentWorkflow(t, root, "Main", tc.input)
			var out map[string]json.RawMessage
			if err := json.Unmarshal([]byte(run.OutputJSON), &out); err != nil {
				t.Fatalf("output json: %v %s", err, run.OutputJSON)
			}
			raw, ok := out["value"]
			if !ok {
				t.Fatalf("output has no value key: %s", run.OutputJSON)
			}
			if string(raw) != tc.want {
				t.Fatalf("caller-visible value %s, want %s (output %s)", raw, tc.want, run.OutputJSON)
			}
		})
	}
}

// Schema validation operates on the actual document: a scalar document is
// validated as that scalar, never as the {"value": ...} wrapper.
func TestValidateWorkflowInput_nonObjectDocument(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("str.json", `{"type":"string"}`)
	write("arr.json", `{"type":"array","items":{"type":"number"}}`)
	write("null.json", `{"type":"null"}`)
	ex := &Executor{ProjectRoot: root}

	cases := []struct {
		name    string
		schema  string
		doc     any
		wantErr bool
	}{
		{"string ok", "./str.json", "hello", false},
		{"string rejects wrapper", "./str.json", map[string]any{"value": "hello"}, true},
		{"string rejects array", "./str.json", []any{"hello"}, true},
		{"array ok", "./arr.json", []any{1.0, 2.0}, false},
		{"array rejects bad element", "./arr.json", []any{"x"}, true},
		{"null ok", "./null.json", nil, false},
		{"null rejects string", "./null.json", "x", true},
	}
	for _, tc := range cases {
		err := ex.validateWorkflowInputSchema(wfWithInputSchema(tc.schema), tc.doc)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

// docResumeGraph is parent -> child where child runs a HITL-gated tool and then
// returns its whole input document. The parent passes input.doc as the single
// positional argument.
func docResumeGraph() *spec.ProjectGraph {
	g := nestedCFGraph()
	g.Tools["publisher"] = &spec.ToolResource{APIVersion: spec.APIVersionV0, Kind: spec.KindTool, Metadata: spec.Metadata{Name: "publisher"}, Spec: spec.ToolSpec{Type: "native", Safety: &spec.ToolSafety{SideEffects: spec.BoolPtr(true)}}}
	g.Policies["gate"] = &spec.PolicyResource{Spec: spec.PolicySpec{
		Approvals: &spec.PolicyApprovals{RequiredFor: []string{"tool.publisher.echo"}},
		Hitl: &spec.HitlPolicy{InterruptOn: map[string]spec.HitlInterruptValue{
			"publisher": {Enabled: true, Config: &spec.HitlInterruptConfig{AllowedDecisions: []spec.HitlDecisionKind{spec.HitlDecisionApprove, spec.HitlDecisionReject}}},
		}},
	}}
	g.Workflows["child"].Spec.Policy = "gate"
	g.Workflows["child"].Spec.Steps = []spec.WorkflowStep{{ID: "a", Uses: "tool.publisher.echo"}}
	g.Workflows["parent"].Spec.Policy = "gate"
	return g
}

func docResumePrograms() map[string]*execir.Program {
	return map[string]*execir.Program{
		"parent": {Workflow: "parent", Params: []string{"input"}, Body: []execir.Node{
			&execir.InvokeWorkflow{Bind: "c", Workflow: "child", WholeDocument: true, ProjectValue: true, Args: map[string]execir.Value{"value": execir.Ref{Path: []string{"input", "doc"}}}},
			&execir.Return{Value: execir.Ref{Path: []string{"c"}}},
		}},
		"child": {Workflow: "child", Params: []string{"value"}, Body: []execir.Node{
			&execir.InvokeTool{Bind: "a", Uses: "tool.publisher.echo"},
			&execir.Return{Value: execir.Ref{Path: []string{"value"}}},
		}},
	}
}

// TestExecIRResume_NestedWholeDocumentSurvivesCheckpoint suspends a callee at a
// HITL gate while it holds a non-object input document, asserts the nested frame
// in the serialized checkpoint carries exactly that document, then resumes
// (rehydrating from the stored JSON) and asserts the callee still sees the same
// document and the gated effect runs exactly once.
func TestExecIRResume_NestedWholeDocumentSurvivesCheckpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  any
		json string // expected nested.input in the serialized checkpoint
	}{
		{"string", "hello", `"hello"`},
		{"array", []any{"a", "b", 3.0}, `["a","b",3]`},
		{"null", nil, `null`},
		{"number", 42.5, `42.5`},
		{"bool", true, `true`},
		{"object", map[string]any{"x": "y"}, `{"x":"y"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ex, ct, runID, started := newResumeExecutor(t, docResumeGraph(), "parent")
			ex.Executables = docResumePrograms()
			ctx := context.Background()
			input := map[string]any{"doc": tc.doc}

			if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "parent", Env: "dev", StartedAt: started, Input: input}); !errors.Is(err, ErrInterrupted) {
				t.Fatalf("fresh run should interrupt at the child gate, got %v", err)
			}

			// The document is serialized in the nested frame as itself, not wrapped.
			cp, err := ex.Store.GetLatestCheckpoint(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Nested *struct {
					Workflow string          `json:"workflow"`
					Input    json.RawMessage `json:"input"`
				} `json:"nested"`
			}
			if err := json.Unmarshal([]byte(cp.ContextJSON), &payload); err != nil {
				t.Fatalf("checkpoint json: %v", err)
			}
			if payload.Nested == nil || payload.Nested.Workflow != "child" {
				t.Fatalf("checkpoint has no nested child frame: %s", cp.ContextJSON)
			}
			if got := string(payload.Nested.Input); got != tc.json {
				t.Fatalf("nested.input = %s, want %s (checkpoint %s)", got, tc.json, cp.ContextJSON)
			}

			// The frame decodes back to the same value (JSON number semantics).
			var frame NestedRunState
			var raw struct {
				Nested json.RawMessage `json:"nested"`
			}
			if err := json.Unmarshal([]byte(cp.ContextJSON), &raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw.Nested, &frame); err != nil {
				t.Fatalf("nested frame: %v", err)
			}
			if tc.doc != nil && !reflect.DeepEqual(frame.Input, tc.doc) {
				t.Fatalf("hydrated nested input %#v, want %#v", frame.Input, tc.doc)
			}
			if tc.doc == nil && frame.Input != nil {
				t.Fatalf("hydrated nested input %#v, want nil", frame.Input)
			}

			if err := ex.Run(ctx, RunInput{
				RunID: runID, WorkflowName: "parent", Env: "dev", StartedAt: started, Input: input,
				Resume: true, Hitl: HitlRunOptions{Actor: "alice", Decision: &policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: "alice"}},
			}); err != nil {
				t.Fatalf("resume: %v", err)
			}
			run, err := ex.Store.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != state.RunStatusSucceeded {
				t.Fatalf("resume status %q err=%q", run.Status, run.ErrorText)
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal([]byte(run.OutputJSON), &out); err != nil {
				t.Fatalf("output json: %v %s", err, run.OutputJSON)
			}
			if got := strings.TrimSpace(string(out["value"])); got != tc.json {
				t.Fatalf("resumed callee returned %s, want the same document %s (output %s)", got, tc.json, run.OutputJSON)
			}
			if got := ct.count("tool.publisher.echo"); got != 1 {
				t.Fatalf("gated effect ran %d times, want exactly 1", got)
			}
		})
	}
}

// The root checkpoint input stays an object even when the run has no input:
// non-object documents are only legal for a nested frame.
func TestRootCheckpointInputNormalization(t *testing.T) {
	t.Parallel()
	for _, in := range []any{nil, map[string]any(nil)} {
		if got, _ := json.Marshal(rootCheckpointInput(in)); string(got) != `{}` {
			t.Errorf("rootCheckpointInput(%#v) = %s, want {}", in, got)
		}
	}
	m := map[string]any{"a": "b"}
	if got := rootCheckpointInput(m); !reflect.DeepEqual(got, m) {
		t.Errorf("non-empty root input changed: %#v", got)
	}
	s, err := marshalCheckpointPayload(Context{Steps: map[string]StepResult{}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, `"input":{}`) {
		t.Errorf("checkpoint for empty root input: %s", s)
	}
}
