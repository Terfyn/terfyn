package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/schema"
	"gopkg.in/yaml.v3"
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

// The literal / untyped fallback (a with value with no typed token) honours the explicit call
// shape exactly like the token path (#575 review): a whole-document argument is checked at the
// consumer's input root, so a literal or a bare ${input} into a closed-object or scalar input
// validates, and only a never root rejects it. Without the bit the same key is a field called
// arg0 (a named call, or one argument of a multi-positional call) and is checked as one.
func TestValidateProjectGraph_wholeDocumentLiteralFallback(t *testing.T) {
	const closed = `{"type":"object","properties":{"title":{"type":"string"}},"additionalProperties":false}`
	cases := []struct {
		name     string
		consumer string
		with     map[string]any
		whole    bool
		wantErr  string // "" = accepted
	}{
		{name: "string literal into scalar", consumer: `{"type":"string"}`, with: map[string]any{"arg0": "hi"}, whole: true},
		{name: "number literal into scalar", consumer: `{"type":"string"}`, with: map[string]any{"arg0": 42}, whole: true},
		{name: "object literal into closed object", consumer: closed, with: map[string]any{"arg0": map[string]any{"title": "hi"}}, whole: true},
		{name: "bare input into closed object", consumer: closed, with: map[string]any{"arg0": "${input}"}, whole: true},
		{name: "status token into scalar", consumer: `{"type":"string"}`, with: map[string]any{"arg0": "${steps.value.status}"}, whole: true},
		{name: "literal into never", consumer: `false`, with: map[string]any{"arg0": "hi"}, whole: true,
			wantErr: `literal value (any) does not match Agent/consumer input "input" (never)`},
		{name: "empty object literal into never", consumer: `false`, with: map[string]any{"arg0": map[string]any{}}, whole: true,
			wantErr: `literal value (any) does not match Agent/consumer input "input" (never)`},
		{name: "bare input into never", consumer: `false`, with: map[string]any{"arg0": "${input}"}, whole: true,
			wantErr: `untyped value (any) does not match Agent/consumer input "input" (never)`},
		{name: "named arg0 literal into closed object", consumer: closed, with: map[string]any{"arg0": "hi"},
			wantErr: `with "arg0" is not declared`},
		{name: "multi-positional literals into closed object", consumer: closed, with: map[string]any{"arg0": "a", "arg1": "b"},
			wantErr: `with "arg0" is not declared`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeSchema(t, root, "schemas/out.json", `{"type":"string"}`)
			writeSchema(t, root, "schemas/in.json", tc.consumer)
			err := ValidateProjectGraph(wholeDocumentGraph(t, tc.with, tc.whole), root)
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

// validateWiring validates the demo graph (value -> return_consumer) with the given
// producer output / consumer input schemas and consumer with: map.
func validateWiring(t *testing.T, outSchema, inSchema string, with map[string]any, whole bool) error {
	t.Helper()
	root := t.TempDir()
	writeSchema(t, root, "schemas/out.json", outSchema)
	writeSchema(t, root, "schemas/in.json", inSchema)
	return ValidateProjectGraph(wholeDocumentGraph(t, with, whole), root)
}

const reviewRequestSchema = `{"type":"object","properties":{"repo":{"type":"string"},"number":{"type":"integer"}},"additionalProperties":false}`

// An object-literal whole document (`reviewer({repo: v, number: 1})`) checks each
// interpolated field against THAT field's input type, not against the root input
// document (#550 review): a string into repo validates, an integer is rejected
// naming the field, and an undeclared field is rejected naming it.
func TestValidateProjectGraph_wholeDocumentObjectCheckedPerField(t *testing.T) {
	doc := func(fields map[string]any) map[string]any {
		return map[string]any{WholeDocumentArgKey: fields}
	}
	tok := "${steps.value.output}"
	if err := validateWiring(t, `{"type":"string"}`, reviewRequestSchema, doc(map[string]any{"repo": tok, "number": 1}), true); err != nil {
		t.Fatalf("well-typed object whole document must validate, got %v", err)
	}
	err := validateWiring(t, `{"type":"integer"}`, reviewRequestSchema, doc(map[string]any{"repo": tok, "number": 1}), true)
	if err == nil || !strings.Contains(err.Error(), `input "repo" (string)`) || !strings.Contains(err.Error(), "(integer)") {
		t.Fatalf("ill-typed field must be rejected naming it, got %v", err)
	}
	err = validateWiring(t, `{"type":"string"}`, reviewRequestSchema, doc(map[string]any{"repo": tok, "extra": tok}), true)
	if err == nil || !strings.Contains(err.Error(), `input field "extra" is not declared`) {
		t.Fatalf("undeclared field must be rejected naming it, got %v", err)
	}
}

// A named argument with a nested object value (`consumer(meta: {x: v})`) is looked
// up at meta.x — before the fix x's type was compared to the whole meta object.
func TestValidateProjectGraph_namedNestedObjectCheckedAtNestedPath(t *testing.T) {
	in := `{"type":"object","properties":{"meta":{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}},"additionalProperties":false}`
	with := map[string]any{"meta": map[string]any{"x": "${steps.value.output}"}}
	if err := validateWiring(t, `{"type":"string"}`, in, with, false); err != nil {
		t.Fatalf("well-typed nested field must validate, got %v", err)
	}
	err := validateWiring(t, `{"type":"integer"}`, in, with, false)
	if err == nil || !strings.Contains(err.Error(), `input "meta.x"`) {
		t.Fatalf("ill-typed nested field must be rejected at meta.x, got %v", err)
	}
}

// Array elements are typed by the array's items; an array schema with no items
// accepts any element (unknown, not "not declared").
func TestValidateProjectGraph_arrayElementsCheckedAgainstItems(t *testing.T) {
	tok := "${steps.value.output}"
	typed := `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}}}`
	if err := validateWiring(t, `{"type":"string"}`, typed, map[string]any{"tags": []any{tok}}, false); err != nil {
		t.Fatalf("string element into string items must validate, got %v", err)
	}
	err := validateWiring(t, `{"type":"integer"}`, typed, map[string]any{"tags": []any{"ok", tok}}, false)
	if err == nil || !strings.Contains(err.Error(), `input "tags.1"`) {
		t.Fatalf("integer element into string items must be rejected, got %v", err)
	}
	untypedItems := `{"type":"object","properties":{"tags":{"type":"array"}}}`
	if err := validateWiring(t, `{"type":"integer"}`, untypedItems, map[string]any{"tags": []any{tok}}, false); err != nil {
		t.Fatalf("an array without items accepts any element, got %v", err)
	}
	wholeArray := `{"type":"array","items":{"type":"string"}}`
	err = validateWiring(t, `{"type":"integer"}`, wholeArray, map[string]any{WholeDocumentArgKey: []any{tok}}, true)
	if err == nil || !strings.Contains(err.Error(), `input "0"`) {
		t.Fatalf("whole-document array element must be checked against items, got %v", err)
	}
}

// The "array without items" exemption applies only under a parent that may be an
// array: an element under a closed object or a scalar is undeclared, whole-document
// or named (#579 review).
func TestValidateProjectGraph_arrayElementUnderNonArrayIsUndeclared(t *testing.T) {
	tok := "${steps.value.output}"
	whole := map[string]any{WholeDocumentArgKey: []any{tok}}
	err := validateWiring(t, `{"type":"string"}`, reviewRequestSchema, whole, true)
	if err == nil || !strings.Contains(err.Error(), `input field "0" is not declared`) {
		t.Fatalf("whole-document array into a closed object must be rejected, got %v", err)
	}
	err = validateWiring(t, `{"type":"string"}`, `{"type":"string"}`, whole, true)
	if err == nil || !strings.Contains(err.Error(), `input field "0" is not declared`) {
		t.Fatalf("whole-document array into a string input must be rejected, got %v", err)
	}
	stringTags := `{"type":"object","properties":{"tags":{"type":"string"}}}`
	err = validateWiring(t, `{"type":"string"}`, stringTags, map[string]any{"tags": []any{tok}}, false)
	if err == nil || !strings.Contains(err.Error(), `with "tags.0" is not declared`) {
		t.Fatalf("named array into a string field must be rejected, got %v", err)
	}
	// An array (or untyped) parent without items still accepts any element.
	for _, in := range []string{
		`{"type":"object","properties":{"tags":{"type":"array"}}}`,
		`{"type":"object","properties":{"tags":{"type":["array","string"]}}}`,
		`{"type":"object","properties":{"tags":{}}}`,
	} {
		if err := validateWiring(t, `{"type":"integer"}`, in, map[string]any{"tags": []any{tok}}, false); err != nil {
			t.Fatalf("%s: an element of an array without items is unknown, got %v", in, err)
		}
	}
	if err := validateWiring(t, `{"type":"integer"}`, `{"type":"array"}`, whole, true); err != nil {
		t.Fatalf("whole-document array without items accepts any element, got %v", err)
	}
}

// A token embedded in a larger string yields a string, wherever it sits.
func TestValidateProjectGraph_wholeDocumentEmbeddedTokenIsString(t *testing.T) {
	with := map[string]any{WholeDocumentArgKey: map[string]any{"repo": "org/${steps.value.output}"}}
	if err := validateWiring(t, `{"type":"integer"}`, reviewRequestSchema, with, true); err != nil {
		t.Fatalf("embedded token into a string field must validate, got %v", err)
	}
	with = map[string]any{WholeDocumentArgKey: map[string]any{"number": "n${steps.value.output}"}}
	err := validateWiring(t, `{"type":"integer"}`, reviewRequestSchema, with, true)
	if err == nil || !strings.Contains(err.Error(), `(string) does not match Agent/consumer input "number" (integer)`) {
		t.Fatalf("embedded token into an integer field must be rejected as a string, got %v", err)
	}
}

// YAML export shows the call shape (#550 review): a whole-document step emits
// wholeDocument: true, a named-arg0 step does not, and the marker is rejected by
// the strict decoder so a re-read fails loudly instead of loading the step as a
// named call.
func TestWorkflowStepMarshalYAML_wholeDocumentMarker(t *testing.T) {
	marshal := func(whole bool) []byte {
		t.Helper()
		wr := WorkflowResource{
			APIVersion: APIVersionV0,
			Kind:       KindWorkflow,
			Metadata:   Metadata{Name: "demo"},
			Spec: WorkflowSpec{Steps: []WorkflowStep{
				{ID: "s", Agent: "a", With: map[string]any{WholeDocumentArgKey: "x"}, WholeDocument: whole},
			}},
		}
		out, err := yaml.Marshal(wr)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return out
	}
	whole, named := marshal(true), marshal(false)
	if !strings.Contains(string(whole), "wholeDocument: true") {
		t.Fatalf("whole-document step must emit the marker:\n%s", whole)
	}
	if strings.Contains(string(named), "wholeDocument") {
		t.Fatalf("named-arg0 step must not emit the marker:\n%s", named)
	}
	if _, err := ParseResourceFromBytes(named, "named.yaml"); err != nil {
		t.Fatalf("named-arg0 step must reparse: %v", err)
	}
	if _, err := ParseResourceFromBytes(whole, "whole.yaml"); err == nil || !strings.Contains(err.Error(), "wholeDocument") {
		t.Fatalf("the wholeDocument marker must be rejected by the strict decoder, got %v", err)
	}
}
