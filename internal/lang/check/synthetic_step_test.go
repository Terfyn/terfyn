package check

import (
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

// TestCheck_ControlFlowBodyValidatesAsSynthetic reproduces the flagship pattern
// (#305): an agent call taking a single whole-value argument, inside a bounded
// `while`, where a later body step consumes an earlier body step's output. The
// flattened control-flow steps are marked Synthetic, so the executable-graph
// validations — interpolation-predecessor `needs` wiring and per-field input-schema
// mapping of `with` — are skipped for them (they are an effect-analysis
// over-approximation, executed only via the pinned execir program). Before #305 this
// failed with "not a predecessor (needs)" and "arg0 is not declared in ... input
// schema".
func TestCheck_ControlFlowBodyValidatesAsSynthetic(t *testing.T) {
	t.Parallel()
	src := `
agent Rev {
    model mock/gpt-4
    input CodingState
    output CodingState
}

workflow W(input: CodingState) -> CodingState {
    state = input
    while !state.approved limit 2 {
        impl = Rev(state)
        state = Rev(impl)
    }
    return state
}
`
	f := parseOrFatal(t, src)
	prog, diags := Check(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("control-flow body with whole-value agent args must check clean: %v", diagMessages(diags))
	}
	// Full resource validation must also pass — the synthetic steps are skipped.
	if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
		t.Fatalf("flattened control-flow projection should validate, got %v", errs)
	}

	// The flattened body steps are marked Synthetic; the workflow has no top-level
	// (non-synthetic) call steps in this program.
	wf := prog.Graph.Workflows["W"]
	if wf == nil {
		t.Fatalf("no workflow W")
	}
	synth := 0
	for _, st := range wf.Spec.Steps {
		if st.Synthetic {
			synth++
		}
	}
	if synth == 0 {
		t.Fatalf("expected flattened control-flow steps to be marked Synthetic, got none in %+v", wf.Spec.Steps)
	}
}

// TestCheck_StraightLinePositionalAgentArgValidates is the #550 repro: a
// single positional agent call is the whole input document. Straight-line
// (non-synthetic) steps still run per-field with: wiring; that wiring must
// treat arg0 as the document, not a missing input property.
func TestCheck_StraightLinePositionalAgentArgValidates(t *testing.T) {
	t.Parallel()
	src := `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output StringOrInteger
}

agent consumer {
    model mock/default
    instructions "accept a string"
    input String
    output String
}

workflow demo(input: String) -> String {
    value = producer(input)
    return consumer(value)
}
`
	f := parseOrFatal(t, src)
	prog, diags := Check(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("straight-line whole-document agent args must check clean: %v", diagMessages(diags))
	}
	if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
		t.Fatalf("straight-line positional agent arg0 must validate as the whole document, got %v", errs)
	}
	wf := prog.Graph.Workflows["demo"]
	if wf == nil {
		t.Fatalf("no workflow demo")
	}
	var consumer spec.WorkflowStep
	for _, st := range wf.Spec.Steps {
		if st.Agent == "consumer" {
			consumer = st
			break
		}
	}
	if _, ok := consumer.With["arg0"]; !ok {
		t.Fatalf("expected lowering placeholder arg0 on the consumer step, got %+v", consumer.With)
	}
	if consumer.Synthetic {
		t.Fatalf("straight-line consumer step must not be marked Synthetic")
	}
}

// TestCheck_TopLevelStepsAreNotSynthetic guards against over-relaxation: a
// straight-line (non-control-flow) call step is NOT marked Synthetic, so it still
// receives the full executable-graph validation.
func TestCheck_TopLevelStepsAreNotSynthetic(t *testing.T) {
	t.Parallel()
	src := `
workflow W(input: PullRequest) -> Review {
    pr = github.get_pr(input.repo, input.number)
    return pr
}
`
	f := parseOrFatal(t, src)
	prog, _ := Check(f, Options{SchemaDir: "testdata"})
	wf := prog.Graph.Workflows["W"]
	if wf == nil || len(wf.Spec.Steps) == 0 {
		t.Fatalf("expected a top-level step")
	}
	for _, st := range wf.Spec.Steps {
		if st.Synthetic {
			t.Fatalf("a top-level (non-control-flow) step must not be Synthetic: %+v", st)
		}
	}
}
