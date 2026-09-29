package lower

import (
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
)

// wholeDocumentSrc has one call of every agent call shape (#550). Each binds a
// distinct name so the tests can find its node/step.
const wholeDocumentSrc = `
workflow W(input: any) {
    positional = Reviewer(input)
    named_arg0 = Reviewer(arg0: input)
    named_other = Reviewer(topic: input)
    multi = Reviewer(input, input)
    none = Reviewer()
    tool = svc.op(input)
    return positional
}
`

var wantWholeDocument = map[string]bool{
	"positional":  true,
	"named_arg0":  false, // explicitly named: literally an input field called arg0
	"named_other": false,
	"multi":       false, // undefined field-order ABI, never a whole document
	"none":        false,
}

// TestLowerExec_WholeDocumentIsExplicitCallShape: the execution IR carries the
// call shape as data, decided from the AST, not from the arg0 key.
func TestLowerExec_WholeDocumentIsExplicitCallShape(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, wholeDocumentSrc, nil)
	if diags.HasErrors() {
		t.Fatalf("lower diagnostics: %v", diags)
	}
	seen := map[string]bool{}
	for _, n := range prog.Body {
		ia, ok := n.(*execir.InvokeAgent)
		if !ok {
			continue
		}
		want, known := wantWholeDocument[ia.Bind]
		if !known {
			t.Fatalf("unexpected agent bind %q", ia.Bind)
		}
		seen[ia.Bind] = true
		if ia.WholeDocument != want {
			t.Errorf("%s: WholeDocument = %v, want %v (args %v)", ia.Bind, ia.WholeDocument, want, ia.Args)
		}
	}
	if len(seen) != len(wantWholeDocument) {
		t.Fatalf("lowered agent calls %v, want all of %v", seen, wantWholeDocument)
	}
	// named_arg0 and positional have IDENTICAL Args maps; only the bit differs.
	var pos, named *execir.InvokeAgent
	for _, n := range prog.Body {
		if ia, ok := n.(*execir.InvokeAgent); ok {
			switch ia.Bind {
			case "positional":
				pos = ia
			case "named_arg0":
				named = ia
			}
		}
	}
	if _, ok := pos.Args["arg0"]; !ok || len(pos.Args) != 1 {
		t.Fatalf("positional args = %v", pos.Args)
	}
	if _, ok := named.Args["arg0"]; !ok || len(named.Args) != 1 {
		t.Fatalf("named arg0 args = %v", named.Args)
	}
	if (&execir.Program{Body: []execir.Node{pos}}).Digest() == (&execir.Program{Body: []execir.Node{named}}).Digest() {
		t.Fatal("call shape must be part of the program digest")
	}
}

// TestLowerFile_WholeDocumentOnResourceProjection: the resource projection carries
// the same bit, including on synthetic (control-flow) steps, and never on a tool
// call, and LowerWorkflowResource propagates it to the executable program.
func TestLowerFile_WholeDocumentOnResourceProjection(t *testing.T) {
	t.Parallel()
	src := wholeDocumentSrc[:len(wholeDocumentSrc)-len("    return positional\n}\n")] + `
    if input.flag {
        in_if = Reviewer(input)
        in_if_named = Reviewer(arg0: input)
    }
    return positional
}
`
	f, diags := lang.Parse("test.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	res, diags := LowerFile(f, Options{})
	if diags.HasErrors() {
		t.Fatalf("lower: %v", diags)
	}
	if len(res.Workflows) != 1 {
		t.Fatalf("workflows = %d", len(res.Workflows))
	}
	want := map[string]bool{"in_if": true, "in_if_named": false, "svc": false}
	for k, v := range wantWholeDocument {
		want[k] = v
	}
	seen := 0
	for _, st := range res.Workflows[0].Spec.Steps {
		if st.Agent == "" {
			if st.WholeDocument {
				t.Errorf("non-agent step %q must never carry WholeDocument", st.ID)
			}
			continue
		}
		w, ok := want[st.ID]
		if !ok {
			t.Fatalf("unexpected agent step %q", st.ID)
		}
		seen++
		if st.WholeDocument != w {
			t.Errorf("step %q (synthetic=%v): WholeDocument = %v, want %v", st.ID, st.Synthetic, st.WholeDocument, w)
		}
	}
	if seen != 7 {
		t.Fatalf("agent steps seen = %d, want 7", seen)
	}

	// The YAML/resource -> execir path preserves the bit from the step.
	prog, ldiags := LowerWorkflowResource(res.Workflows[0])
	if ldiags.HasErrors() {
		t.Fatalf("LowerWorkflowResource: %v", ldiags)
	}
	got := map[string]bool{}
	for _, n := range flattenNodes(prog.Body) {
		if ia, ok := n.(*execir.InvokeAgent); ok {
			got[ia.Bind] = ia.WholeDocument
		}
	}
	for id, w := range want {
		if id == "svc" {
			continue
		}
		if got[id] != w {
			t.Errorf("resource-lowered program %q: WholeDocument = %v, want %v", id, got[id], w)
		}
	}
}

func flattenNodes(nodes []execir.Node) []execir.Node {
	var out []execir.Node
	for _, n := range nodes {
		out = append(out, n)
		if g, ok := n.(*execir.Graph); ok {
			for _, gn := range g.Nodes {
				out = append(out, flattenNodes([]execir.Node{gn.Run})...)
			}
		}
	}
	return out
}
