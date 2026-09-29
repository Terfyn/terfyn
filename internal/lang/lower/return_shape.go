package lower

import (
	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

// ReturnShape is how a workflow's interpreter Return value becomes its output
// document — the one value that is the run's output, the persisted run_steps
// output of a workflow: step, and what a YAML caller reads as
// `${steps.<id>.output}` (DESIGN_DOC §13.2). It is a pure function of the
// executable program and its resource projection ([WorkflowReturnShape]), so the
// engine (building the output) and the checker (deciding whether a `.agent`
// caller binds the output's `value` field, [execir.InvokeWorkflow.ProjectValue])
// can never disagree about it.
type ReturnShape int

const (
	// ReturnNone: the program has no Return node, so the output is the empty
	// document {} (a YAML workflow without output.value, a `.agent` workflow with
	// no return statement).
	ReturnNone ReturnShape = iota
	// ReturnDocument: every Return returns an object literal ([execir.Object]) and
	// the resource output is not the single-`value` envelope, so the returned
	// object IS the output document (`return {a: x}` → `{a: x}`, a multi-key YAML
	// output.value).
	ReturnDocument
	// ReturnValueEnvelope: the output is `{value: <return value>}` — the `.agent`
	// scalar-return convention (`return x`, see lower.go outputValueFor) and a YAML
	// output.value of exactly `{value: X}` (which LowerWorkflowResource unwraps to
	// `Return X`).
	ReturnValueEnvelope
)

// WorkflowReturnShape classifies prog's Return nodes against wf's resource output
// (see [ReturnShape]). A program whose Returns mix object literals with other
// values uses the value envelope for every Return, so the output never depends on
// which arm's shape the flattened resource projection happened to record; a
// resource output of exactly `{value: …}` also forces the envelope, because that
// is the documented shape of the step output (and it is what a YAML twin's
// unwrapped `Return X` needs to be re-wrapped into).
func WorkflowReturnShape(prog *execir.Program, wf *spec.WorkflowResource) ReturnShape {
	if prog == nil {
		return ReturnNone
	}
	returns, objects := countReturns(prog.Body)
	switch {
	case returns == 0:
		return ReturnNone
	case returns == objects && !IsSingleValueOutput(wf):
		return ReturnDocument
	default:
		return ReturnValueEnvelope
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
