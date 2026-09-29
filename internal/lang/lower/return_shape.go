package lower

import (
	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

// ReturnShape is how a workflow's interpreter Return value becomes its output
// document — the one value that is the run's output, the persisted run_steps
// output of a workflow: step, and what a YAML caller reads as
// `${steps.<id>.output}` (DESIGN_DOC §13.2). It is a pure function of the
// executable program (and, for an unmarked single object-literal Return only,
// its resource projection — see [WorkflowReturnShape]), so the engine (building the
// output) and the checker (deciding whether a `.agent` caller binds the output's
// `value` field, [execir.InvokeWorkflow.ProjectValue]) can never disagree about
// it.
type ReturnShape int

const (
	// ReturnNone: the program has no Return node, so the output is the empty
	// document {} (a YAML workflow without output.value, a `.agent` workflow with
	// no return statement).
	ReturnNone ReturnShape = iota
	// ReturnDocument: every Return returns an object literal ([execir.Object]), so
	// the returned object IS the output document (`return {a: x}` → `{a: x}`,
	// `return {value: x}` → `{value: x}`, a multi-key YAML output.value) — except
	// the YAML `output.value: {value: <map>}` envelope (see [WorkflowReturnShape]).
	ReturnDocument
	// ReturnValueEnvelope: the output is `{value: <return value>}` — the `.agent`
	// scalar-return convention (`return x`, see lower.go outputValueFor), a
	// program mixing object-literal and other returns, and a YAML output.value of
	// exactly `{value: X}` (which LowerWorkflowResource unwraps to `Return X`).
	ReturnValueEnvelope
)

// WorkflowReturnShape classifies prog's Return nodes (see [ReturnShape]):
//
//   - no Return: [ReturnNone];
//   - any Return that is not an object literal: [ReturnValueEnvelope];
//   - every Return an object literal: [ReturnDocument], with ONE exception — the
//     YAML envelope around a map (below).
//
// The exception. LowerWorkflowResource unwraps `output.value: {value: X}` to
// `Return X`, and that Return is an object literal exactly when X is a map
// (lowerYAMLValue lowers only a map[string]any to an [execir.Object]; a string,
// number, bool, null or sequence becomes a Ref, Template, Lit or List), in which
// case the Return mirrors X: an Object with X's keys at every level where X has
// a map. So an unmarked single object-literal Return is [ReturnValueEnvelope]
// exactly when the resource output.value is `{value: X}`, X is a map, and the
// Return mirrors X ([isYAMLMapEnvelope]); otherwise it is [ReturnDocument].
//
// No `.agent` program meets that test. The only ones it could are those whose
// single Return is a `{value: e}` literal — their flattened resource output.value
// is then `{value: X}` with X the projection of e (outputValueFor) — and such a
// Return is `{value: <e>}`, one level deeper than X, so it never mirrors X (X
// must be a map, i.e. e an object literal; induct on e's depth). LowerExec still
// marks the one form that meets the map condition, `return {value: {…}}`,
// [execir.Program.DocumentReturn], which skips the exception, so a freshly
// lowered `.agent` program's shape is recorded at lowering, not recovered from
// the resource.
//
// Hence every `.agent` program is classified from its Return nodes alone:
// `return {value: x}` outputs the document `{value: x}` whether it is the only
// `return` or one of several, marked or not. That includes a program pinned in a
// deployment snapshot before the bit existed: on main (8741333) a lone
// `return {value: x}` that fired produced `{value: x}` as a nested call — the
// interpolated resource projection — and `{value: {value: x}}` as a root run; it
// now produces `{value: x}` for both, as a fresh apply does
// (internal/engine TestMainPinnedPrograms_keepMainOutputsAndBindings runs
// programs compiled by main). A multi-Return program never
// consults the resource: that only comes from `.agent` source, and its flattened
// resource output.value is whichever `return` was lowered LAST (outputValueFor
// overwrites it per arm).
func WorkflowReturnShape(prog *execir.Program, wf *spec.WorkflowResource) ReturnShape {
	if prog == nil {
		return ReturnNone
	}
	returns, objects := countReturns(prog.Body)
	switch {
	case returns == 0:
		return ReturnNone
	case returns != objects:
		return ReturnValueEnvelope
	case returns == 1 && !prog.DocumentReturn && isYAMLMapEnvelope(soleReturn(prog.Body), wf):
		return ReturnValueEnvelope
	default:
		return ReturnDocument
	}
}

// isYAMLMapEnvelope reports whether ret is the Return LowerWorkflowResource
// produces for wf's resource `output.value: {value: X}` with X a map: X is a map
// and ret's value mirrors it ([mirrorsYAML]). See [WorkflowReturnShape].
func isYAMLMapEnvelope(ret *execir.Return, wf *spec.WorkflowResource) bool {
	if ret == nil || wf == nil || wf.Spec.Output == nil {
		return false
	}
	inner, ok := singleValueField(wf.Spec.Output.Value)
	if !ok {
		return false
	}
	if _, isMap := inner.(map[string]any); !isMap {
		return false
	}
	return mirrorsYAML(ret.Value, inner)
}

// mirrorsYAML reports whether v has the structure lowerYAMLValue gives the YAML
// value x: where x is a map[string]any, v is an [execir.Object] with exactly x's
// keys whose fields mirror x's values; where x is anything else, v is not an
// Object. Leaves are not compared further — only the map/Object skeleton
// distinguishes the YAML envelope from a `.agent` `{value: …}` literal.
func mirrorsYAML(v execir.Value, x any) bool {
	m, isMap := x.(map[string]any)
	obj, isObj := v.(execir.Object)
	if !isMap {
		return !isObj
	}
	if !isObj || len(obj.Fields) != len(m) {
		return false
	}
	for _, f := range obj.Fields {
		sub, ok := m[f.Key]
		if !ok || !mirrorsYAML(f.Val, sub) {
			return false
		}
	}
	return true
}

// soleReturn returns the Return node of a body that has exactly one, else nil.
func soleReturn(body []execir.Node) *execir.Return {
	var found *execir.Return
	count := 0
	walkReturns(body, func(r *execir.Return) {
		count++
		found = r
	})
	if count != 1 {
		return nil
	}
	return found
}

// isLoneValueObjectReturn reports whether body has exactly one Return and it
// returns an object literal whose only key is `value` and whose `value` field is
// itself an object literal (`return {value: {k: x}}`) — the only `.agent`
// program whose resource meets the map condition of [WorkflowReturnShape]'s
// YAML-envelope exception, and so the only one LowerExec marks
// [execir.Program.DocumentReturn]. A lone `return {value: x}` with any other x
// has a non-map resource `value` and is [ReturnDocument] unmarked, so it keeps
// the digest and wire bytes it had before the bit existed, as does every other
// program.
func isLoneValueObjectReturn(body []execir.Node) bool {
	ret := soleReturn(body)
	if ret == nil {
		return false
	}
	obj, ok := ret.Value.(execir.Object)
	if !ok || len(obj.Fields) != 1 || obj.Fields[0].Key != "value" {
		return false
	}
	_, inner := obj.Fields[0].Val.(execir.Object)
	return inner
}

// countReturns counts the Return nodes reachable anywhere in nodes and how many
// of them return an object literal.
func countReturns(nodes []execir.Node) (returns, objects int) {
	walkReturns(nodes, func(r *execir.Return) {
		returns++
		if _, ok := r.Value.(execir.Object); ok {
			objects++
		}
	})
	return returns, objects
}

// walkReturns calls fn on every Return node reachable anywhere in nodes,
// recursing into every control-flow body.
func walkReturns(nodes []execir.Node, fn func(*execir.Return)) {
	for _, n := range nodes {
		switch v := n.(type) {
		case *execir.Return:
			fn(v)
		case *execir.Branch:
			walkReturns(v.Then, fn)
			walkReturns(v.Else, fn)
		case *execir.Loop:
			walkReturns(v.Body, fn)
		case *execir.While:
			walkReturns(v.Body, fn)
		case *execir.Retry:
			walkReturns(v.Body, fn)
		case *execir.Fork:
			for _, br := range v.Branches {
				walkReturns(br.Nodes, fn)
			}
		case *execir.Graph:
			for _, gn := range v.Nodes {
				walkReturns([]execir.Node{gn.Run}, fn)
			}
		}
	}
}
