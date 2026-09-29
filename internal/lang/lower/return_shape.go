package lower

import (
	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

// ReturnShape is how a workflow's interpreter Return value becomes its output
// document — the one value that is the run's output, the persisted run_steps
// output of a workflow: step, and what a YAML caller reads as
// `${steps.<id>.output}` (DESIGN_DOC §13.2). It is a pure function of the
// executable program (and, for an unmarked single-Return program only, its
// resource projection — see [WorkflowReturnShape]), so the engine (building the
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
	// the YAML `{value: <map>}` envelope (see [WorkflowReturnShape]).
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
//     YAML envelope. LowerWorkflowResource unwraps `output.value: {value: <map>}`
//     to a single `Return <map>`, an object literal the Return nodes alone cannot
//     tell from a document, so for a single-Return program whose resource
//     output.value is exactly `{value: …}` the shape is [ReturnValueEnvelope].
//
// The exception can only describe a YAML program. The `.agent` program it would
// misread is one whose single Return is itself a `{value: …}` literal (its
// flattened resource output.value is then `{value: …}` too); LowerExec marks
// exactly that program [execir.Program.DocumentReturn], which skips the
// exception. So every `.agent` program is classified from its Return nodes alone
// — `return {value: x}` is the document `{value: x}` whether it is the only
// `return` or one of several — and the resource is consulted only for an
// unmarked single-Return program: a YAML workflow, or a `.agent` program pinned
// before the bit existed, which keeps the envelope it had then. A multi-Return
// program never consults it: that only comes from `.agent` source, and its
// flattened resource output.value is whichever `return` was lowered LAST
// (outputValueFor overwrites it per arm).
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
	case returns == 1 && !prog.DocumentReturn && IsSingleValueOutput(wf):
		return ReturnValueEnvelope
	default:
		return ReturnDocument
	}
}

// isLoneValueObjectReturn reports whether body has exactly one Return and it
// returns an object literal whose only key is `value` — the `.agent` program
// [WorkflowReturnShape]'s YAML-envelope exception would otherwise misread, and so
// the only one LowerExec marks [execir.Program.DocumentReturn]. Marking nothing
// else keeps every other program's digest and wire bytes unchanged.
func isLoneValueObjectReturn(body []execir.Node) bool {
	var found *execir.Return
	count := 0
	walkReturns(body, func(r *execir.Return) {
		count++
		found = r
	})
	if count != 1 {
		return false
	}
	obj, ok := found.Value.(execir.Object)
	return ok && len(obj.Fields) == 1 && obj.Fields[0].Key == "value"
}

// IsSingleValueOutput reports whether wf's resource output.value is exactly one
// `value:` key — the single-value envelope (see lower.go outputValueFor).
func IsSingleValueOutput(wf *spec.WorkflowResource) bool {
	if wf == nil || wf.Spec.Output == nil {
		return false
	}
	_, ok := singleValueField(wf.Spec.Output.Value)
	return ok
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
