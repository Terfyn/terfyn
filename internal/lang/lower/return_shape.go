package lower

import (
	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

// ReturnShape is how a workflow's interpreter Return value becomes its output
// document — the one value that is the run's output, the persisted run_steps
// output of a workflow: step, and what a YAML caller reads as
// `${steps.<id>.output}` (DESIGN_DOC §13.2). It is a pure function of the
// executable program (and, for a single-Return program only, its resource
// projection — see [WorkflowReturnShape]), so the engine (building the output)
// and the checker (deciding whether a `.agent` caller binds the output's `value`
// field, [execir.InvokeWorkflow.ProjectValue]) can never disagree about it.
type ReturnShape int

const (
	// ReturnNone: the program has no Return node, so the output is the empty
	// document {} (a YAML workflow without output.value, a `.agent` workflow with
	// no return statement).
	ReturnNone ReturnShape = iota
	// ReturnDocument: every Return returns an object literal ([execir.Object]), so
	// the returned object IS the output document (`return {a: x}` → `{a: x}`, a
	// multi-key YAML output.value) — except a single-Return program whose resource
	// output is the `{value: …}` envelope (see [WorkflowReturnShape]).
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
//   - more than one Return: classified from the Return nodes ALONE — every one an
//     object literal is [ReturnDocument], anything else [ReturnValueEnvelope].
//     The resource is not consulted: a multi-Return program only comes from
//     `.agent` source (a YAML workflow lowers output.value to at most one trailing
//     Return), and its flattened resource output.value is whichever `return` was
//     lowered LAST (lower.go outputValueFor overwrites it per arm), so consulting
//     it would make every arm's output depend on source order;
//   - exactly one Return: as above, except that a resource output of exactly
//     `{value: …}` forces [ReturnValueEnvelope]. This is the YAML envelope:
//     LowerWorkflowResource unwraps `output.value: {value: <map>}` to
//     `Return <map>`, which is an object literal the program alone cannot tell
//     from a document. With one Return the resource describes that Return and no
//     other, so the answer is independent of source order. (A `.agent`
//     `return {value: x}` also takes this branch and keeps the envelope, as the
//     root output always did.)
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
	case returns == 1 && IsSingleValueOutput(wf):
		return ReturnValueEnvelope
	default:
		return ReturnDocument
	}
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
	for _, n := range nodes {
		var r, o int
		switch v := n.(type) {
		case *execir.Return:
			r = 1
			if _, ok := v.Value.(execir.Object); ok {
				o = 1
			}
		case *execir.Branch:
			r1, o1 := countReturns(v.Then)
			r2, o2 := countReturns(v.Else)
			r, o = r1+r2, o1+o2
		case *execir.Loop:
			r, o = countReturns(v.Body)
		case *execir.While:
			r, o = countReturns(v.Body)
		case *execir.Retry:
			r, o = countReturns(v.Body)
		case *execir.Fork:
			for _, br := range v.Branches {
				rb, ob := countReturns(br.Nodes)
				r, o = r+rb, o+ob
			}
		case *execir.Graph:
			for _, gn := range v.Nodes {
				rg, og := countReturns([]execir.Node{gn.Run})
				r, o = r+rg, o+og
			}
		}
		returns += r
		objects += o
	}
	return returns, objects
}
