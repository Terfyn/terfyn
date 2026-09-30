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
//
// Two further shapes carry no typed producer and run over every consumer:
//   - literal: `C("hi")` vs `with: {body: hello}` — an untyped producer on both paths (LitExpr /
//     a token-free with value), so gradual everywhere except into never;
//   - no argument: `C()` vs a step with no `with:` — rejected into never on both paths. For a typed
//     non-never consumer the .agent arity rule ("declares an input type but was called with no
//     arguments") is deliberately stricter than YAML, where an absent `with` is an empty input
//     object; parity is asserted only where the flow rule decides, i.e. never and untyped consumers.
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

	yamlConsumerFor := func(c side) string {
		if c.schema != "" && !c.never {
			return `{"type":"object","properties":{"body":` + c.schema + `}}`
		}
		return c.schema
	}

	for _, p := range producers {
		for _, c := range consumers {
			p, c := p, c
			t.Run(p.name+"->"+c.name, func(t *testing.T) {
				t.Parallel()
				agentOK := agentFlowAccepted(t, p.schema, c.schema)
				yamlOK := yamlFlowAccepted(t, p.schema, yamlConsumerFor(c))
				if agentOK != yamlOK {
					t.Fatalf(".agent checker accepted=%v but YAML wiring accepted=%v", agentOK, yamlOK)
				}
				if w := want(p, c); agentOK != w {
					t.Fatalf("accepted=%v, want %v", agentOK, w)
				}
			})
		}
	}

	for _, c := range consumers {
		c := c
		t.Run("literal->"+c.name, func(t *testing.T) {
			t.Parallel()
			agentOK := agentCallAccepted(t, c.schema, `C("hi")`)
			yamlOK := yamlWithAccepted(t, yamlConsumerFor(c), "      with:\n        body: hello\n")
			if agentOK != yamlOK {
				t.Fatalf(".agent checker accepted=%v but YAML wiring accepted=%v", agentOK, yamlOK)
			}
			if w := !c.never; agentOK != w {
				t.Fatalf("accepted=%v, want %v", agentOK, w)
			}
		})
		t.Run("no-argument->"+c.name, func(t *testing.T) {
			t.Parallel()
			agentOK := agentCallAccepted(t, c.schema, `C()`)
			yamlOK := yamlWithAccepted(t, yamlConsumerFor(c), "")
			if w := !c.never; yamlOK != w {
				t.Fatalf("YAML accepted=%v, want %v", yamlOK, w)
			}
			if !c.never && c.schema != "" {
				// Typed non-never consumer: the .agent arity rule is stricter by design (see above).
				if agentOK {
					t.Fatalf(".agent zero-argument call to a typed agent must be an error")
				}
				return
			}
			if agentOK != yamlOK {
				t.Fatalf(".agent checker accepted=%v but YAML wiring accepted=%v", agentOK, yamlOK)
			}
		})
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

// agentCallAccepted checks `workflow W(input: Seed) { <call> }` where call invokes agent C, whose
// input is consumer — a flow with no typed producer (a literal argument or no argument).
func agentCallAccepted(t *testing.T, consumer, call string) bool {
	t.Helper()
	root := t.TempDir()
	writeParitySchema(t, root, "Consumed.json", consumer)
	f := parseOrFatal(t, `
agent C {
    model mock/default
    instructions "consume"
    input Consumed
}

workflow W(input: Seed)
{
    `+call+`
}
`)
	_, diags := Check(f, Options{SchemaDir: root})
	return !diags.HasErrors()
}

func yamlFlowAccepted(t *testing.T, producer, consumer string) bool {
	t.Helper()
	return yamlStepAccepted(t, producer, consumer, "      with:\n        body: ${steps.r.output}\n")
}

// yamlWithAccepted runs a consumer step whose with block (YAML lines, "" for none) carries no
// token, after a reporter step with an untyped output.
func yamlWithAccepted(t *testing.T, consumer, with string) bool {
	t.Helper()
	return yamlStepAccepted(t, "", consumer, with)
}

func yamlStepAccepted(t *testing.T, producer, consumer, with string) bool {
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
`+with), "workflow.yaml")
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
