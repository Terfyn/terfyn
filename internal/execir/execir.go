// Package execir defines the execution IR: the derived, never-authored
// projection of a checked .agent program where control flow lives (ADR 002 §5,
// issue #199).
//
// ADR 002 fixes two sibling projections of one checked program. The resource
// projection (internal/spec resources) is what humans author, what `plan`
// diffs, and what `apply` writes; it never acquires an expression language, so
// `Branch` and `Loop` may not appear on a spec.WorkflowStep (ADR 002 §4). The
// execution IR — this package — is the other projection: it holds
// InvokeTool/InvokeAgent/InvokeWorkflow/Fork/Branch/Loop/Return and is where a
// conditional or a loop becomes something after the AST is discarded. It is
// never hand-authored and has no YAML surface.
//
// The two projections are NOT a pipeline: the execution IR is lowered directly
// from the checked program (internal/lang/lower.LowerExec), not derived from the
// resource projection, which by design cannot represent control flow.
//
// Ingress convergence is the DESIGN target, not yet the implementation. ADR 002
// §5 requires both ingress paths to converge on this IR — a straight-line YAML
// workflow lowering to the same flat node list a straight-line `.agent` workflow
// does, so execution semantics never diverge. Both lowerings now exist —
// internal/lang/lower.LowerExec (`.agent`) and internal/lang/lower.
// LowerWorkflowResource (YAML, #256), the latter proven to produce a byte-
// identical Program for a straight-line YAML/`.agent` twin pair. An engine-backed
// [Invoker] now runs a lowered program through [Interp] behind a test-only flag
// at parity with the DAG on completing graphs (#257, internal/engine
// engineInvoker); the DAG remains the production default, and durable
// resume/HITL on the execir path is still a follow-up (#258). Until that lands,
// do not read this package as proof the two paths already share the production
// runtime.
//
// The YAML lowering also introduces two constructs that outrun the `.agent`
// surface, both execution-deferred (the standalone Interp rejects them): [Graph],
// a general needs-DAG (YAML `needs:` is not series-parallel, so it is not [Fork]),
// and [Approval], the fourth XOR step kind (#195).
//
// Runtime independence: nodes reference values by the source binding namespace
// (parameter names, assignment targets, loop variables), not by resource-model
// interpolation tokens such as ${steps.x.output}. An [Interp] executes a Program
// against a [Scope] and an injected [Invoker]; the package deliberately does not
// depend on the engine, state, or model registry, so control-flow semantics are
// unit-testable in isolation (issue #199, library-level scope).
package execir

import "github.com/Terfyn/terfyn/internal/spec"

// Pos aliases spec.Pos so an execution-IR node can carry the same position a
// lang AST node did, with no conversion (mirrors lang.Pos).
type Pos = spec.Pos

// Program is the execution lowering of one workflow: its parameter names, the
// ordered top-level nodes to execute, and a canonical digest that folds into the
// workflow hash (ADR 002 §5; see [Program.Digest] and internal/plan).
type Program struct {
	Workflow string
	Params   []string
	Body     []Node
}

// Node is one execution-IR construct.
type Node interface{ node() }

// InvokeTool is a deterministic workflow-level tool call `uses(args)`. Bind is
// the source binding name the result is stored under, or "" when the call is
// evaluated only for its effect (a bare-expression statement).
type InvokeTool struct {
	Pos  Pos
	Bind string
	Uses string
	Args map[string]Value
}

func (*InvokeTool) node() {}

// InvokeAgent invokes a declared agent. Bind is the result binding, or "".
type InvokeAgent struct {
	Pos   Pos
	Bind  string
	Agent string
	Args  map[string]Value
}

func (*InvokeAgent) node() {}

// InvokeWorkflow invokes a declared subworkflow. Bind is the result binding, or "".
type InvokeWorkflow struct {
	Pos      Pos
	Bind     string
	Workflow string
	Args     map[string]Value
}

func (*InvokeWorkflow) node() {}

// Let binds a name to a value without invoking anything — the lowering of an
// alias assignment `x = y`.
type Let struct {
	Pos   Pos
	Bind  string
	Value Value
}

func (*Let) node() {}

// Branch is a conditional: run Then when Cond evaluates truthy, else Else. Else
// may be empty. Branch lives only in the execution IR (ADR 002 §4); the resource
// projection instead flattens both arms into steps so the effect bound is the
// union over branches.
type Branch struct {
	Pos  Pos
	Cond Expr
	Then []Node
	Else []Node
}

func (*Branch) node() {}

// ForkBranch is one statically-known parallel branch: a name it binds and the
// nodes it runs.
type ForkBranch struct {
	Bind  string
	Nodes []Node
}

// Fork runs statically-known branches concurrently and joins at its end — the
// lowering of `parallel { a = ...; b = ... }` (#192). The join is the implicit
// barrier at the close of the node: every branch completes before the node that
// follows the Fork begins.
type Fork struct {
	Pos      Pos
	Branches []ForkBranch
}

func (*Fork) node() {}

// Loop iterates Body once per element of Collection with Var bound to the
// element. Parallel marks dynamic fan-out — iterations run with bounded
// concurrency (ADR 002 §1: dynamic fan-out is a loop, not a graph field).
//
// Scope rules differ by kind, and match the type checker:
//
//   - A SEQUENTIAL Loop runs its iterations in order on the ENCLOSING scope. The
//     loop variable and any body binding escape the loop (last iteration wins),
//     and a Return in the body returns from the workflow and stops the loop.
//   - A PARALLEL Loop runs each iteration in an ISOLATED child scope, so
//     iterations never race and no body binding escapes; `return` is not lowered
//     into a parallel body (LowerExec rejects it), so isolation loses nothing.
//
// There is no unbounded (`while`) loop in the surface, and the interpreter caps
// the iteration count (internal/spec MaxLoopIterations) so termination is always
// bounded (#199).
type Loop struct {
	Pos        Pos
	Var        string
	Collection Value
	Body       []Node
	Parallel   bool
}

func (*Loop) node() {}

// While is a bounded, condition-driven sequential loop — the lowering of
// `while <cond> limit N { … }` (#288, ADR 002 §6). Before each iteration Cond is
// evaluated over the current scope (pure, like a [Branch] condition); the body
// runs while Cond is truthy, at most Limit times.
//
// The bound is load-bearing, not advisory: there is NO unbounded effectful loop
// in the surface, and the interpreter caps executions at min(Limit,
// MaxLoopIterations) regardless of whether Cond ever becomes false, so an
// adversarial or malformed carried state cannot buy an extra iteration. Limit is
// the per-loop explicit source bound; the global cap is only an additional
// ceiling.
//
// Scope is the SEQUENTIAL [Loop] rule: the body runs on the enclosing scope so a
// preexisting binding rebound in the body carries forward across iterations and
// out of the loop (loop-carried state), while a body-local binding is recreated
// each iteration. A Return in the body returns from the workflow and stops the
// loop. Each iteration i folds i into the leaf CallSite.Loop vector, so an
// effectful leaf in iteration i has a stable identity across a durable resume.
type While struct {
	Pos   Pos
	Cond  Expr
	Limit int
	Body  []Node
}

func (*While) node() {}

// Retry is a bounded RETRY loop (#361): it runs Body up to Limit times, checking Cond (the
// SUCCESS condition) AFTER each attempt, and — unlike While, which exits silently on
// exhaustion — FAILS the run (RetryExhausted) if the attempts are exhausted with Cond still
// false. Cond is pure over already-bound values (same rule as Branch). Each attempt i folds i
// into the leaf CallSite.Loop vector, so an effectful leaf has a stable identity across a
// durable resume, exactly like While/Loop.
type Retry struct {
	Pos   Pos
	Cond  Expr
	Limit int
	Body  []Node
}

func (*Retry) node() {}

// Return sets the workflow output value.
type Return struct {
	Pos   Pos
	Value Value
}

func (*Return) node() {}

// Graph is a general needs-DAG of steps — the concurrency construct the YAML
// lowering owns (issue #256, ADR 002 §5). It is the deliberate resolution of
// "design decision 1": YAML `needs:` is validated as an ARBITRARY DAG, which
// [Fork] (series-parallel, all-branches-join) cannot express without either
// false synchronization or duplicating a node. Concretely, in
//
//	A, B roots;  C needs [A];  D needs [A, B];  E needs [C]
//
// D is runnable once A and B finish (not waiting for C/E), and a per-branch
// suspend (an approval in one branch, #195) must not force a sibling whose own
// needs are already met to wait. A Graph preserves each node's authored
// dependency set so the reviewable resource DAG executes faithfully; `.agent`
// `parallel { }` ([Fork]) is the structured special case, not the general form.
//
// A straight-line (implicit-sequential) YAML workflow does NOT lower to a Graph:
// it lowers to a flat top-level node list, identical to the `.agent`
// straight-line twin (that parity is the differential-test bar). Only a workflow
// that opts into graph mode (any `needs:` key, [spec.WorkflowUsesExplicitNeeds])
// lowers to a Graph.
//
// Execution is Phase 1 (#257), deliberately out of scope here; the interpreter
// rejects a Graph loudly rather than silently serializing it.
type Graph struct {
	Pos   Pos
	Nodes []GraphNode
}

func (*Graph) node() {}

// GraphNode is one node of a [Graph]: its stable identity (the step id, also the
// binding name its result is published under, so a downstream `${steps.<id>...}`
// reference resolves), the predecessor ids that must complete before it runs,
// and the single Invoke*/Approval node it executes.
type GraphNode struct {
	ID    string
	Needs []string
	Run   Node
}

// Approval is a workflow-level human pause — the FOURTH XOR step kind (a step is
// exactly one of uses/agent/workflow/approval, DESIGN_DOC §7.4) and the
// resolution of "design decision 2" (issue #256/#195, ADR 002). It has no
// InvokeX to lower to: it suspends the workflow at this node for a human
// decision that is not a tool call. Bind is the source binding name (the step
// id) the reviewed payload is published under; Description and RedactKeys are
// review presentation only and do not decide whether the node pauses (policy
// still gates tool-call approvals separately).
//
// Args is the reviewed payload (the lowered `with:` of the approval step); the
// approved payload becomes the node's output, so a downstream reference resolves
// to what the human approved. Description and RedactKeys are review presentation
// only and do not decide whether the node pauses (policy still gates tool-call
// approvals separately).
//
// Durable suspend/resume is Phase 2 (#258): a fresh run suspends here for a human
// decision; resume applies the decision and publishes the payload.
type Approval struct {
	Pos         Pos
	Bind        string
	Description string
	RedactKeys  []string
	Args        map[string]Value
}

func (*Approval) node() {}

// --- Values -----------------------------------------------------------------

// Value is a data operand: a [Ref] into the runtime scope or a [Lit].
type Value interface{ value() }

// Ref is a dotted path into the runtime scope, e.g. ["pr","number"] resolving
// scope["pr"] then its "number" field, or ["input","repo"], or a loop variable.
// Path is always non-empty.
type Ref struct {
	Pos  Pos
	Path []string
}

func (Ref) value() {}

// Lit is a literal operand: a string, int64, float64, or bool.
type Lit struct {
	Pos Pos
	V   any
}

func (Lit) value() {}

// Object is a composite operand: an ordered set of named fields, each a Value.
// It is the lowering of a YAML `with:` sub-map or a multi-key `output.value`
// (issue #256) — structure the `.agent` surface never produces (an `.agent`
// argument is a single Ref/Lit), so it appears only on the YAML side. Digest and
// equality are field-order independent: fields are canonicalized by Key.
type Object struct {
	Pos    Pos
	Fields []Field
}

func (Object) value() {}

// Field is one named member of an [Object].
type Field struct {
	Key string
	Val Value
}

// List is a composite operand: an ordered sequence of Values — the lowering of a
// YAML sequence value under a `with:`/`output` key (issue #256). Order IS
// significant (unlike [Object] fields).
type List struct {
	Pos   Pos
	Elems []Value
}

func (List) value() {}

// Template is an interpolated string: a YAML string value that is NOT a whole-
// field `${...}` token but contains one or more embedded tokens mixed with
// literal text, e.g. a `body:` built from prose plus `${steps.x.output.summary}`
// (issue #256). Parts are concatenated as strings at evaluation; each part is a
// literal [Lit] chunk or a [Ref]. A string with no token lowers to a plain
// [Lit]; a whole-field token lowers to a [Ref] — a Template is strictly the
// mixed case, so the `${...}` → Ref mapping still holds for every token.
type Template struct {
	Pos   Pos
	Parts []Value
}

func (Template) value() {}

// --- Condition expressions --------------------------------------------------

// Expr is a boolean condition tree.
type Expr interface{ expr() }

// Leaf wraps a Value (Ref or Lit) as a condition leaf.
type Leaf struct{ V Value }

func (Leaf) expr() {}

// Not is logical negation.
type Not struct{ X Expr }

func (Not) expr() {}

// BinOp is a comparison or logical connective. Op is one of:
// "==", "!=", "<", "<=", ">", ">=", "&&", "||".
type BinOp struct {
	Op   string
	X, Y Expr
}

func (BinOp) expr() {}
