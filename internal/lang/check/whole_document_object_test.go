package check

import (
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

// wholeDocumentObjectSrc is the #550 review repro: a straight-line whole-document
// agent call whose argument is an object literal with an interpolated field. The
// producer's output type (String or Count) decides whether the repo field is well
// typed against ReviewRequest {repo: string, number: integer}.
func wholeDocumentObjectSrc(producerOutput string) string {
	return `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output ` + producerOutput + `
}

agent reviewer {
    model mock/default
    instructions "review"
    input ReviewRequest
    output Review
}

workflow demo(input: String) -> Review {
    v = producer(input)
    return reviewer({repo: v, number: 1})
}
`
}

func reviewerStep(t *testing.T, g *spec.ProjectGraph) spec.WorkflowStep {
	t.Helper()
	wf := g.Workflows["demo"]
	if wf == nil {
		t.Fatalf("no workflow demo")
	}
	for _, st := range wf.Spec.Steps {
		if st.Agent == "reviewer" {
			return st
		}
	}
	t.Fatalf("no reviewer step in %+v", wf.Spec.Steps)
	return spec.WorkflowStep{}
}

// A well-typed object-literal whole document checks clean AND validates: graph
// validation checks each interpolated field against that field's input type, not
// against the whole input document (#550: checker and validator cannot disagree).
func TestCheck_StraightLineWholeDocumentObjectLiteralValidates(t *testing.T) {
	t.Parallel()
	prog, diags := Check(parseOrFatal(t, wholeDocumentObjectSrc("String")), Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("well-typed object-literal whole document must check clean: %v", diagMessages(diags))
	}
	st := reviewerStep(t, prog.Graph)
	if !st.WholeDocument || st.Synthetic {
		t.Fatalf("expected a straight-line whole-document step, got %+v", st)
	}
	if _, ok := st.With[spec.WholeDocumentArgKey].(map[string]any); !ok {
		t.Fatalf("expected the object literal under %q, got %+v", spec.WholeDocumentArgKey, st.With)
	}
	if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
		t.Fatalf("well-typed object-literal whole document must validate, got %v", errs)
	}
}

// An ill-typed field is rejected by BOTH the checker and graph validation, and the
// validator names the field.
func TestCheck_StraightLineWholeDocumentObjectLiteralIllTypedFieldRejected(t *testing.T) {
	t.Parallel()
	prog, diags := Check(parseOrFatal(t, wholeDocumentObjectSrc("Count")), Options{SchemaDir: "testdata"})
	if !diags.HasErrors() || !strings.Contains(strings.Join(diagMessages(diags), "\n"), `field "repo"`) {
		t.Fatalf("integer into the string repo field must be a checker error naming the field, got %v", diagMessages(diags))
	}
	errs := spec.ValidateProjectGraph(prog.Graph, "testdata")
	if errs == nil {
		t.Fatalf("integer into the string repo field must fail graph validation")
	}
	if msg := errs.Error(); !strings.Contains(msg, `input "repo"`) || !strings.Contains(msg, "integer") {
		t.Fatalf("validation error must name the repo field and the producer type, got %v", msg)
	}
}

// A NAMED call with a nested object value is looked up at its nested path
// (meta.x), not at the with: key: the older false positive compared x's type to
// the whole meta object.
func TestCheck_NamedNestedObjectArgValidatesPerField(t *testing.T) {
	t.Parallel()
	src := func(out string) string {
		return `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output ` + out + `
}

agent consumer {
    model mock/default
    instructions "consume"
    input MetaRequest
    output String
}

workflow demo(input: String) -> String {
    v = producer(input)
    return consumer(meta: {x: v})
}
`
	}
	prog, _ := Check(parseOrFatal(t, src("String")), Options{SchemaDir: "testdata"})
	if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
		t.Fatalf("named nested object with a well-typed field must validate, got %v", errs)
	}
	prog, _ = Check(parseOrFatal(t, src("Count")), Options{SchemaDir: "testdata"})
	errs := spec.ValidateProjectGraph(prog.Graph, "testdata")
	if errs == nil || !strings.Contains(errs.Error(), `input "meta.x"`) {
		t.Fatalf("ill-typed nested field must be rejected at meta.x, got %v", errs)
	}
}

// An undeclared field of a closed input type is rejected by both layers too.
func TestCheck_StraightLineWholeDocumentObjectLiteralUndeclaredFieldRejected(t *testing.T) {
	t.Parallel()
	src := strings.Replace(wholeDocumentObjectSrc("String"), "{repo: v, number: 1}", "{repo: v, extra: v}", 1)
	prog, diags := Check(parseOrFatal(t, src), Options{SchemaDir: "testdata"})
	if !diags.HasErrors() || !strings.Contains(strings.Join(diagMessages(diags), "\n"), `field "extra" is not declared`) {
		t.Fatalf("undeclared field must be a checker error, got %v", diagMessages(diags))
	}
	errs := spec.ValidateProjectGraph(prog.Graph, "testdata")
	if errs == nil || !strings.Contains(errs.Error(), `input field "extra" is not declared`) {
		t.Fatalf("undeclared field must fail graph validation, got %v", errs)
	}
}

// The same ill-typed object literal inside a control-flow body (a Synthetic step,
// which graph validation skips) is still rejected — by the checker — so a synthetic
// step cannot run what a straight-line step would refuse.
func TestCheck_ControlFlowWholeDocumentObjectLiteralIllTypedFieldRejected(t *testing.T) {
	t.Parallel()
	src := `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output Count
}

agent reviewer {
    model mock/default
    instructions "review"
    input ReviewRequest
    output ReviewRequest
}

workflow demo(input: String) -> ReviewRequest {
    v = producer(input)
    r = reviewer({repo: input, number: 1})
    while r.number > 0 limit 2 {
        r = reviewer({repo: v, number: 1})
    }
    return r
}
`
	_, diags := Check(parseOrFatal(t, src), Options{SchemaDir: "testdata"})
	if !diags.HasErrors() || !strings.Contains(strings.Join(diagMessages(diags), "\n"), `field "repo"`) {
		t.Fatalf("ill-typed field in a loop body must be a checker error, got %v", diagMessages(diags))
	}
}
