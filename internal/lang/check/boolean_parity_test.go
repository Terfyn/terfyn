package check

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

// TestBooleanSchemaFlowParity runs the same producer→consumer flow through the .agent checker and
// YAML step wiring (spec.ValidateProjectGraph) and requires identical accept/reject (issue #549
// review): both must apply schema.CompatibleLookup, so never is the bottom type (a false producer
// flows into every consumer) and a false consumer accepts only never.
//
// The .agent flow is `r = R(input); C(r)` — R's output against C's whole input. The YAML flow is
// `with: {body: ${steps.r.output}}` — R's output against C's input at "body". So the YAML consumer
// document wraps the slot type in {"properties":{"body": ...}}, except for an impossible consumer,
// whose root is itself false (every with-key of a false document is never).
func TestBooleanSchemaFlowParity(t *testing.T) {
	t.Parallel()
	const neverRef = `{"$ref":"#/$defs/n","$defs":{"n":false}}`
	type side struct {
		name   string
		schema string // "" = no schema file (untyped)
		never  bool
	}
	producers := []side{
		{name: "false", schema: `false`, never: true},
		{name: "ref-false", schema: neverRef, never: true},
		{name: "true", schema: `true`},
		{name: "string", schema: `{"type":"string"}`},
		{name: "integer", schema: `{"type":"integer"}`},
		{name: "untyped"},
	}
	consumers := []side{
		{name: "false", schema: `false`, never: true},
		{name: "ref-false", schema: neverRef, never: true},
		{name: "true", schema: `true`},
		{name: "string", schema: `{"type":"string"}`},
		{name: "untyped"},
	}
	want := func(p, c side) bool {
		if p.never {
			return true // bottom: an impossible producer flows anywhere
		}
		if c.never {
			return false // only never flows into never
		}
		return !(p.name == "integer" && c.name == "string")
	}

	for _, p := range producers {
		for _, c := range consumers {
			p, c := p, c
			t.Run(p.name+"->"+c.name, func(t *testing.T) {
				t.Parallel()
				agentOK := agentFlowAccepted(t, p.schema, c.schema)
				yamlConsumer := c.schema
				if c.schema != "" && !c.never {
					yamlConsumer = `{"type":"object","properties":{"body":` + c.schema + `}}`
				}
				yamlOK := yamlFlowAccepted(t, p.schema, yamlConsumer)
				if agentOK != yamlOK {
					t.Fatalf(".agent checker accepted=%v but YAML wiring accepted=%v", agentOK, yamlOK)
				}
				if w := want(p, c); agentOK != w {
					t.Fatalf("accepted=%v, want %v", agentOK, w)
				}
			})
		}
	}
}

func writeParitySchema(t *testing.T, root, name, body string) {
	t.Helper()
	if body == "" {
		return
	}
	p := filepath.Join(root, "schemas", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func agentFlowAccepted(t *testing.T, producer, consumer string) bool {
	t.Helper()
	root := t.TempDir()
	writeParitySchema(t, root, "Produced.json", producer)
	writeParitySchema(t, root, "Consumed.json", consumer)
	f := parseOrFatal(t, `
agent R {
    model mock/default
    instructions "produce"
    output Produced
}

agent C {
    model mock/default
    instructions "consume"
    input Consumed
}

workflow W(input: Seed)
{
    r = R(input)
    C(r)
}
`)
	_, diags := Check(f, Options{SchemaDir: root})
	return !diags.HasErrors()
}

func yamlFlowAccepted(t *testing.T, producer, consumer string) bool {
	t.Helper()
	root := t.TempDir()
	writeParitySchema(t, root, "out.json", producer)
	writeParitySchema(t, root, "in.json", consumer)
	dec, err := spec.ParseResourceFromBytes([]byte(`apiVersion: agentic.dev/v0
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
        body: ${steps.r.output}
`), "workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reporter := spec.AgentSpec{}
	if producer != "" {
		reporter.Output = &spec.AgentIO{Schema: "./schemas/out.json"}
	}
	cons := spec.AgentSpec{}
	if consumer != "" {
		cons.Input = &spec.AgentIO{Schema: "./schemas/in.json"}
	}
	g := &spec.ProjectGraph{
		Agents: map[string]*spec.AgentResource{
			"reporter": {Kind: spec.KindAgent, Metadata: spec.Metadata{Name: "reporter"}, Spec: reporter},
			"consumer": {Kind: spec.KindAgent, Metadata: spec.Metadata{Name: "consumer"}, Spec: cons},
		},
		Workflows: map[string]*spec.WorkflowResource{"demo": dec.Resource.(*spec.WorkflowResource)},
	}
	return spec.ValidateProjectGraph(g, root) == nil
}
