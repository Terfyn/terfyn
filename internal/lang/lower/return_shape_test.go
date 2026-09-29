package lower

import (
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

func TestWorkflowReturnShape(t *testing.T) {
	t.Parallel()
	obj := func(k string) execir.Value {
		return execir.Object{Fields: []execir.Field{{Key: k, Val: execir.Ref{Path: []string{"input", k}}}}}
	}
	ref := execir.Ref{Path: []string{"input", "a"}}
	ret := func(v execir.Value) execir.Node { return &execir.Return{Value: v} }
	branch := func(then, els execir.Node) execir.Node {
		return &execir.Branch{Cond: execir.Leaf{V: ref}, Then: []execir.Node{then}, Else: []execir.Node{els}}
	}
	wfOut := func(out map[string]any) *spec.WorkflowResource {
		wf := &spec.WorkflowResource{}
		if out != nil {
			wf.Spec.Output = &spec.WorkflowOutput{Value: out}
		}
		return wf
	}
	valueOut := map[string]any{"value": "${input.a}"}

	for _, tc := range []struct {
		name string
		body []execir.Node
		wf   *spec.WorkflowResource
		want ReturnShape
	}{
		{"no return", nil, wfOut(nil), ReturnNone},
		{"scalar return", []execir.Node{ret(ref)}, wfOut(valueOut), ReturnValueEnvelope},
		{"object literal", []execir.Node{ret(obj("r"))}, wfOut(map[string]any{"r": "${input.r}"}), ReturnDocument},
		{"multi-return object literals", []execir.Node{branch(ret(obj("r")), ret(obj("s")))}, wfOut(map[string]any{"s": "${input.s}"}), ReturnDocument},
		{"mixed returns", []execir.Node{branch(ret(ref), ret(obj("s")))}, wfOut(map[string]any{"s": "${input.s}"}), ReturnValueEnvelope},
		// A lone `{value: <ref>}` object-literal Return with a `{value: <string>}`
		// resource cannot be the YAML envelope (YAML unwraps a non-map `value` to a
		// non-object Return), so it is the document even unmarked — a `.agent`
		// `return {value: x}` pinned before DocumentReturn existed (review #578).
		{"object literal with only a value key", []execir.Node{ret(obj("value"))}, wfOut(map[string]any{"value": "${input.value}"}), ReturnDocument},
		// The YAML envelope around a map: the Return mirrors X.
		{"yaml value around a map", []execir.Node{ret(obj("a"))}, wfOut(map[string]any{"value": map[string]any{"a": "${input.a}"}}), ReturnValueEnvelope},
		// An unmarked `.agent` `return {value: {a: x}}` (pinned before the bit): its
		// resource is `{value: {a: …}}` too, but the Return is one level deeper
		// than X, so it does not mirror X and is the document.
		{"unmarked value around an object literal", []execir.Node{ret(execir.Object{Fields: []execir.Field{{Key: "value", Val: obj("a")}}})}, wfOut(map[string]any{"value": map[string]any{"a": "${input.a}"}}), ReturnDocument},
		// Same for `return {value: {value: x}}` against `{value: {value: …}}`, and
		// the YAML `output.value: {value: {value: …}}` it must not be confused with.
		{"unmarked value around a value object", []execir.Node{ret(execir.Object{Fields: []execir.Field{{Key: "value", Val: obj("value")}}})}, wfOut(map[string]any{"value": map[string]any{"value": "${input.value}"}}), ReturnDocument},
		{"yaml value around a value map", []execir.Node{ret(obj("value"))}, wfOut(map[string]any{"value": map[string]any{"value": "${input.value}"}}), ReturnValueEnvelope},
		// Key sets must match exactly.
		{"object not mirroring the map", []execir.Node{ret(obj("b"))}, wfOut(map[string]any{"value": map[string]any{"a": "${input.a}"}}), ReturnDocument},
		// A multi-Return program is classified from its Return nodes alone: the
		// flattened resource records only the last-lowered arm, so a `{value: …}`
		// arm must not flip the shape depending on source order (review #578).
		{"value-only arm lowered last", []execir.Node{branch(ret(obj("r")), ret(obj("value")))}, wfOut(map[string]any{"value": "${input.value}"}), ReturnDocument},
		{"value-only arm lowered first", []execir.Node{branch(ret(obj("value")), ret(obj("r")))}, wfOut(map[string]any{"r": "${input.r}"}), ReturnDocument},
		{"mixed returns with a value envelope resource", []execir.Node{branch(ret(obj("r")), ret(ref))}, wfOut(valueOut), ReturnValueEnvelope},
		{"return in loop body", []execir.Node{&execir.Loop{Var: "x", Collection: ref, Body: []execir.Node{ret(obj("r"))}}}, wfOut(map[string]any{"r": "x"}), ReturnDocument},
	} {
		if got := WorkflowReturnShape(&execir.Program{Body: tc.body}, tc.wf); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// DocumentReturn skips the YAML-envelope exception and nothing else: a
	// non-object Return is still the envelope.
	marked := &execir.Program{DocumentReturn: true, Body: []execir.Node{ret(obj("a"))}}
	if got := WorkflowReturnShape(marked, wfOut(map[string]any{"value": map[string]any{"a": "${input.a}"}})); got != ReturnDocument {
		t.Errorf("marked lone object literal: got %v, want ReturnDocument", got)
	}
	marked.Body = []execir.Node{ret(ref)}
	if got := WorkflowReturnShape(marked, wfOut(valueOut)); got != ReturnValueEnvelope {
		t.Errorf("marked scalar return: got %v, want ReturnValueEnvelope", got)
	}
}

// A YAML workflow's output.value lowers to the trailing Return whose shape
// re-creates exactly that output document: `{value: X}` unwraps to `Return X`
// (value envelope), a multi-key or other single-key map is an object literal,
// and `{value: <map>}` stays the value envelope around the map.
func TestWorkflowReturnShape_yamlLowering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  map[string]any
		want ReturnShape
	}{
		{"none", nil, ReturnNone},
		{"single value", map[string]any{"value": "${input.a}"}, ReturnValueEnvelope},
		{"value around a map", map[string]any{"value": map[string]any{"a": "${input.a}"}}, ReturnValueEnvelope},
		{"value around a nested map", map[string]any{"value": map[string]any{"a": map[string]any{"b": "${input.a}"}, "l": []any{map[string]any{"c": 1}}}}, ReturnValueEnvelope},
		{"value around a value map", map[string]any{"value": map[string]any{"value": "${input.a}"}}, ReturnValueEnvelope},
		{"value around an empty map", map[string]any{"value": map[string]any{}}, ReturnValueEnvelope},
		{"value around a list", map[string]any{"value": []any{"${input.a}"}}, ReturnValueEnvelope},
		{"single other key", map[string]any{"got": "${input.a}"}, ReturnDocument},
		{"multi key", map[string]any{"a": "${input.a}", "value": "${input.b}"}, ReturnDocument},
	} {
		wf := &spec.WorkflowResource{Metadata: spec.Metadata{Name: "w"}}
		if tc.out != nil {
			wf.Spec.Output = &spec.WorkflowOutput{Value: tc.out}
		}
		prog, diags := LowerWorkflowResource(wf)
		if diags.HasErrors() {
			t.Fatalf("%s: %v", tc.name, diags)
		}
		if got := WorkflowReturnShape(prog, wf); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
