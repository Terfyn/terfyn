package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/schema"
)

func TestValidateProjectGraph_schemaMismatchReportsPosition(t *testing.T) {
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"summary": { "type": "string" },
			"count": { "type": "integer" }
		},
		"additionalProperties": false
	}`)
	writeSchema(t, root, "schemas/in.json", `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"body": { "type": "object" }
		},
		"additionalProperties": false
	}`)

	wfYAML := `apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: demo
spec:
  steps:
    - id: r
      agent: reporter
    - id: c
      agent: consumer
      with:
        body: ${steps.r.output.summary}
`
	dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wr := dec.Resource.(*WorkflowResource)
	g := wiringGraph(wr)
	err = ValidateProjectGraph(g, root)
	if err == nil {
		t.Fatal("expected schema mismatch")
	}
	msg := err.Error()
	pos := wr.Spec.Steps[1].Pos.String()
	if pos == "" || !strings.Contains(msg, pos) {
		t.Fatalf("want positioned error containing %q, got %q", pos, msg)
	}
	if !strings.Contains(msg, "${steps.r.output.summary}") {
		t.Fatalf("want interpolation path in error, got %q", msg)
	}
	if !strings.Contains(msg, "string") || !strings.Contains(msg, "object") {
		t.Fatalf("want type names in error, got %q", msg)
	}
}

func TestValidateProjectGraph_undeclaredOutputPathReportsPosition(t *testing.T) {
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{
		"type": "object",
		"properties": { "summary": { "type": "string" } },
		"additionalProperties": false
	}`)
	writeSchema(t, root, "schemas/in.json", `{
		"type": "object",
		"properties": { "body": { "type": "string" } },
		"additionalProperties": true
	}`)

	wfYAML := `apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: demo
spec:
  steps:
    - id: r
      agent: reporter
    - id: c
      agent: consumer
      with:
        body: ${steps.r.output.missing}
`
	dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wr := dec.Resource.(*WorkflowResource)
	err = ValidateProjectGraph(wiringGraph(wr), root)
	if err == nil {
		t.Fatal("expected undeclared output path")
	}
	msg := err.Error()
	pos := wr.Spec.Steps[1].Pos.String()
	if pos == "" || !strings.Contains(msg, pos) {
		t.Fatalf("want positioned error containing %q, got %q", pos, msg)
	}
	if !strings.Contains(msg, "not declared") || !strings.Contains(msg, "output.missing") {
		t.Fatalf("got %q", msg)
	}
}

func TestValidateProjectGraph_embeddedTokenStringVsObjectMismatch(t *testing.T) {
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{
		"type": "object",
		"properties": { "summary": { "type": "string" } },
		"additionalProperties": false
	}`)
	writeSchema(t, root, "schemas/in.json", `{
		"type": "object",
		"properties": { "body": { "type": "object" } },
		"additionalProperties": false
	}`)

	wfYAML := `apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: demo
spec:
  steps:
    - id: r
      agent: reporter
    - id: c
      agent: consumer
      with:
        body: "Summary: ${steps.r.output.summary}"
`
	dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wr := dec.Resource.(*WorkflowResource)
	err = ValidateProjectGraph(wiringGraph(wr), root)
	if err == nil {
		t.Fatal("expected embedded stringify vs object mismatch")
	}
	msg := err.Error()
	if !strings.Contains(msg, wr.Spec.Steps[1].Pos.String()) {
		t.Fatalf("want position, got %q", msg)
	}
	if !strings.Contains(msg, "string") || !strings.Contains(msg, "object") {
		t.Fatalf("got %q", msg)
	}
}

func TestValidateProjectGraph_matchingSchemasOK(t *testing.T) {
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{
		"type": "object",
		"properties": {
			"summary": { "type": "string" },
			"findings": { "type": "array" }
		},
		"additionalProperties": false
	}`)
	writeSchema(t, root, "schemas/in.json", `{
		"type": "object",
		"properties": {
			"body": { "type": "string" },
			"findings": { "type": "array" }
		},
		"additionalProperties": true
	}`)

	wfYAML := `apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: demo
spec:
  steps:
    - id: r
      agent: reporter
    - id: c
      agent: consumer
      with:
        body: ${steps.r.output.summary}
        findings: ${steps.r.output.findings}
`
	dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProjectGraph(wiringGraph(dec.Resource.(*WorkflowResource)), root); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProjectGraph_absentSchemasStillOK(t *testing.T) {
	wfYAML := `apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: demo
spec:
  steps:
    - id: r
      agent: reporter
    - id: c
      agent: consumer
      with:
        body: ${steps.r.output.summary}
`
	dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	g := &ProjectGraph{
		Agents: map[string]*AgentResource{
			"reporter": {Kind: KindAgent, Metadata: Metadata{Name: "reporter"}},
			"consumer": {Kind: KindAgent, Metadata: Metadata{Name: "consumer"}},
		},
		Workflows: map[string]*WorkflowResource{"demo": dec.Resource.(*WorkflowResource)},
	}
	if err := ValidateProjectGraph(g, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProjectGraph_loadsSchemaOntoGraph(t *testing.T) {
	root := t.TempDir()
	writeSchema(t, root, "schemas/in.json", `{"type":"object","properties":{"pr":{"type":"object"}}}`)
	g := &ProjectGraph{
		Agents: map[string]*AgentResource{
			"a": {
				Kind:     KindAgent,
				Metadata: Metadata{Name: "a"},
				Spec: AgentSpec{
					Input: &AgentIO{Schema: "./schemas/in.json"},
				},
			},
		},
	}
	if err := ValidateProjectGraph(g, root); err != nil {
		t.Fatal(err)
	}
	if g.Agents["a"].Spec.Input.Resolved == nil {
		t.Fatal("expected input.schema to be loaded onto the graph")
	}
	got := g.Agents["a"].Spec.Input.Resolved.Lookup([]string{"pr"})
	if !got.Known || !got.Types.Has(schema.TypeObject) {
		t.Fatalf("pr lookup = %+v", got)
	}
}

func wiringGraph(wr *WorkflowResource) *ProjectGraph {
	return &ProjectGraph{
		Agents: map[string]*AgentResource{
			"reporter": {
				Kind:     KindAgent,
				Metadata: Metadata{Name: "reporter"},
				Spec: AgentSpec{
					Output: &AgentIO{Schema: "./schemas/out.json"},
				},
			},
			"consumer": {
				Kind:     KindAgent,
				Metadata: Metadata{Name: "consumer"},
				Spec: AgentSpec{
					Input:  &AgentIO{Schema: "./schemas/in.json"},
					Output: &AgentIO{Schema: "./schemas/out.json"},
				},
			},
		},
		Workflows: map[string]*WorkflowResource{"demo": wr},
	}
}

func writeSchema(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wholeDocumentGraph builds the straight-line consumer step of the #550 repro with
// the given with: map and explicit call shape.
func wholeDocumentGraph(t *testing.T, with map[string]any, whole bool) *ProjectGraph {
	t.Helper()
	wr := &WorkflowResource{
		Kind:     KindWorkflow,
		Metadata: Metadata{Name: "demo"},
		Spec: WorkflowSpec{Steps: []WorkflowStep{
			{ID: "value", Agent: "reporter"},
			{ID: "return_consumer", Agent: "consumer", With: with, WholeDocument: whole},
		}},
	}
	return wiringGraph(wr)
}

func validateWholeDocumentGraph(t *testing.T, with map[string]any, whole bool) error {
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{"type":"string"}`)
	writeSchema(t, root, "schemas/in.json", `{"type":"string"}`)
	return ValidateProjectGraph(wholeDocumentGraph(t, with, whole), root)
}

// A single positional agent argument (explicit WholeDocument bit) is the agent's
// whole input document, so a string producer wired into a string input validates.
func TestValidateProjectGraph_wholeDocumentAgentArgIsRootDocument(t *testing.T) {
	if err := validateWholeDocumentGraph(t, map[string]any{"arg0": "${steps.value.output}"}, true); err != nil {
		t.Fatalf("whole-document agent arg must validate against the root input schema, got %v", err)
	}
}

// The same with: map WITHOUT the bit is a named call whose field is literally arg0:
// it is looked up as a field, and a string input schema declares no such field.
// Validation must not guess the shape from the key.
func TestValidateProjectGraph_namedArg0AgentArgIsAField(t *testing.T) {
	err := validateWholeDocumentGraph(t, map[string]any{"arg0": "${steps.value.output}"}, false)
	if err == nil || !strings.Contains(err.Error(), `with "arg0" is not declared`) {
		t.Fatalf("named arg0 must be checked as a named field, got %v", err)
	}
}

// A multi-argument positional call has no whole-document bit, so it never takes
// the root-document path even though it carries an arg0 key.
func TestValidateProjectGraph_multiPositionalAgentArgsAreNotWholeDocument(t *testing.T) {
	err := validateWholeDocumentGraph(t, map[string]any{
		"arg0": "${steps.value.output}", "arg1": "${steps.value.output}",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("multi-arg positional must be checked per field, got %v", err)
	}
}

// The bit is a representation invariant: on a non-agent step, or an agent step
// whose with: is not exactly one placeholder argument, validation fails loudly.
func TestValidateProjectGraph_wholeDocumentBitShapeInvariant(t *testing.T) {
	err := validateWholeDocumentGraph(t, map[string]any{"arg0": "x", "arg1": "y"}, true)
	if err == nil || !strings.Contains(err.Error(), "wholeDocument requires with") {
		t.Fatalf("extra args with the bit: got %v", err)
	}
	err = validateWholeDocumentGraph(t, map[string]any{"topic": "x"}, true)
	if err == nil || !strings.Contains(err.Error(), "wholeDocument requires with") {
		t.Fatalf("wrong key with the bit: got %v", err)
	}
	g := wholeDocumentGraph(t, map[string]any{"arg0": "x"}, true)
	st := &g.Workflows["demo"].Spec.Steps[1]
	st.Agent, st.Uses = "", "tool.x.y"
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{"type":"string"}`)
	writeSchema(t, root, "schemas/in.json", `{"type":"string"}`)
	err = ValidateProjectGraph(g, root)
	if err == nil || !strings.Contains(err.Error(), "only valid on an agent step") {
		t.Fatalf("bit on a tool step: got %v", err)
	}
}

// The bit is resource JSON identity: it survives marshal/unmarshal (the deployment
// snapshot form) and changes the JSON, and false is absent.
func TestWorkflowStep_wholeDocumentJSONIdentity(t *testing.T) {
	for _, whole := range []bool{true, false} {
		raw, err := json.Marshal(WorkflowStep{ID: "s", Agent: "a", With: map[string]any{"arg0": "x"}, WholeDocument: whole})
		if err != nil {
			t.Fatal(err)
		}
		if has := strings.Contains(string(raw), "wholeDocument"); has != whole {
			t.Fatalf("whole=%v: json %s", whole, raw)
		}
		var back WorkflowStep
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back.WholeDocument != whole {
			t.Fatalf("whole=%v: hydrated %v", whole, back.WholeDocument)
		}
	}
}
