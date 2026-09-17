package spec

import (
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

func TestValidateProjectGraph_agentPositionalArg0IsWholeDocument(t *testing.T) {
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", `{"type":"string"}`)
	writeSchema(t, root, "schemas/in.json", `{"type":"string"}`)

	wfYAML := `apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: demo
spec:
  steps:
    - id: value
      agent: reporter
    - id: return_consumer
      agent: consumer
      with:
        arg0: ${steps.value.output}
`
	dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProjectGraph(wiringGraph(dec.Resource.(*WorkflowResource)), root); err != nil {
		t.Fatalf("single positional agent arg0 is the whole input document, got %v", err)
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
