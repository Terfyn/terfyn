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

// TestValidateProjectGraph_booleanSchemas covers Draft 2020-12 boolean schemas in YAML wiring
// (issue #549 review). Wiring applies schema.CompatibleLookup unconditionally: never is the bottom
// type (an impossible producer flows anywhere), an impossible consumer accepts only never, a false
// subschema under properties/items forbids that key, and a true one is declared.
func TestValidateProjectGraph_booleanSchemas(t *testing.T) {
	cases := []struct {
		name    string
		out, in string
		with    string
		wantErr string // "" = accepted
	}{
		{"false producer into string (whole field)", `false`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output}`, ""},
		{"false producer into string (embedded)", `false`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `x ${steps.r.output}`, ""},
		{"false producer into object", `false`, `{"type":"object","properties":{"body":{"type":"object"}}}`, `${steps.r.output}`, ""},
		{"descent through false producer", `false`, `{"type":"object","properties":{"body":{"type":"object"}}}`, `${steps.r.output.x}`, ""},
		{"ref-false producer into string", `{"$ref":"#/$defs/n","$defs":{"n":false}}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output}`, ""},
		{"false producer into false consumer", `false`, `false`, `${steps.r.output}`, ""},
		{"string into false consumer", `{"type":"object","properties":{"s":{"type":"string"}}}`, `false`, `${steps.r.output.s}`, `(string) does not match Agent/consumer input "body" (never)`},
		{"untyped into false consumer", `true`, `false`, `${steps.r.output}`, `(any) does not match Agent/consumer input "body" (never)`},
		{"embedded into false consumer", `true`, `false`, `x ${steps.r.output}`, `(string) does not match Agent/consumer input "body" (never)`},
		{"string into ref-false consumer", `{"type":"object","properties":{"s":{"type":"string"}}}`, `{"$ref":"#/$defs/n","$defs":{"n":false}}`, `${steps.r.output.s}`, `does not match Agent/consumer input "body" (never)`},
		{"false consumer property forbids key", `{"type":"object","properties":{"s":{"type":"string"}}}`, `{"type":"object","properties":{"body":false}}`, `${steps.r.output.s}`, `with "body" is not declared in Agent/consumer input schema`},
		{"true consumer property is declared", `{"type":"object","properties":{"s":{"type":"string"}}}`, `{"type":"object","properties":{"body":true},"additionalProperties":false}`, `${steps.r.output.s}`, ""},
		{"false producer property is not declared", `{"type":"object","properties":{"body":false}}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output.body}`, `${steps.r.output.body} is not declared in Agent/reporter output schema`},
		{"true producer property is declared", `{"type":"object","properties":{"body":true},"additionalProperties":false}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output.body}`, ""},
		{"items false forbids index", `{"type":"array","items":false}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output.0}`, `${steps.r.output.0} is not declared in Agent/reporter output schema`},
		{"prefixItems before items false", `{"type":"array","prefixItems":[{"type":"integer"}],"items":false}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output.0}`, `(integer) does not match Agent/consumer input "body" (string)`},
		{"true consumer pattern is declared", `{"type":"object","properties":{"s":{"type":"string"}}}`, `{"type":"object","patternProperties":{"^body$":true},"additionalProperties":false}`, `${steps.r.output.s}`, ""},
		{"false consumer pattern forbids key", `{"type":"object","properties":{"s":{"type":"string"}}}`, `{"type":"object","patternProperties":{"^body$":false}}`, `${steps.r.output.s}`, `with "body" is not declared in Agent/consumer input schema`},
		{"typed producer pattern", `{"type":"object","patternProperties":{"^n_":{"type":"integer"}},"additionalProperties":false}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output.n_x}`, `(integer) does not match Agent/consumer input "body" (string)`},
		{"producer key no pattern matches", `{"type":"object","patternProperties":{"^n_":{"type":"integer"}},"additionalProperties":false}`, `{"type":"object","properties":{"body":{"type":"string"}}}`, `${steps.r.output.other}`, `${steps.r.output.other} is not declared in Agent/reporter output schema`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeSchema(t, root, "schemas/out.json", tc.out)
			writeSchema(t, root, "schemas/in.json", tc.in)
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
        body: "` + tc.with + `"
`
			dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateProjectGraph(wiringGraph(dec.Resource.(*WorkflowResource)), root)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidateProjectGraph_literalAndMissingWith covers the with shapes that carry no typed token
// (issue #549 review): a token-free with value is an untyped producer, exactly like a .agent
// literal argument, so it is gradual against typed consumers but rejected by a never (false)
// consumer and by a forbidden key; a step with no with into a never consumer is rejected like a
// .agent zero-argument call to a typed agent.
func TestValidateProjectGraph_literalAndMissingWith(t *testing.T) {
	const (
		neverIn  = `false`
		refNever = `{"$ref":"#/$defs/n","$defs":{"n":false}}`
		strBody  = `{"type":"object","properties":{"body":{"type":"string"}}}`
		closed   = `{"type":"object","properties":{"body":{"type":"string"}},"additionalProperties":false}`
		forbid   = `{"type":"object","properties":{"body":false}}`
	)
	cases := []struct {
		name    string
		in      string
		with    string // YAML lines under `with:`, or "" for a step with no with
		wantErr string // "" = accepted
	}{
		{"string literal into never", neverIn, "body: hello", `literal value (any) does not match Agent/consumer input "body" (never)`},
		{"number literal into never", neverIn, "body: 42", `literal value (any) does not match Agent/consumer input "body" (never)`},
		{"bool literal into never", neverIn, "body: true", `literal value (any) does not match Agent/consumer input "body" (never)`},
		{"null literal into never", neverIn, "body: null", `literal value (any) does not match Agent/consumer input "body" (never)`},
		{"object literal into never", neverIn, "body: {a: 1, b: [x]}", `literal value (any) does not match Agent/consumer input "body" (never)`},
		{"literal into ref-false", refNever, "body: hello", `does not match Agent/consumer input "body" (never)`},
		{"no with into never", neverIn, "", `Agent/consumer input schema is never (false) but the step supplies no with`},
		{"no with into ref-false", refNever, "", `input schema is never (false) but the step supplies no with`},
		{"untyped token into never", neverIn, "body: ${steps.r.status}", `untyped value (any) does not match Agent/consumer input "body" (never)`},
		{"literal into forbidden key", forbid, "body: hello", `with "body" is not declared in Agent/consumer input schema`},
		{"literal into undeclared key of closed object", closed, "other: hello", `with "other" is not declared in Agent/consumer input schema`},
		{"string literal into string (gradual)", strBody, "body: hello", ""},
		{"number literal into string (gradual, like .agent)", strBody, "body: 42", ""},
		{"object literal into string (gradual, like .agent)", strBody, "body: {a: 1}", ""},
		{"no with into typed object", strBody, "", ""},
		{"literal into true consumer", `true`, "body: hello", ""},
		{"no with into true consumer", `true`, "", ""},
		{"never producer beside a literal into never", neverIn, `body: {a: "${steps.r.output}", b: lit}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeSchema(t, root, "schemas/out.json", `false`)
			writeSchema(t, root, "schemas/in.json", tc.in)
			with := ""
			if tc.with != "" {
				with = "      with:\n        " + tc.with + "\n"
			}
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
` + with
			dec, err := ParseResourceFromBytes([]byte(wfYAML), "workflow.yaml")
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateProjectGraph(wiringGraph(dec.Resource.(*WorkflowResource)), root)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
