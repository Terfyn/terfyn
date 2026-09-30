package check

import (
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

func invokeWorkflowsByBind(nodes []execir.Node) map[string]*execir.InvokeWorkflow {
	out := map[string]*execir.InvokeWorkflow{}
	forEachInvokeWorkflow(nodes, func(v *execir.InvokeWorkflow) { out[v.Bind] = v })
	return out
}

// The checker — which knows each callee's declared parameters, program, and
// resource output — decides both InvokeWorkflow bits and stamps them on the
// execution IR (#551, #552); the runtime never infers them.
func TestCheck_InvokeWorkflowCallShapeBits(t *testing.T) {
	t.Parallel()
	src := `
workflow Identity(value: any) { return value }
workflow Pack(value: any) { return { doc: value, tag: "t" } }
workflow Nothing(value: any) { x = value }
workflow Two(a: any, b: any) { return a }
workflow Main(input: any) {
    pos = Identity(input.doc)
    named = Identity(value: input.doc)
    packed = Pack(input.doc)
    none = Nothing(input.doc)
    two = Two(input.a, input.b)
    yaml = Legacy(input.doc)
    if input.flag { inBranch = Identity(input.doc) }
    Identity(input.doc)
    return Identity(input.doc)
}
`
	legacy := &spec.WorkflowResource{
		APIVersion: spec.APIVersionV0, Kind: spec.KindWorkflow, Metadata: spec.Metadata{Name: "Legacy"},
		Spec: spec.WorkflowSpec{Output: &spec.WorkflowOutput{Value: map[string]any{"value": "${input.x}"}}},
	}
	g := projectWith()
	g.Workflows["Legacy"] = legacy
	prog, diags := Check(parseOrFatal(t, src), Options{Project: g})
	if diags.HasErrors() {
		t.Fatalf("program must check clean: %v", diagMessages(diags))
	}
	main := prog.Executables["Main"]
	if main == nil {
		t.Fatalf("no executable for Main")
	}
	calls := invokeWorkflowsByBind(main.Body)

	for _, tc := range []struct {
		bind           string
		whole, project bool
	}{
		{"pos", true, true},      // one positional arg → single param; scalar-return callee
		{"named", true, true},    // one named arg binding the single param
		{"packed", true, false},  // object-literal return: the output document IS the return value
		{"none", true, false},    // no return: nothing to project
		{"two", false, true},     // multi-param: args map is the document; scalar return is projected
		{"yaml", false, false},   // YAML-only callee: with: map is the document, output document is bound
		{"inBranch", true, true}, // nested in control flow
		{"", true, false},        // effect-only call binds nothing
		{"_t0", true, true},      // hoisted `return Identity(...)` temp
	} {
		v, ok := calls[tc.bind]
		if !ok {
			t.Fatalf("no InvokeWorkflow bound to %q; have %v", tc.bind, calls)
		}
		if v.WholeDocument != tc.whole || v.ProjectValue != tc.project {
			t.Errorf("call %q: WholeDocument=%v ProjectValue=%v, want %v/%v", tc.bind, v.WholeDocument, v.ProjectValue, tc.whole, tc.project)
		}
	}
	if args := calls["pos"].Args; len(args) != 1 || args["value"] == nil {
		t.Errorf("positional call args %v, want the single argument under the parameter name", args)
	}
	if args := calls["yaml"].Args; len(args) != 1 || args["arg0"] == nil {
		t.Errorf("YAML-callee call args %v, want the unchanged argument map", args)
	}
}
