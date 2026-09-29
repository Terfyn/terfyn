package check

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/lang/lower"
	"github.com/Terfyn/terfyn/internal/schema"
)

// agentTypeInfo is the resolved input/output schema for one agent declared in
// the compilation unit (f or Options.Files). A nil field is untyped — either
// the .agent source omitted the field, or no schemas/<Name>.json file exists
// for the declared type name (gradual typing: an unresolved type checks as
// untyped rather than failing).
type agentTypeInfo struct {
	Input, Output *schema.Document
}

// workflowTypeInfo is the resolved param/result schema for one workflow.
// ParamOrder preserves declaration order so a positional call argument can be
// matched to the right parameter even without a named arg.
type workflowTypeInfo struct {
	ParamOrder []string
	Params     map[string]*schema.Document
	Result     *schema.Document
}

// typeUniverse resolves every TypeRef reachable from the compilation unit
// against the schemas/<Name>.json convention — a naming rule with no prior
// ADR text, introduced by this package. Only agents and workflows declared in
// this compilation unit (f plus Options.Files) get resolved types; a callee
// that resolves only through Options.Project (a YAML-only sibling) is treated
// as untyped here — wiring full YAML schema interop into this checker is a
// follow-up, not silently assumed.
type typeUniverse struct {
	dir       string
	agents    map[string]agentTypeInfo
	workflows map[string]workflowTypeInfo
}

func resolveTypes(f *lang.File, opts Options) (*typeUniverse, lang.Diagnostics) {
	dir := opts.SchemaDir
	if dir == "" {
		dir = schemaDirFor(f)
	}
	tu := &typeUniverse{dir: dir, agents: map[string]agentTypeInfo{}, workflows: map[string]workflowTypeInfo{}}

	files := make([]*lang.File, 0, len(opts.Files)+1)
	files = append(files, f)
	files = append(files, opts.Files...)

	var diags lang.Diagnostics
	for _, file := range files {
		if file == nil {
			continue
		}
		for _, d := range file.Decls {
			switch decl := d.(type) {
			case *lang.AgentDecl:
				name := identName(decl.Name)
				if name == "" {
					continue
				}
				input, d1 := tu.resolve(decl.Input)
				output, d2 := tu.resolve(decl.Output)
				diags = append(diags, d1...)
				diags = append(diags, d2...)
				tu.agents[name] = agentTypeInfo{Input: input, Output: output}
			case *lang.WorkflowDecl:
				name := identName(decl.Name)
				if name == "" {
					continue
				}
				order := make([]string, 0, len(decl.Params))
				params := make(map[string]*schema.Document, len(decl.Params))
				for _, p := range decl.Params {
					pn := identName(p.Name)
					order = append(order, pn)
					doc, d := tu.resolve(p.Type)
					diags = append(diags, d...)
					params[pn] = doc
				}
				result, dr := tu.resolve(decl.Result)
				diags = append(diags, dr...)
				tu.workflows[name] = workflowTypeInfo{
					ParamOrder: order,
					Params:     params,
					Result:     result,
				}
			}
		}
	}
	return tu, diags
}

// resolve loads t's backing schema document. A TypeRef naming no
// schemas/<Name>.json file is untyped (gradual typing, #193) — that is the
// missing-file case. A file that EXISTS but fails to compile is a different,
// louder failure: the author named a real file and it did not parse, so this
// is reported as an error rather than silently treated the same as "no such
// type." schema.LoadDocument distinguishes the two via FileError (absent) vs
// CompileError (present, broken); only the former is swallowed here.
func (tu *typeUniverse) resolve(t *lang.TypeRef) (*schema.Document, lang.Diagnostics) {
	if t == nil || t.Name == "" {
		return nil, nil
	}
	// The same schemas/<Name>.json convention the resource projection stores as an
	// AgentIO.Schema ref (#294), so the checker and the projection never diverge.
	path := filepath.Join(tu.dir, filepath.FromSlash(lower.SchemaRef(t.Name)))
	doc, err := schema.LoadDocument(path)
	if err == nil {
		return doc, nil
	}
	var compileErr *schema.CompileError
	if errors.As(err, &compileErr) {
		return nil, lang.Diagnostics{{
			Pos: t.Pos,
			Msg: fmt.Sprintf("type %q: %v", t.Name, err),
		}}
	}
	return nil, nil
}

func schemaDirFor(f *lang.File) string {
	if f == nil || f.Pos.File == "" {
		return "."
	}
	return filepath.Dir(f.Pos.File)
}

// typeRef is a value's resolved type: a root schema.Document plus the dotted
// path walked so far. A nil doc means untyped (gradual typing — always
// compatible). Kept as (doc, path) rather than eagerly resolving to a TypeSet
// so a chain like result.summary can extend the path one field at a time,
// mirroring how internal/spec/wiring.go walks interpolation paths against the
// same schema.Document API, but over AST RefExpr.Parts instead of a regex over
// an interpolation string.
//
// lit is a schema-less primitive type for a value whose type is known without a
// schema document — only a string template mixing text and ${…} tokens (always a
// string, see checkTemplate). A zero typeRef (no doc, no lit) is untyped.
type typeRef struct {
	doc  *schema.Document
	path []string
	lit  schema.TypeSet
}

// stringType is the type of a string template that is not a single whole-string token.
var stringType = typeRef{lit: schema.TypeSet{schema.TypeString: {}}}

func (t typeRef) types() schema.TypeSet {
	if t.doc == nil {
		return t.lit
	}
	return t.doc.Lookup(t.path).Types
}

func (t typeRef) child(field string) (typeRef, schema.LookupResult) {
	if t.doc == nil {
		return typeRef{}, schema.LookupResult{}
	}
	p := make([]string, 0, len(t.path)+1)
	p = append(p, t.path...)
	p = append(p, field)
	res := t.doc.Lookup(p)
	return typeRef{doc: t.doc, path: p}, res
}

// rebind records that ONE call site's lowered workflow: step must have its
// placeholder with: keys (arg0, arg1, ...) renamed to the callee's real
// parameter names. Type checking runs over the AST; lower.LowerFile has
// already produced Program.Graph by the time checkTypes runs, and lowering
// has no symbol table of its own to resolve a callee's parameters against
// (that is what this package exists to add) — so the checker computes the
// correct names here and Check applies them to the already-lowered graph as
// a second, narrow pass, via position correlation: pos is the exact
// call-site position lowering already stamped onto the step as WorkflowPos
// (applyCallee in internal/lang/lower/lower.go sets it from the same AST
// Callee node), so finding the one step this rebind belongs to needs no
// re-derivation of lowering's step-id scheme.
//
// renames holds every rename for that ONE call site together (oldKey ->
// newKey), not one rebind per argument: applying them one entry at a time
// against a single live map is unsound whenever a rename's target collides
// with another rename's source (lowering's placeholder namespace and the
// callee's real parameter namespace are the same map, and "arg1" is a legal
// parameter name) — see applyRebinds for why grouping them lets the rewrite
// build a fresh map instead.
type rebind struct {
	pos     lang.Pos
	renames map[string]string
}

// checkTypes type-checks agent invocation arguments and value flow between
// bindings for every workflow across every file in the compilation unit
// (files is f plus every Options.Files entry, the same set already lowered
// and merged onto Program.Graph — checking only f would leave a positional
// workflow: call in another file with its lowered arg0/arg1 keys never
// rebound, and its effects clause never checked), and returns the with: key
// rebinds (see rebind) that Check must apply to the resource projection for
// the result to be an executable graph rather than one that merely
// type-checked.
func checkTypes(files []*lang.File, tu *typeUniverse) (lang.Diagnostics, []rebind) {
	var diags lang.Diagnostics
	var rebinds []rebind
	for _, file := range files {
		if file == nil {
			continue
		}
		for _, d := range file.Decls {
			wd, ok := d.(*lang.WorkflowDecl)
			if !ok {
				continue
			}
			wdDiags, wdRebinds := checkWorkflowTypes(wd, tu)
			diags = append(diags, wdDiags...)
			rebinds = append(rebinds, wdRebinds...)
		}
	}
	return diags, rebinds
}

type wfChecker struct {
	tu      *typeUniverse
	wf      *lang.WorkflowDecl
	env     map[string]typeRef
	rebinds []rebind
}

func checkWorkflowTypes(wd *lang.WorkflowDecl, tu *typeUniverse) (lang.Diagnostics, []rebind) {
	wc := &wfChecker{tu: tu, wf: wd, env: map[string]typeRef{}}
	info := tu.workflows[identName(wd.Name)]
	for _, p := range wd.Params {
		name := identName(p.Name)
		if name == "" {
			continue
		}
		wc.env[name] = typeRef{doc: info.Params[name]}
	}
	var diags lang.Diagnostics
	for _, st := range wd.Body {
		diags = append(diags, wc.checkStmt(st)...)
	}
	return diags, wc.rebinds
}

func (wc *wfChecker) checkStmt(st lang.Stmt) lang.Diagnostics {
	switch s := st.(type) {
	case *lang.AssignStmt:
		t, diags := wc.checkExpr(s.Value)
		wc.env[identName(s.Target)] = t
		return diags
	case *lang.ExprStmt:
		_, diags := wc.checkExpr(s.X)
		return diags
	case *lang.ApprovalStmt:
		// Type-check the review payload's argument expressions (they reference prior bindings), then bind
		// the decision name (untyped/any) so a later step may reference it.
		var diags lang.Diagnostics
		for _, arg := range s.With {
			if arg == nil {
				continue
			}
			_, d := wc.checkValue(arg.Value)
			diags = append(diags, d...)
		}
		wc.env[identName(s.Bind)] = typeRef{}
		return diags
	case *lang.ParallelStmt:
		var diags lang.Diagnostics
		results := make(map[string]typeRef, len(s.Body))
		for _, b := range s.Body {
			t, d := wc.checkExpr(b.Value)
			diags = append(diags, d...)
			results[identName(b.Target)] = t
		}
		for name, t := range results {
			wc.env[name] = t
		}
		return diags
	case *lang.ReturnStmt:
		got, diags := wc.checkValue(s.Value)
		want := typeRef{doc: wc.tu.workflows[identName(wc.wf.Name)].Result}
		diags = append(diags, wc.checkCompatible(s.Value.Position(), got, want, "return value")...)
		return diags
	case *lang.IfStmt:
		// An `if` is EXCLUSIVE choice, not sequential composition: the two arms
		// never see each other's bindings, and a binding is in scope after the
		// `if` only if it is definitely assigned — bound in BOTH arms (or already
		// bound before). Each arm is checked against its own snapshot of the
		// pre-`if` environment, then the arms are joined (joinEnv). This matches
		// the interpreter, which runs exactly one arm on the enclosing scope.
		var diags lang.Diagnostics
		_, d := wc.checkExpr(s.Cond)
		diags = append(diags, d...)

		base := wc.env
		wc.env = snapshotEnv(base)
		for _, st := range s.Then {
			diags = append(diags, wc.checkStmt(st)...)
		}
		thenEnv := wc.env

		wc.env = snapshotEnv(base)
		for _, st := range s.Else {
			diags = append(diags, wc.checkStmt(st)...)
		}
		elseEnv := wc.env

		wc.env = joinEnv(thenEnv, elseEnv)
		return diags
	case *lang.ForStmt:
		// The collection (sequential and parallel alike) is a value position: the
		// execution IR lowers it with lowerValue, so `for x in "${xs}"` iterates the
		// binding xs, and its tokens must resolve here (graph validation never sees
		// s.In — the resource projection does not lower it).
		var diags lang.Diagnostics
		_, d := wc.checkValue(s.In)
		diags = append(diags, d...)
		// The loop variable is untyped — element-type inference from the
		// collection is a follow-up; gradual typing keeps a reference to it
		// compatible with anything. Scoping matches the interpreter exactly, and
		// the body runs with the loop variable bound in BOTH kinds:
		//   - a PARALLEL loop isolates each iteration, so nothing the body binds
		//     escapes (the interpreter runs each iteration in a child scope);
		//   - a SEQUENTIAL loop may run ZERO times, so a name it first binds
		//     (including the loop variable) is NOT definitely assigned afterward
		//     and does not escape; a name that existed BEFORE the loop survives,
		//     but reassignment inside it collapses to a union (untyped), never the
		//     last iteration's type. loopJoin computes exactly that.
		name := identName(s.Var)
		pre := wc.env
		wc.env = snapshotEnv(pre)
		if name != "" {
			wc.env[name] = typeRef{}
		}
		for _, st := range s.Body {
			diags = append(diags, wc.checkStmt(st)...)
		}
		if s.Parallel {
			wc.env = pre
		} else {
			wc.env = loopJoin(pre, wc.env)
		}
		return diags
	case *lang.WhileStmt:
		// A bounded `while` carries state exactly like a SEQUENTIAL `for` (#288):
		// the condition is checked, then the body runs on a snapshot joined back
		// with loopJoin. A name that existed BEFORE the loop may be rebound and
		// survives (collapsed to an untyped union, since the loop may run zero
		// times); a name first bound INSIDE is loop-local and does not escape. This
		// is the loop-carried-state rule the interpreter mirrors (ADR 002 §6).
		var diags lang.Diagnostics
		_, d := wc.checkExpr(s.Cond)
		diags = append(diags, d...)
		pre := wc.env
		wc.env = snapshotEnv(pre)
		for _, st := range s.Body {
			diags = append(diags, wc.checkStmt(st)...)
		}
		wc.env = loopJoin(pre, wc.env)
		return diags
	case *lang.RetryStmt:
		// A bounded `retry until` carries state like `while` (#361): the body runs on a
		// snapshot joined back with loopJoin, then the condition is checked. A name bound
		// before the loop may be rebound and survives (untyped union — the body runs at
		// least once but the success attempt count varies); a name first bound inside is
		// loop-local.
		var diags lang.Diagnostics
		pre := wc.env
		wc.env = snapshotEnv(pre)
		for _, st := range s.Body {
			diags = append(diags, wc.checkStmt(st)...)
		}
		_, d := wc.checkExpr(s.Cond)
		diags = append(diags, d...)
		wc.env = loopJoin(pre, wc.env)
		return diags
	}
	return nil
}

// snapshotEnv shallow-copies the binding environment so an isolated block (a
// parallel loop body) or one `if` arm can bind names that are discarded or
// joined when the block ends.
func snapshotEnv(env map[string]typeRef) map[string]typeRef {
	out := make(map[string]typeRef, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// joinEnv computes the environment after an `if` from the two arms'
// environments, each already derived from a copy of the pre-`if` env. A name is
// in scope after the `if` only if it is present in BOTH arms — i.e. bound before
// the `if` (arms start from the same base, so a pre-existing name is in both) or
// newly bound in both arms. A name bound in only one arm is NOT definitely
// assigned and is dropped, so a later reference to it is not silently well-typed.
// When both arms agree on a name's type it is kept; when they disagree (either
// arm rebound it, or two arms bound it to different types) the joined type is
// untyped/gradual — the honest representation of a union without a union type,
// which stays permissive rather than picking whichever arm ran last.
func joinEnv(thenEnv, elseEnv map[string]typeRef) map[string]typeRef {
	out := make(map[string]typeRef, len(thenEnv))
	for name, tThen := range thenEnv {
		tElse, inElse := elseEnv[name]
		if !inElse {
			continue // bound in the then arm only: not definitely assigned
		}
		out[name] = mergeType(tThen, tElse)
	}
	return out
}

// loopJoin computes the environment after a sequential loop from the pre-loop
// env and the env after one symbolic body pass (which started as a copy of the
// pre-loop env plus the loop variable). Only names that existed BEFORE the loop
// survive — a name the body first binds (the loop variable included) is not
// definitely assigned, since the loop may run zero times. A surviving name whose
// type the body changed collapses to untyped (its post-loop value is either the
// pre-loop value or the last iteration's), never the body's type.
func loopJoin(pre, after map[string]typeRef) map[string]typeRef {
	out := make(map[string]typeRef, len(pre))
	for name, tPre := range pre {
		if tAfter, ok := after[name]; ok {
			out[name] = mergeType(tPre, tAfter)
		} else {
			out[name] = tPre
		}
	}
	return out
}

// mergeType joins a name's type across the two arms: identical types are kept,
// differing types collapse to untyped (gradual), never to one arm's value.
func mergeType(a, b typeRef) typeRef {
	if a.doc == b.doc && samePath(a.path, b.path) && sameTypeSet(a.lit, b.lit) {
		return a
	}
	return typeRef{}
}

func sameTypeSet(a, b schema.TypeSet) bool {
	if len(a) != len(b) {
		return false
	}
	for t := range a {
		if !b.Has(t) {
			return false
		}
	}
	return true
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (wc *wfChecker) checkExpr(e lang.Expr) (typeRef, lang.Diagnostics) {
	switch v := e.(type) {
	case *lang.RefExpr:
		return wc.checkRef(v)
	case *lang.CallExpr:
		return wc.checkCall(v)
	case *lang.BinaryExpr:
		// A comparison or logical connective (#199): its operands are checked
		// for reference well-formedness; the result is an untyped boolean.
		_, dx := wc.checkExpr(v.X)
		_, dy := wc.checkExpr(v.Y)
		return typeRef{}, append(dx, dy...)
	case *lang.UnaryExpr:
		_, d := wc.checkExpr(v.X)
		return typeRef{}, d
	case *lang.LitExpr:
		return typeRef{}, nil
	case *lang.ObjectExpr:
		// An object literal (issue #440): each field value is checked for reference well-formedness;
		// the object itself is untyped (its shape is gradual, like any other structured value).
		// An object literal only occurs in a value position, so its fields are values too.
		var diags lang.Diagnostics
		for _, f := range v.Fields {
			if f == nil {
				continue
			}
			_, d := wc.checkValue(f.Value)
			diags = append(diags, d...)
		}
		return typeRef{}, diags
	}
	return typeRef{}, nil
}

// checkRef resolves a dotted reference (pr, input.repo, result.summary)
// against the environment. An unresolved head — a name the scope model says is
// not in scope at this point — is a compile error, so a reference the runtime
// would fail on is rejected here rather than passing under one model and
// panicking under another (#199). This is the same condition and message
// lowering's prefixOf reports for the resource projection, but the checker's
// env is the AUTHORITY: it is the definite-assignment scope (a one-arm `if`
// binding and a sequential loop's body locals are absent, since a zero-iteration
// loop or the untaken arm never binds them), whereas the resource-flatten env is
// an effect over-approximation that keeps every name any branch binds. Check
// dedups the two when they coincide on a straight-line reference (dedupDiags).
// A member access past a resolved head is an error only when the schema
// positively forbids the path (additionalProperties: false); an untyped head is
// otherwise gradual.
func (wc *wfChecker) checkRef(r *lang.RefExpr) (typeRef, lang.Diagnostics) {
	return wc.resolveRef(r, "unresolved reference %q")
}

// resolveRef is checkRef with the unresolved-head message format as a parameter, so
// a ${…} token in a string template reports the same message lowering does for it.
func (wc *wfChecker) resolveRef(r *lang.RefExpr, unresolvedFmt string) (typeRef, lang.Diagnostics) {
	if r == nil || len(r.Parts) == 0 {
		return typeRef{}, nil
	}
	head := r.Parts[0].Name
	cur, ok := wc.env[head]
	if !ok {
		return typeRef{}, lang.Diagnostics{{
			Pos: r.Pos,
			Msg: fmt.Sprintf(unresolvedFmt, head),
		}}
	}
	for _, p := range r.Parts[1:] {
		if cur.doc == nil {
			return typeRef{}, nil
		}
		next, res := cur.child(p.Name)
		if res.Missing {
			return typeRef{}, lang.Diagnostics{{
				Pos: r.Pos,
				Msg: fmt.Sprintf("%q is not declared in the schema for %q", p.Name, head),
			}}
		}
		cur = next
	}
	return cur, nil
}

// checkValue types e in a VALUE position, which is exactly where the execution IR
// lowers an expression with execLowerer.lowerValue and so interpolates a string
// literal's ${…} tokens (stringTemplateValue): a call argument (agent, workflow or
// tool, statement or binding), a field of an object literal in one of these
// positions, an approval `with` payload entry, a return value, and a `for` /
// `parallel for` collection. A string literal there is typed by checkTemplate;
// every other expression by checkExpr. A literal in any other position — a
// condition operand (if/while/retry, lowered by lowerCond), or the whole value of a
// binding `x = "…"` or a `parallel { }` branch (lowered by lowerAssign as a plain
// Lit) — is never interpolated, so checkExpr keeps it an untyped literal.
func (wc *wfChecker) checkValue(e lang.Expr) (typeRef, lang.Diagnostics) {
	if lit, ok := e.(*lang.LitExpr); ok {
		if s, ok := lit.Value.(string); ok {
			return wc.checkTemplate(s, lit.Pos)
		}
	}
	return wc.checkExpr(e)
}

// templateTokenRE matches a ${…} interpolation token: the same shape lowering
// (internal/lang/lower) and graph validation (internal/spec) use.
var templateTokenRE = regexp.MustCompile(`\$\{([^}]*)\}`)

// checkTemplate types a string value by the rule graph validation applies to an
// interpolated string (internal/spec checkConsumerType), so the checker and the
// validator cannot disagree about it (#550) — including inside control-flow
// bodies, whose Synthetic steps graph validation skips:
//
//   - a string that is exactly one ${binding…} token is the referenced value itself
//     (the execution IR lowers it to a Ref), so it has the binding's type;
//   - any other string containing a token is a Template, so it is a string;
//   - a string with no token is a plain literal and stays untyped (the validator
//     ignores it too).
//
// Each token's inner is a source binding path (lowering maps it to the resource
// ${steps.<id>.output…} form), resolved against the checker's definite-assignment
// env like any reference, so an unresolved head or an undeclared member path is
// reported here too.
func (wc *wfChecker) checkTemplate(s string, pos lang.Pos) (typeRef, lang.Diagnostics) {
	locs := templateTokenRE.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return typeRef{}, nil
	}
	var diags lang.Diagnostics
	var last typeRef
	for _, loc := range locs {
		t, d := wc.checkTemplateToken(s[loc[2]:loc[3]], pos)
		diags = append(diags, d...)
		last = t
	}
	if len(locs) == 1 && locs[0][0] == 0 && locs[0][1] == len(s) {
		return last, diags
	}
	return stringType, diags
}

// checkTemplateToken resolves one ${…} token's inner binding path. The path is
// split the way the execution IR splits it (interpPath: dot-separated, trimmed,
// empty segments dropped); an empty path is left to lowering's diagnostic.
func (wc *wfChecker) checkTemplateToken(inner string, pos lang.Pos) (typeRef, lang.Diagnostics) {
	var parts []*lang.Ident
	for _, p := range strings.Split(inner, ".") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, &lang.Ident{Pos: pos, Name: p})
		}
	}
	if len(parts) == 0 {
		return typeRef{}, nil
	}
	return wc.resolveRef(&lang.RefExpr{Pos: pos, Parts: parts}, "unresolved reference %q in interpolation")
}

// checkCall type-checks a call's arguments against its callee's declared
// parameter/input types (when the callee is declared in this compilation
// unit) and returns the call's own result type.
func (wc *wfChecker) checkCall(c *lang.CallExpr) (typeRef, lang.Diagnostics) {
	var diags lang.Diagnostics
	if c.Callee == nil || len(c.Callee.Parts) == 0 {
		return typeRef{}, nil
	}
	if len(c.Callee.Parts) >= 2 {
		// A dotted callee is a tool call. There is no .agent-visible tool
		// schema, so an argument's own internal well-formedness is still
		// checked (nested calls, member access), but not compatibility against
		// a callee parameter — gradual typing.
		for _, arg := range c.Args {
			_, d := wc.checkValue(arg.Value)
			diags = append(diags, d...)
		}
		return typeRef{}, diags
	}
	name := c.Callee.Parts[0].Name
	if wi, ok := wc.tu.workflows[name]; ok {
		diags = append(diags, wc.checkWorkflowArgs(name, wi, c)...)
		return typeRef{doc: wi.Result}, diags
	}
	if ai, ok := wc.tu.agents[name]; ok {
		diags = append(diags, wc.checkAgentArgs(name, ai, c)...)
		return typeRef{doc: ai.Output}, diags
	}
	// Not declared in this compilation unit: a YAML-only sibling or simply
	// undeclared (spec.ValidateProjectGraph reports the latter after lowering).
	// Gradual typing: only check argument well-formedness.
	for _, arg := range c.Args {
		_, d := wc.checkValue(arg.Value)
		diags = append(diags, d...)
	}
	return typeRef{}, diags
}

// checkWorkflowArgs matches call arguments to the callee's declared,
// ORDERED parameters — named args by name, positional args filling the
// remaining declared slots in order — checks each against the parameter's
// resolved type, and validates call-site arity: an unknown named argument, a
// positional argument past the last declared parameter, and a declared
// parameter that ends up bound by nothing are all errors, not silent
// successes. Every positional argument that resolves to a real parameter
// adds one oldKey->newKey entry to a SINGLE rebind for this call site (see
// rebind's doc comment for why they must be grouped rather than applied one
// at a time).
//
// Named and positional arguments may be mixed at a call site (the grammar
// does not forbid it — see Arg's doc comment). A positional argument binds to
// the next parameter slot NOT already claimed by a named argument anywhere in
// the call, not to ParamOrder[i] by raw position: otherwise a named argument
// earlier in the call silently does not "use up" its slot, and a later
// positional argument is checked against the same parameter the named
// argument already bound, producing a false mismatch against an unrelated
// parameter while the true target parameter goes unchecked.
func (wc *wfChecker) checkWorkflowArgs(name string, wi workflowTypeInfo, c *lang.CallExpr) lang.Diagnostics {
	var diags lang.Diagnostics
	renames := map[string]string{}

	// bound tracks every parameter claimed so far — named arguments up front
	// (order-independent: a later positional must skip a name used anywhere in
	// the call, not just earlier in it), then each positional claim as it
	// resolves — so arity can be validated once every argument is processed.
	bound := make(map[string]bool, len(wi.ParamOrder))
	for _, arg := range c.Args {
		if arg.Name != nil {
			bound[arg.Name.Name] = true
		}
	}
	nextIdx := 0
	nextPositionalParam := func() string {
		for nextIdx < len(wi.ParamOrder) {
			p := wi.ParamOrder[nextIdx]
			nextIdx++
			if !bound[p] {
				bound[p] = true
				return p
			}
		}
		return ""
	}

	for i, arg := range c.Args {
		argType, d := wc.checkValue(arg.Value)
		diags = append(diags, d...)

		if arg.Name != nil {
			paramDoc, known := wi.Params[arg.Name.Name]
			if !known {
				diags = append(diags, lang.Diagnostic{
					Pos: arg.Position(),
					Msg: fmt.Sprintf("%s has no parameter %q", name, arg.Name.Name),
				})
				continue
			}
			diags = append(diags, wc.checkCompatible(arg.Position(), argType, typeRef{doc: paramDoc},
				fmt.Sprintf("argument %q of %s", arg.Name.Name, name))...)
			continue
		}

		paramName := nextPositionalParam()
		if paramName == "" {
			diags = append(diags, lang.Diagnostic{
				Pos: arg.Position(),
				Msg: fmt.Sprintf("%s takes %d parameter(s); this is an extra positional argument", name, len(wi.ParamOrder)),
			})
			continue
		}
		diags = append(diags, wc.checkCompatible(arg.Position(), argType, typeRef{doc: wi.Params[paramName]},
			fmt.Sprintf("argument %q of %s", paramName, name))...)

		oldKey := "arg" + strconv.Itoa(i)
		if paramName != oldKey {
			renames[oldKey] = paramName
		}
	}

	for _, p := range wi.ParamOrder {
		if !bound[p] {
			diags = append(diags, lang.Diagnostic{
				Pos: c.Pos,
				Msg: fmt.Sprintf("%s: missing required argument %q", name, p),
			})
		}
	}
	if len(renames) > 0 {
		wc.rebinds = append(wc.rebinds, rebind{pos: c.Callee.Pos, renames: renames})
	}
	return diags
}

// checkAgentArgs checks the unambiguous case — a single positional argument
// standing for the agent's whole declared input — against that type. An
// agent's `input` is one type, not a named parameter list, so every OTHER
// call shape against a known input type is an undefined ABI, not a smaller
// version of the same problem to skip past quietly:
//
//   - zero arguments never supplies a value for a declared input — that is a
//     missing-required-value error, not gradual typing;
//   - a single NAMED argument (`A(input: x)`) is one key instead of the whole
//     document — no less undefined than three positional arguments, so it
//     gets the same treatment;
//   - more than one argument (the ADR 002 normative surface's own
//     `Synthesizer(security, quality, tests)` shape) has no defined
//     field-order binding yet: internal/lang/lower/lower.go placeholder-keys
//     those arguments arg0, arg1, ... with no declared meaning for the
//     receiving agent to bind against.
//
// The latter two stay unchecked — guessing a mapping would produce
// false-positive type errors on well-typed calls — but LOUDLY: a warning
// names the call as unverified rather than passing with no signal that
// nothing was checked. See docs/LANGUAGE.md's "Type checking" section.
func (wc *wfChecker) checkAgentArgs(name string, ai agentTypeInfo, c *lang.CallExpr) lang.Diagnostics {
	var diags lang.Diagnostics
	for _, arg := range c.Args {
		_, d := wc.checkValue(arg.Value)
		diags = append(diags, d...)
	}
	if len(c.Args) == 1 && c.Args[0].Name == nil {
		diags = append(diags, wc.checkValueAgainst(c.Args[0].Value, c.Args[0].Position(), typeRef{doc: ai.Input},
			fmt.Sprintf("input of %s", name))...)
		return diags
	}
	if ai.Input == nil {
		return diags // untyped agent input: nothing to check any shape against
	}
	if len(c.Args) == 0 {
		diags = append(diags, lang.Diagnostic{
			Pos: c.Pos,
			Msg: fmt.Sprintf("%s declares an input type but was called with no arguments", name),
		})
		return diags
	}
	diags = append(diags, lang.Diagnostic{
		Pos: c.Pos,
		Msg: fmt.Sprintf(
			"cannot type-check %d argument(s) to %s: agent-call field binding is only defined for a single positional argument, so %s's declared input type was not checked against this call",
			len(c.Args), name, name),
		Severity: lang.SeverityWarning,
	})
	return diags
}

// checkValueAgainst checks the value expression e against want. An object literal
// is checked field by field against want's property types (recursively): its own
// type is untyped, so checking it as one value would pass anything, while graph
// validation types each interpolated field against the input location it fills
// (#550) — the two must not disagree. A field the declared type forbids
// (additionalProperties: false, or a field of a non-object) is an error, the same
// "not declared" graph validation reports. Anything else is checked as one value.
// Reference well-formedness diagnostics are the caller's (checkValue), not repeated.
// A string-template field is typed by checkTemplate, the rule graph validation applies.
func (wc *wfChecker) checkValueAgainst(e lang.Expr, pos lang.Pos, want typeRef, what string) lang.Diagnostics {
	obj, ok := e.(*lang.ObjectExpr)
	if !ok || want.doc == nil {
		got, _ := wc.checkValue(e)
		return wc.checkCompatible(pos, got, want, what)
	}
	var diags lang.Diagnostics
	for _, f := range obj.Fields {
		if f == nil || f.Key == nil || f.Value == nil {
			continue
		}
		path := append(append([]string(nil), want.path...), f.Key.Name)
		field := fmt.Sprintf("%s field %q", what, strings.Join(path, "."))
		if want.doc.Lookup(path).Missing {
			diags = append(diags, lang.Diagnostic{
				Pos: f.Position(),
				Msg: fmt.Sprintf("%s is not declared by the declared type", field),
			})
			continue
		}
		diags = append(diags, wc.checkValueAgainst(f.Value, f.Value.Position(), typeRef{doc: want.doc, path: path}, field)...)
	}
	return diags
}

func (wc *wfChecker) checkCompatible(pos lang.Pos, got, want typeRef, what string) lang.Diagnostics {
	gotTypes, wantTypes := got.types(), want.types()
	if len(gotTypes) == 0 || len(wantTypes) == 0 {
		return nil // gradual typing: an untyped side is always compatible
	}
	if schema.Compatible(gotTypes, wantTypes) {
		return nil
	}
	return lang.Diagnostics{{
		Pos: pos,
		Msg: fmt.Sprintf("%s: type %s is not compatible with declared type %s", what, gotTypes, wantTypes),
	}}
}
