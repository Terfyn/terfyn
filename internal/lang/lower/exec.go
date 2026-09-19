package lower

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// LowerExec lowers one workflow declaration into its execution IR (ADR 002 §5,
// issue #199): the sibling projection where Branch/Loop/Fork live. It is lowered
// directly from the AST, never derived from the resource projection (which by
// design cannot represent control flow).
//
// workflows names every single-identifier callee that is a workflow (in-file or
// project-level), so a bare call classifies as InvokeWorkflow rather than
// InvokeAgent — the same classification the resource lowering makes via
// classifyDecls. Diagnostics are lowering-time problems; a diagnostic-free
// Program is a valid executable projection.
//
// References use the SOURCE binding namespace (parameter names, assignment
// targets, loop variables), not resource-model interpolation tokens, so the
// execution IR is independent of how the resource projection renders ${steps.x}.
func LowerExec(wd *lang.WorkflowDecl, workflows map[string]bool) (*execir.Program, lang.Diagnostics) {
	el := &execLowerer{workflows: workflows, used: map[string]struct{}{}}
	if wd == nil {
		return &execir.Program{}, nil
	}
	prog := &execir.Program{Workflow: identName(wd.Name)}
	for _, p := range wd.Params {
		if n := identName(p.Name); n != "" {
			prog.Params = append(prog.Params, n)
			el.used[n] = struct{}{}
		}
	}
	el.reserve(wd.Body)
	prog.Body = el.lowerStmts(wd.Body)
	return prog, el.diags
}

type execLowerer struct {
	workflows map[string]bool
	diags     lang.Diagnostics
	used      map[string]struct{} // reserved binding names, so temps never collide
	tempN     int
	// parallelDepth > 0 while lowering the body of a `parallel for` (or a nested
	// loop inside one). A parallel body runs in an isolated per-iteration scope
	// with no defined join target for a value, so `return` inside it is rejected
	// here — the interpreter accordingly keeps that isolation without having to
	// decide which racing iteration's return wins.
	parallelDepth int
}

func (el *execLowerer) diag(p spec.Pos, format string, args ...any) {
	el.diags = append(el.diags, lang.Diagnostic{Pos: p, Msg: fmt.Sprintf(format, args...)})
}

// reserve records every binding name that appears anywhere in the body
// (including nested branch/loop/fork bindings and loop variables), so a
// generated temporary for a hoisted nested call never shadows a real name.
func (el *execLowerer) reserve(stmts []lang.Stmt) {
	for _, st := range stmts {
		switch s := st.(type) {
		case *lang.AssignStmt:
			el.reserveName(s.Target)
		case *lang.ParallelStmt:
			for _, b := range s.Body {
				el.reserveName(b.Target)
			}
		case *lang.IfStmt:
			el.reserve(s.Then)
			el.reserve(s.Else)
		case *lang.ForStmt:
			el.reserveName(s.Var)
			el.reserve(s.Body)
		case *lang.WhileStmt:
			el.reserve(s.Body)
		case *lang.RetryStmt:
			el.reserve(s.Body)
		}
	}
}

func (el *execLowerer) reserveName(id *lang.Ident) {
	if id != nil && id.Name != "" {
		el.used[id.Name] = struct{}{}
	}
}

func (el *execLowerer) freshTemp() string {
	for {
		name := "_t" + strconv.Itoa(el.tempN)
		el.tempN++
		if _, ok := el.used[name]; !ok {
			el.used[name] = struct{}{}
			return name
		}
	}
}

func (el *execLowerer) lowerStmts(stmts []lang.Stmt) []execir.Node {
	var out []execir.Node
	for _, st := range stmts {
		out = append(out, el.lowerStmt(st)...)
	}
	return out
}

// lowerStmt lowers one statement into the nodes it produces. A statement whose
// arguments contain nested calls emits those hoisted invocations first (pre),
// then the statement's own node, so evaluation order matches source order.
func (el *execLowerer) lowerStmt(st lang.Stmt) []execir.Node {
	var pre []execir.Node
	switch s := st.(type) {
	case *lang.AssignStmt:
		return el.lowerAssign(identName(s.Target), s.Value, s.Pos)
	case *lang.ExprStmt:
		call, ok := s.X.(*lang.CallExpr)
		if !ok {
			el.diag(s.Pos, "expression statement is not a call and has no effect")
			return nil
		}
		node := el.lowerCallNode("", call, &pre)
		return append(pre, node)
	case *lang.ParallelStmt:
		return []execir.Node{el.lowerFork(s)}
	case *lang.IfStmt:
		branch := &execir.Branch{Pos: s.Pos, Cond: el.lowerCond(s.Cond)}
		branch.Then = el.lowerStmts(s.Then)
		branch.Else = el.lowerStmts(s.Else)
		return []execir.Node{branch}
	case *lang.ForStmt:
		loop := &execir.Loop{Pos: s.Pos, Var: identName(s.Var), Parallel: s.Parallel}
		loop.Collection = el.lowerValue(s.In, &pre)
		if s.Parallel {
			el.parallelDepth++
		}
		loop.Body = el.lowerStmts(s.Body)
		if s.Parallel {
			el.parallelDepth--
		}
		return append(pre, loop)
	case *lang.WhileStmt:
		// A bounded while lowers to execir.While: a condition (pure over already-
		// bound values, same rule as a Branch), the explicit source Limit, and the
		// body. The interpreter enforces the bound (ADR 002 §6); the resource
		// projection flattens the body separately for effect analysis.
		return []execir.Node{&execir.While{
			Pos:   s.Pos,
			Cond:  el.lowerCond(s.Cond),
			Limit: s.Limit,
			Body:  el.lowerStmts(s.Body),
		}}
	case *lang.RetryStmt:
		// A bounded retry-until lowers to execir.Retry: the success condition (pure over
		// already-bound values, same rule as a Branch), the explicit source Limit, and the
		// body. The interpreter runs the body up to Limit times, checks the condition after
		// each attempt, and fails the run on exhaustion (#361).
		return []execir.Node{&execir.Retry{
			Pos:   s.Pos,
			Cond:  el.lowerCond(s.Cond),
			Limit: s.Limit,
			Body:  el.lowerStmts(s.Body),
		}}
	case *lang.ReturnStmt:
		if el.parallelDepth > 0 {
			el.diag(s.Pos, "return is not allowed inside a parallel loop body; a parallel iteration has no join target for a return value")
			return nil
		}
		val := el.lowerValue(s.Value, &pre)
		return append(pre, &execir.Return{Pos: s.Pos, Value: val})
	case *lang.ApprovalStmt:
		node := &execir.Approval{Pos: s.Pos, Bind: identName(s.Bind), Args: el.lowerArgs(s.With, &pre)}
		if s.Description != nil {
			node.Description = s.Description.Value
		}
		for _, k := range s.RedactKeys {
			if k != nil {
				node.RedactKeys = append(node.RedactKeys, k.Value)
			}
		}
		return append(pre, node)
	}
	return nil
}

func (el *execLowerer) lowerAssign(bind string, value lang.Expr, pos spec.Pos) []execir.Node {
	var pre []execir.Node
	switch v := value.(type) {
	case *lang.CallExpr:
		node := el.lowerCallNode(bind, v, &pre)
		return append(pre, node)
	case *lang.RefExpr:
		return []execir.Node{&execir.Let{Pos: pos, Bind: bind, Value: el.lowerRef(v)}}
	case *lang.LitExpr:
		return []execir.Node{&execir.Let{Pos: pos, Bind: bind, Value: execir.Lit{Pos: v.Pos, V: v.Value}}}
	default:
		el.diag(pos, "unsupported binding value")
		return nil
	}
}

func (el *execLowerer) lowerFork(s *lang.ParallelStmt) *execir.Fork {
	fork := &execir.Fork{Pos: s.Pos}
	for _, b := range s.Body {
		bind := identName(b.Target)
		nodes := el.lowerAssign(bind, b.Value, b.Pos)
		fork.Branches = append(fork.Branches, execir.ForkBranch{Bind: bind, Nodes: nodes})
	}
	return fork
}

// lowerCallNode lowers a call to an Invoke node, hoisting any nested-call
// arguments into pre first. bind is the result binding, or "" for effect-only.
func (el *execLowerer) lowerCallNode(bind string, c *lang.CallExpr, pre *[]execir.Node) execir.Node {
	args := el.lowerArgs(c.Args, pre)
	callee := c.Callee
	if callee == nil || len(callee.Parts) == 0 {
		el.diag(c.Pos, "call has no callee")
		return &execir.InvokeAgent{Pos: c.Pos, Bind: bind}
	}
	if len(callee.Parts) >= 2 {
		return &execir.InvokeTool{Pos: c.Pos, Bind: bind, Uses: usesFromCallee(callee), Args: args}
	}
	name := callee.Parts[0].Name
	if el.workflows[name] {
		return &execir.InvokeWorkflow{Pos: c.Pos, Bind: bind, Workflow: name, Args: args}
	}
	return &execir.InvokeAgent{Pos: c.Pos, Bind: bind, Agent: name, Args: args}
}

func (el *execLowerer) lowerArgs(args []*lang.Arg, pre *[]execir.Node) map[string]execir.Value {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]execir.Value, len(args))
	for i, arg := range args {
		key := "arg" + strconv.Itoa(i)
		if arg.Name != nil && arg.Name.Name != "" {
			key = arg.Name.Name
		}
		out[key] = el.lowerValue(arg.Value, pre)
	}
	return out
}

// lowerValue lowers a value-position expression. A nested call is hoisted into a
// fresh temporary Invoke appended to pre and referenced by that temp name.
func (el *execLowerer) lowerValue(e lang.Expr, pre *[]execir.Node) execir.Value {
	switch v := e.(type) {
	case *lang.RefExpr:
		return el.lowerRef(v)
	case *lang.LitExpr:
		if s, ok := v.Value.(string); ok {
			if tv, isTemplate := stringTemplateValue(s, v.Pos); isTemplate {
				return tv
			}
		}
		return execir.Lit{Pos: v.Pos, V: v.Value}
	case *lang.CallExpr:
		temp := el.freshTemp()
		node := el.lowerCallNode(temp, v, pre)
		*pre = append(*pre, node)
		return execir.Ref{Pos: v.Pos, Path: []string{temp}}
	case *lang.ObjectExpr:
		fields := make([]execir.Field, 0, len(v.Fields))
		for _, f := range v.Fields {
			if f == nil || f.Key == nil {
				continue
			}
			fields = append(fields, execir.Field{Key: f.Key.Name, Val: el.lowerValue(f.Value, pre)})
		}
		return execir.Object{Pos: v.Pos, Fields: fields}
	default:
		if e != nil {
			el.diag(e.Position(), "unsupported expression")
		}
		return execir.Lit{V: nil}
	}
}

func (el *execLowerer) lowerRef(r *lang.RefExpr) execir.Ref {
	path := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		path = append(path, p.Name)
	}
	return execir.Ref{Pos: r.Pos, Path: path}
}

// lowerCond lowers a boolean condition. Calls are NOT permitted inside a
// condition (a diagnostic): conditions must be pure over already-bound values.
// That keeps effect analysis simple (the effect bound is the union of the steps
// each arm reaches, computed on the resource projection) and avoids the question
// of whether a short-circuited call still runs.
func (el *execLowerer) lowerCond(e lang.Expr) execir.Expr {
	switch v := e.(type) {
	case *lang.BinaryExpr:
		op := binOpString(v.Op)
		if v.Op == lang.KindAndAnd || v.Op == lang.KindOrOr {
			return execir.BinOp{Op: op, X: el.lowerCond(v.X), Y: el.lowerCond(v.Y)}
		}
		return execir.BinOp{Op: op, X: execir.Leaf{V: el.lowerCondLeaf(v.X)}, Y: execir.Leaf{V: el.lowerCondLeaf(v.Y)}}
	case *lang.UnaryExpr:
		return execir.Not{X: el.lowerCond(v.X)}
	case *lang.RefExpr:
		return execir.Leaf{V: el.lowerRef(v)}
	case *lang.LitExpr:
		return execir.Leaf{V: execir.Lit{Pos: v.Pos, V: v.Value}}
	case *lang.CallExpr:
		el.diag(v.Pos, "a call is not allowed in a condition; bind its result to a name and test that name")
		return execir.Leaf{V: execir.Lit{V: false}}
	default:
		if e != nil {
			el.diag(e.Position(), "unsupported condition")
		}
		return execir.Leaf{V: execir.Lit{V: false}}
	}
}

func (el *execLowerer) lowerCondLeaf(e lang.Expr) execir.Value {
	switch v := e.(type) {
	case *lang.RefExpr:
		return el.lowerRef(v)
	case *lang.LitExpr:
		return execir.Lit{Pos: v.Pos, V: v.Value}
	case *lang.CallExpr:
		el.diag(v.Pos, "a call is not allowed in a condition; bind its result to a name and test that name")
		return execir.Lit{V: nil}
	default:
		el.diag(e.Position(), "comparison operand must be a reference or literal")
		return execir.Lit{V: nil}
	}
}

// stringTemplateValue lowers a string ARGUMENT value that embeds `${<binding>}`
// interpolation into an execir value (#316): a whole-field `${x}` becomes a Ref, a
// string mixing prose and tokens becomes a Template (literal chunks + Refs). Each
// token's inner is a `.agent` binding path used directly (the execution IR references
// values by source binding name), so `${review.summary}` -> Ref{[review, summary]}.
// A string with no token returns (nil, false) so the caller keeps it a plain Lit;
// `instructions`/`description` never reach here (they are StringLit, not LitExpr).
func stringTemplateValue(s string, pos spec.Pos) (execir.Value, bool) {
	locs := interpTokenRE.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil, false
	}
	// A single token spanning the whole string is the referent's own value.
	if len(locs) == 1 && locs[0][0] == 0 && locs[0][1] == len(s) {
		return execir.Ref{Pos: pos, Path: interpPath(s[locs[0][2]:locs[0][3]])}, true
	}
	var parts []execir.Value
	last := 0
	for _, loc := range locs {
		if loc[0] > last {
			parts = append(parts, execir.Lit{Pos: pos, V: s[last:loc[0]]})
		}
		parts = append(parts, execir.Ref{Pos: pos, Path: interpPath(s[loc[2]:loc[3]])})
		last = loc[1]
	}
	if last < len(s) {
		parts = append(parts, execir.Lit{Pos: pos, V: s[last:]})
	}
	return execir.Template{Pos: pos, Parts: parts}, true
}

// interpPath splits a `${...}` inner into a dotted binding path.
func interpPath(inner string) []string {
	inner = strings.TrimSpace(inner)
	out := make([]string, 0)
	for _, p := range strings.Split(inner, ".") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func usesFromCallee(callee *lang.RefExpr) string {
	tool := callee.Parts[0].Name
	ops := make([]string, 0, len(callee.Parts)-1)
	for _, p := range callee.Parts[1:] {
		ops = append(ops, p.Name)
	}
	return "tool." + tool + "." + strings.Join(ops, ".")
}

func binOpString(k lang.Kind) string {
	switch k {
	case lang.KindEqEq:
		return "=="
	case lang.KindBangEq:
		return "!="
	case lang.KindLt:
		return "<"
	case lang.KindLte:
		return "<="
	case lang.KindGt:
		return ">"
	case lang.KindGte:
		return ">="
	case lang.KindAndAnd:
		return "&&"
	case lang.KindOrOr:
		return "||"
	default:
		return "?"
	}
}
