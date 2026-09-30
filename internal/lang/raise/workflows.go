package raise

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// wholeTokenRE matches a string that is EXACTLY one ${…} token (the whole field). InterpolateWalk
// resolves such a field to the referenced VALUE (any type); an embedded token resolves to a string.
var wholeTokenRE = regexp.MustCompile(`^\$\{([^}]*)\}$`)

// embeddedTokenRE matches every ${…} token, for rewriting references inside a template string.
var embeddedTokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// workflow raises a YAML WorkflowResource to a lang.WorkflowDecl (issue #440, ADR 007). It reproduces
// the workflow's behavior — steps, dependencies, approvals, input, and object output — not its source:
// interpolation becomes expressions and the step DAG is linearized into a valid topological order. It
// returns ok=false (and records an Unsupported) for a construct with no .agent form, so the workflow is
// refused rather than mistranslated.
func (r *raiser) workflow(wr *spec.WorkflowResource) (*lang.WorkflowDecl, bool) {
	if wr == nil {
		return nil, false
	}
	name := wr.Metadata.Name
	d := &lang.WorkflowDecl{Name: ident(name)}

	// Input: the workflow's runtime input is a single `input` parameter whose type is the schema's
	// convention name (schemas/<Name>.json → <Name>); an untyped input becomes `input: any`.
	d.Params = []*lang.Param{{Name: ident("input"), Type: &lang.TypeRef{Name: workflowInputType(wr.Spec.Input)}}}
	if p := strings.TrimSpace(wr.Spec.Policy); p != "" {
		d.Policy = ident(p)
	}

	steps, ok := topoSortSteps(wr.Spec.Steps)
	if !ok {
		r.reject("Workflow", name, "spec.steps", "step needs form a cycle; cannot linearize into a .agent body")
		return nil, false
	}
	for _, st := range steps {
		stmt, sok := r.raiseStep(name, st)
		if !sok {
			return nil, false
		}
		if stmt != nil {
			d.Body = append(d.Body, stmt)
		}
	}
	if ret, rok := r.raiseOutput(name, wr.Spec.Output); rok {
		if ret != nil {
			d.Body = append(d.Body, ret)
		}
	} else {
		return nil, false
	}
	return d, true
}

// workflowInputType returns the .agent input type name for a workflow input schema: the schema's
// convention basename (schemas/<Name>.json → <Name>), or `any` when there is no schema.
func workflowInputType(in *spec.WorkflowInput) string {
	if in == nil || strings.TrimSpace(in.Schema) == "" {
		return "any"
	}
	base := path.Base(strings.TrimSpace(in.Schema))
	base = strings.TrimSuffix(base, ".json")
	if base == "" {
		return "any"
	}
	return base
}

// raiseStep raises one WorkflowStep to a statement: an approval to an ApprovalStmt, and a
// uses/agent/workflow call to an `id = callee(args)` AssignStmt. ok=false on an unraiseable construct.
func (r *raiser) raiseStep(wf string, st spec.WorkflowStep) (lang.Stmt, bool) {
	id := strings.TrimSpace(st.ID)
	if spec.StepIsApproval(st) {
		return r.raiseApprovalStep(wf, id, st)
	}
	callee, cok := stepCallee(st)
	if !cok {
		r.reject("Workflow", wf, "spec.steps", "step "+id+" has no uses, agent, workflow, or approval")
		return nil, false
	}
	args, aok := r.raiseWith(wf, st.With)
	if !aok {
		return nil, false
	}
	if st.WholeDocument {
		// The explicit whole-document call shape (#550) raises to a POSITIONAL
		// argument, `id = Agent(value)`; emitting the named `arg0: value` would
		// reload as a named call with a field called arg0 — a different ABI.
		if strings.TrimSpace(st.Agent) == "" || len(args) != 1 || args[0].Name == nil || args[0].Name.Name != spec.WholeDocumentArgKey {
			r.reject("Workflow", wf, "spec.steps", "step "+id+" is marked wholeDocument but is not an agent call with exactly one positional argument")
			return nil, false
		}
		args[0].Name = nil
	}
	return &lang.AssignStmt{
		Target: ident(id),
		Value:  &lang.CallExpr{Callee: callee, Args: args},
	}, true
}

// raiseApprovalStep raises an approval step to an ApprovalStmt with its config and review payload.
func (r *raiser) raiseApprovalStep(wf, id string, st spec.WorkflowStep) (lang.Stmt, bool) {
	s := &lang.ApprovalStmt{Bind: ident(id)}
	if cfg := st.Approval.Config; cfg != nil {
		if strings.TrimSpace(cfg.Description) != "" {
			s.Description = strLit(cfg.Description)
		}
		for _, k := range cfg.RedactKeys {
			s.RedactKeys = append(s.RedactKeys, strLit(k))
		}
	}
	args, aok := r.raiseWith(wf, st.With)
	if !aok {
		return nil, false
	}
	s.With = args
	return s, true
}

// stepCallee returns the .agent callee ref for a uses/agent/workflow step. A tool `tool.<name>.<op>`
// becomes the dotted ref <name>.<op>; an agent/workflow becomes its bare name.
func stepCallee(st spec.WorkflowStep) (*lang.RefExpr, bool) {
	switch {
	case strings.TrimSpace(st.Uses) != "":
		parts := strings.Split(st.Uses, ".")
		if len(parts) < 3 || parts[0] != "tool" {
			return nil, false
		}
		return refFromParts(parts[1:]), true
	case strings.TrimSpace(st.Agent) != "":
		return refFromParts([]string{st.Agent}), true
	case strings.TrimSpace(st.Workflow) != "":
		return refFromParts([]string{st.Workflow}), true
	}
	return nil, false
}

// raiseWith raises a step's `with:` map to sorted named .agent arguments. ok=false on an unraiseable
// argument value.
func (r *raiser) raiseWith(wf string, with map[string]any) ([]*lang.Arg, bool) {
	if len(with) == 0 {
		return nil, true
	}
	keys := make([]string, 0, len(with))
	for k := range with {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*lang.Arg, 0, len(keys))
	for _, k := range keys {
		expr, ok := r.raiseValue(wf, with[k])
		if !ok {
			return nil, false
		}
		out = append(out, &lang.Arg{Name: ident(k), Value: expr})
	}
	return out, true
}

// raiseOutput raises a workflow's object output.value to a `return { … }` statement, or nil when there
// is no output. ok=false on an unraiseable output value.
func (r *raiser) raiseOutput(wf string, out *spec.WorkflowOutput) (lang.Stmt, bool) {
	if out == nil || len(out.Value) == 0 {
		return nil, true
	}
	obj, ok := r.raiseObject(wf, out.Value)
	if !ok {
		return nil, false
	}
	return &lang.ReturnStmt{Value: obj}, true
}

// raiseObject raises a map[string]any to an ObjectExpr with sorted fields.
func (r *raiser) raiseObject(wf string, m map[string]any) (*lang.ObjectExpr, bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	obj := &lang.ObjectExpr{}
	for _, k := range keys {
		v, ok := r.raiseValue(wf, m[k])
		if !ok {
			return nil, false
		}
		obj.Fields = append(obj.Fields, &lang.ObjectField{Key: ident(k), Value: v})
	}
	return obj, true
}

// raiseValue converts a with/output value (any) to a .agent expression. Strings become a bare
// reference (whole-field ${…}), a template string, or a plain literal; numbers/bools become literals;
// nested maps become object literals. An array (no .agent literal) or a steps.<id>.meta reference is
// refused.
func (r *raiser) raiseValue(wf string, v any) (lang.Expr, bool) {
	switch val := v.(type) {
	case string:
		return r.raiseStringValue(wf, val)
	case bool:
		return &lang.LitExpr{Kind: lang.KindIdent, Value: val}, true
	case int:
		return &lang.LitExpr{Kind: lang.KindNumber, Value: int64(val)}, true
	case int64:
		return &lang.LitExpr{Kind: lang.KindNumber, Value: val}, true
	case float64:
		return &lang.LitExpr{Kind: lang.KindNumber, Value: val}, true
	case map[string]any:
		return r.raiseObject(wf, val)
	case nil:
		// .agent has no null literal, and null is observably distinct from "" (presence checks, nullable
		// schema fields, additionalProperties). Refuse rather than mistranslate it to an empty string.
		r.reject("Workflow", wf, "spec.steps", "null argument value has no .agent literal form")
		return nil, false
	default:
		r.reject("Workflow", wf, "spec.steps", "unraiseable argument value (arrays and non-scalar/object literals have no .agent form)")
		return nil, false
	}
}

// raiseStringValue raises a with/output string. A whole-field ${…} token becomes the referenced
// expression (the value, any type); a string with embedded tokens becomes a template literal with its
// references rewritten to .agent form; a plain string becomes a string literal.
func (r *raiser) raiseStringValue(wf, s string) (lang.Expr, bool) {
	if m := wholeTokenRE.FindStringSubmatch(s); m != nil {
		ref, ok := interpPathToRef(m[1])
		if !ok {
			r.reject("Workflow", wf, "spec.steps", "interpolation "+s+" has no .agent reference form (only input.* and steps.<id>.output.* are raiseable)")
			return nil, false
		}
		return ref, true
	}
	if strings.Contains(s, "${") {
		rewritten, ok := rewriteTemplate(s)
		if !ok {
			r.reject("Workflow", wf, "spec.steps", "interpolation in "+s+" has no .agent reference form")
			return nil, false
		}
		return &lang.LitExpr{Kind: lang.KindString, Value: rewritten}, true
	}
	return &lang.LitExpr{Kind: lang.KindString, Value: s}, true
}

// rewriteTemplate rewrites the ${…} references inside a template string from the YAML resolver form
// (steps.<id>.output.<rest>, input.<rest>) to the .agent form (<id>.<rest>, input.<rest>). ok=false if
// any token is not a raiseable reference.
func rewriteTemplate(s string) (string, bool) {
	ok := true
	out := embeddedTokenRE.ReplaceAllStringFunc(s, func(tok string) string {
		inner := embeddedTokenRE.FindStringSubmatch(tok)[1]
		ref, refOK := interpPathToRef(inner)
		if !refOK {
			ok = false
			return tok
		}
		return "${" + dottedRefParts(ref) + "}"
	})
	return out, ok
}

// interpPathToRef translates a YAML interpolation path to a .agent reference:
//   - input.<rest>          -> input.<rest>
//   - steps.<id>.output     -> <id>
//   - steps.<id>.output.<r> -> <id>.<r>
//
// A steps.<id>.meta path (or any other shape) is not raiseable.
func interpPathToRef(pathStr string) (*lang.RefExpr, bool) {
	parts := strings.Split(strings.TrimSpace(pathStr), ".")
	if len(parts) == 0 || parts[0] == "" {
		return nil, false
	}
	if parts[0] == "input" {
		return refFromParts(parts), true
	}
	if parts[0] == "steps" {
		// steps.<id>.output(.<rest>)
		if len(parts) < 3 || parts[2] != "output" {
			return nil, false
		}
		id := parts[1]
		rest := parts[3:]
		return refFromParts(append([]string{id}, rest...)), true
	}
	return nil, false
}

func refFromParts(parts []string) *lang.RefExpr {
	ref := &lang.RefExpr{}
	for _, p := range parts {
		ref.Parts = append(ref.Parts, ident(p))
	}
	return ref
}

func dottedRefParts(ref *lang.RefExpr) string {
	names := make([]string, 0, len(ref.Parts))
	for _, p := range ref.Parts {
		names = append(names, p.Name)
	}
	return strings.Join(names, ".")
}

// topoSortSteps returns the steps in a topological order (dependencies before dependents), preserving
// the original relative order among independent steps so a sequential workflow keeps its source order.
// ok=false when the needs edges contain a cycle.
func topoSortSteps(steps []spec.WorkflowStep) ([]spec.WorkflowStep, bool) {
	n := len(steps)
	idx := make(map[string]int, n)
	for i, st := range steps {
		idx[strings.TrimSpace(st.ID)] = i
	}
	visited := make([]int, n) // 0=unseen, 1=in-progress, 2=done
	var order []int
	var visit func(i int) bool
	visit = func(i int) bool {
		switch visited[i] {
		case 2:
			return true
		case 1:
			return false // cycle
		}
		visited[i] = 1
		for _, need := range steps[i].Needs {
			if j, ok := idx[strings.TrimSpace(need)]; ok {
				if !visit(j) {
					return false
				}
			}
		}
		visited[i] = 2
		order = append(order, i)
		return true
	}
	for i := range steps {
		if !visit(i) {
			return nil, false
		}
	}
	out := make([]spec.WorkflowStep, 0, n)
	for _, i := range order {
		out = append(out, steps[i])
	}
	return out, true
}
