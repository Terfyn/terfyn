package check

import (
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

// TestCheck_AgentSchemasWired proves an .agent agent's input/output type refs are
// lowered onto the resource projection (#294): a resolved type carries the
// project-root-relative Schema ref (schemas/<Name>.json) AND the compiled document,
// so validate and the runtime enforce structured agent I/O.
func TestCheck_AgentSchemasWired(t *testing.T) {
	t.Parallel()
	src := `
agent Reviewer {
    model mock/gpt-4
    input ReviewRequest
    output Review
}
`
	f := parseOrFatal(t, src)
	prog, diags := Check(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diagMessages(diags))
	}
	ar := prog.Graph.Agents["Reviewer"]
	if ar == nil {
		t.Fatalf("no Reviewer agent in the projection")
	}
	if ar.Spec.Input == nil || ar.Spec.Input.Schema != "schemas/ReviewRequest.json" {
		t.Fatalf("input schema not wired: %+v", ar.Spec.Input)
	}
	if ar.Spec.Input.Resolved == nil {
		t.Fatalf("input schema ref present but not resolved to a compiled document")
	}
	if ar.Spec.Output == nil || ar.Spec.Output.Schema != "schemas/Review.json" || ar.Spec.Output.Resolved == nil {
		t.Fatalf("output schema not wired: %+v", ar.Spec.Output)
	}

	// The wired Schema ref resolves cleanly under full validation (projectRoot =
	// the same schema dir), i.e. validate enforces the agent's structured I/O.
	if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
		t.Fatalf("wired agent schemas should validate, got %v", errs)
	}
}

// TestCheck_AgentSchemaMissingStaysUntyped proves the checker's leniency is kept
// (#294): an agent whose type ref has no schema file is left untyped on the
// projection (Input nil), so it is not forced to fail schema-file validation.
func TestCheck_AgentSchemaMissingStaysUntyped(t *testing.T) {
	t.Parallel()
	src := `
agent Loose {
    model mock/gpt-4
    input NoSuchType
}
`
	f := parseOrFatal(t, src)
	prog, diags := Check(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("a missing agent schema must not be an error (leniency), got %v", diagMessages(diags))
	}
	ar := prog.Graph.Agents["Loose"]
	if ar == nil {
		t.Fatalf("no Loose agent")
	}
	if ar.Spec.Input != nil {
		t.Fatalf("an unresolved type must stay untyped, got %+v", ar.Spec.Input)
	}
}

func TestCheck_AgentBooleanOutputSchemaWires(t *testing.T) {
	t.Parallel()
	src := `
agent Assistant {
    model mock/default
    instructions "test"
    output Never
}
`
	f := parseOrFatal(t, src)
	prog, diags := Check(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("boolean false output schema must compile, got %v", diagMessages(diags))
	}
	ar := prog.Graph.Agents["Assistant"]
	if ar == nil || ar.Spec.Output == nil || ar.Spec.Output.Resolved == nil {
		t.Fatalf("Never output schema not wired: %+v", ar)
	}
	got := ar.Spec.Output.Resolved.Lookup(nil)
	if !got.Impossible {
		t.Fatalf("false output schema must be impossible, got %+v", got)
	}
}
