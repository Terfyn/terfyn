package execir

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/Terfyn/terfyn/internal/spec"
)

// DefaultMaxConcurrency bounds goroutine fan-out for a parallel Loop or Fork
// when the caller sets no override. It mirrors the engine's step-concurrency
// default without importing the engine (execir must stay runtime-independent).
const DefaultMaxConcurrency = 8

// ErrSuspend is returned by an [Invoker] to signal that a leaf is now waiting on
// a human decision (a HITL gate or an [Approval] node): the program pauses here
// (issue #258). The engine adapter records the pending anchor (keyed by the
// [CallSite]) as a side effect before returning it; the interpreter halts the
// walk cleanly, leaving [RunState.Suspended] set and the completed-leaf memo
// intact so a later resume replays without re-issuing side effects.
//
// It is a clean pause, not a failure: it propagates up as a signal that
// RunResumable maps to [RunState.Suspended]. Inside a concurrent construct
// (Fork/Graph/parallel Loop) the branch that paused wins the one suspension slot
// and halts, but its siblings are NOT cancelled (issue #275): each runs to its
// own natural pause — a completed sibling is memoized and replays on resume, and
// a sibling that resolves its own gate on resume runs to completion rather than
// being cancelled and forced to re-suspend. Only a genuine branch error cancels
// siblings. The whole run pauses at the join (#270).
var ErrSuspend = errors.New("execir: suspended for human decision")

// ApprovalInfo is the review presentation an [Approval] node carries to the
// [Invoker] (issue #258/#195). It is not policy — it does not decide whether the
// node pauses.
type ApprovalInfo struct {
	Description string
	RedactKeys  []string
}

// CallSite is the structural identity of one leaf invocation, passed to the
// [Invoker] per call (issue #257). It is the key an engine adapter uses for
// trace/persistence identity and the key Phase 2 durability (#258) memoizes a
// completed leaf under — which is why the ABI carries it now, before the
// interface freezes.
//
// It is a per-call VALUE, built fresh as the interpreter walks and never shared
// mutably, so it is goroutine-safe by construction: a parallel Loop, Fork, or
// Graph invokes from several goroutines, each carrying its own CallSite.
//
//   - Bind is the binding name the result is published under — the step id on the
//     YAML path, an assignment target or generated temp on the `.agent` path. It
//     may be empty for an effect-only call, so it is not a sufficient key alone.
//   - Path is the static node address: the child index at each nesting level from
//     the program root, so every static node position is distinct even when Bind
//     is empty or repeats. For a [Graph] node the segment is the node's CANONICAL
//     (id-sorted) rank, not its authored index, so Path is stable under the
//     digest-preserving step reorderings #256 treats as equal — the identity
//     #258 memoizes on must not shift under a semantically-neutral edit.
//   - Loop is the enclosing loop iteration indices (outermost first), so the same
//     static node executed on different iterations has distinct identity. It is
//     empty on the YAML path (no loops) and non-empty only under `.agent` loops.
type CallSite struct {
	Bind string
	Path []int
	Loop []int
}

// Invoker performs the effectful leaf operations. The execution IR carries no
// I/O of its own; an engine adapter supplies one that runs a real tool/agent/
// subworkflow, while tests supply a recording stub. Implementations must be safe
// for concurrent use — a parallel Loop, Fork, or Graph invokes from several
// goroutines. The [CallSite] identifies the invocation (issue #257).
type Invoker interface {
	InvokeTool(ctx context.Context, site CallSite, uses string, args map[string]any) (any, error)
	InvokeAgent(ctx context.Context, site CallSite, agent string, args map[string]any) (any, error)
	InvokeWorkflow(ctx context.Context, site CallSite, workflow string, args map[string]any) (any, error)
	// InvokeApproval runs a workflow-level human pause (issue #195/#258): the
	// reviewed args are the node's payload, and the approved payload is its
	// output. A fresh run returns [ErrSuspend]; a resume applies the decision.
	InvokeApproval(ctx context.Context, site CallSite, info ApprovalInfo, args map[string]any) (any, error)
}

// Interp executes a Program against an Invoker. Zero values pick built-in
// defaults: MaxLoopIterations falls back to spec.DefaultMaxLoopIterations and
// MaxConcurrency to DefaultMaxConcurrency.
type Interp struct {
	Invoker           Invoker
	MaxLoopIterations int
	MaxConcurrency    int
}

func (in *Interp) maxIters() int {
	if in.MaxLoopIterations > 0 {
		return in.MaxLoopIterations
	}
	return spec.DefaultMaxLoopIterations
}

func (in *Interp) maxConc() int {
	if in.MaxConcurrency > 0 {
		return in.MaxConcurrency
	}
	return DefaultMaxConcurrency
}

// RunState carries durable execution progress across a suspend/resume boundary
// (issue #258). It is the execir half of the engine checkpoint: the engine
// serializes it into checkpoint context and seeds it back on resume.
//
//   - Memo maps a leaf's [CallSite] key to its completed output. On resume a
//     memoized leaf is NOT re-invoked — its result is replayed — so an
//     already-completed side effect never fires twice.
//   - Control records each pure control-flow decision (a Branch condition, a Loop
//     collection length) by node address, so replay can assert the control flow
//     did not diverge (a divergence is a loud error, not a silent wrong replay).
//   - Suspended/SuspendKey report that the run paused at the leaf identified by
//     SuspendKey (the pending human decision the engine anchors PendingHitl to).
type RunState struct {
	Memo       map[string]any `json:"memo,omitempty"`
	Control    map[string]int `json:"control,omitempty"`
	Suspended  bool           `json:"-"`
	SuspendKey string         `json:"-"`
}

// Run executes prog with the given workflow input and returns the value set by a
// Return node (nil if the program returns nothing). It discards durable state; a
// suspend (an [Invoker] returning [ErrSuspend]) halts cleanly with a nil-ish
// output. Use [Interp.RunResumable] for the durable path.
func (in *Interp) Run(ctx context.Context, prog *Program, input map[string]any) (any, error) {
	out, _, err := in.RunResumable(ctx, prog, input, nil)
	return out, err
}

// RunResumable executes prog, seeding completed-leaf memo and control records
// from seed (nil for a fresh run), and returns the durable [RunState] — whether
// the run completed or suspended (issue #258).
func (in *Interp) RunResumable(ctx context.Context, prog *Program, input map[string]any, seed *RunState) (any, *RunState, error) {
	if in == nil || in.Invoker == nil {
		return nil, nil, fmt.Errorf("execir: nil interpreter or invoker")
	}
	if prog == nil {
		return nil, nil, fmt.Errorf("execir: nil program")
	}
	sess := &session{memo: map[string]any{}, control: map[string]int{}}
	if seed != nil {
		for k, v := range seed.Memo {
			sess.memo[k] = v
		}
		for k, v := range seed.Control {
			sess.control[k] = v
		}
	}
	scope := paramScope(prog.Params, input)
	r := &runner{in: in, ctx: ctx, sess: sess}
	// A top-level ErrSuspend is a clean pause, not a failure: the run is now
	// waiting on a human decision and RunState reports where (issue #258/#270).
	if err := r.execAll(scope, prog.Body, nil, nil); err != nil && !errors.Is(err, ErrSuspend) {
		return nil, nil, err
	}
	st := &RunState{
		Memo:       sess.memo,
		Control:    sess.control,
		Suspended:  sess.suspended,
		SuspendKey: sess.suspendKey,
	}
	return r.output, st, nil
}

// session is the shared durable state for one RunResumable call: the memo of
// completed leaves and the control-flow record, both accessed from every
// (sub-)runner — including the goroutines a completing Fork/Graph/parallel Loop
// spawns — so it is mutex-guarded.
type session struct {
	mu         sync.Mutex
	memo       map[string]any
	control    map[string]int
	suspended  bool
	suspendKey string
}

func (s *session) getMemo(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.memo[key]
	return v, ok
}

func (s *session) putMemo(key string, v any) {
	s.mu.Lock()
	s.memo[key] = v
	s.mu.Unlock()
}

func (s *session) markSuspended(key string) {
	s.mu.Lock()
	if !s.suspended {
		s.suspended = true
		s.suspendKey = key
	}
	s.mu.Unlock()
}

func (s *session) isSuspended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspended
}

// checkControl records a pure control-flow decision at addr, or, on replay,
// verifies it matches the recorded value — a mismatch means the control flow
// diverged between the original run and the replay (a non-deterministic
// condition or collection), which would make the memo replay unsound.
func (s *session) checkControl(addr string, val int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.control[addr]; ok && prev != val {
		return fmt.Errorf("execir: control flow diverged on replay at %s (was %d, now %d): a non-deterministic condition cannot be durably resumed", addr, prev, val)
	}
	s.control[addr] = val
	return nil
}

// CallKey canonicalizes a CallSite into the memo/anchor key. Path is already
// order-stable (#257), so the key is stable across digest-preserving edits. An
// engine adapter uses it to anchor a suspended leaf's pending state to the same
// key the interpreter memoizes and reports as [RunState.SuspendKey] (issue #258).
func CallKey(site CallSite) string {
	return fmt.Sprintf("%s|%v|%v", site.Bind, site.Path, site.Loop)
}

// controlKey is the determinism-record address for a control node. It folds in
// BOTH the static path AND the enclosing loop-iteration vector — the same lesson
// CallKey applies to leaves: a loop body's nodes share one static path across
// every iteration (#257 addressing), so a data-dependent Branch/Loop legitimately
// decides differently per iteration. Keying on path alone would record every
// iteration under one address and misread iteration N's decision as a divergence
// from iteration N-1. Keyed by path AND loop, the guard compares the same
// static+iteration decision across the original run and its replay — which is
// what "diverged on replay" actually means.
func controlKey(prefix string, path, loop []int) string {
	return prefix + fmt.Sprintf("%v|%v", path, loop)
}

// extend returns a fresh slice base+[x], never aliasing base — a child address
// must not mutate its parent's (several goroutines share the parent).
func extend(base []int, x int) []int {
	out := make([]int, len(base)+1)
	copy(out, base)
	out[len(base)] = x
	return out
}

// paramScope binds workflow parameters into the initial scope, matching the
// resource lowering's convention (internal/lang/lower newEnv): a single
// parameter names the whole workflow input, so `input.repo` (or `pr.repo` for a
// parameter named `pr`) resolves against the entire input document; multiple
// parameters each name one top-level field of the input.
func paramScope(params []string, input map[string]any) map[string]any {
	scope := make(map[string]any, len(params)+1)
	switch {
	case len(params) == 1:
		scope[params[0]] = input
	default:
		for _, p := range params {
			if input != nil {
				scope[p] = input[p]
			}
		}
	}
	return scope
}

type runner struct {
	in     *Interp
	ctx    context.Context
	sess   *session // shared memo/control/suspend state (issue #258)
	output any
	done   bool // a Return has fired OR this walk suspended; stop executing subsequent nodes
}

// isCanceled reports a context cancellation, used to distinguish a sibling
// abandoned by a suspend/failure cancel from a genuine branch error.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classifyConcurrent decides the result of a concurrent construct (Fork/Graph/
// parallel Loop) after its branches join. A suspend anywhere wins (the run pauses,
// #270). Siblings are not cancelled by a suspend (#275): each completed sibling is
// already memoized and a sibling resolving its own gate ran to completion.
// Otherwise the first genuine branch error surfaces; a context cancellation is a
// sibling abandoned by a real failure's cancel, not a fault.
func classifyConcurrent(suspended bool, errs []error) error {
	// A genuine branch error (a failure or a HITL reject) aborts the whole
	// construct and takes precedence over a concurrent suspend. Siblings are no
	// longer cancelled by a suspend (#275), so a non-cancel, non-suspend error in
	// errs is a real fault, not an artifact of a suspend's cancel — pausing a run
	// that is going to fail would strand the failure.
	for _, e := range errs {
		if e != nil && !isCanceled(e) && !errors.Is(e, ErrSuspend) {
			return e
		}
	}
	if suspended {
		return ErrSuspend
	}
	return nil
}

// execAll runs nodes in order against scope, stopping early once a Return fires.
// path is the address of the enclosing node list; child i extends it. loop is the
// enclosing loop-iteration indices.
func (r *runner) execAll(scope map[string]any, nodes []Node, path, loop []int) error {
	for i, n := range nodes {
		if r.done {
			return nil
		}
		if err := r.exec(scope, n, extend(path, i), loop); err != nil {
			return err
		}
	}
	return nil
}

// exec runs one node. path is this node's own address; loop is the enclosing
// loop-iteration indices. Both are folded into the [CallSite] of any leaf
// invocation the node performs.
func (r *runner) exec(scope map[string]any, n Node, path, loop []int) error {
	switch v := n.(type) {
	case *InvokeTool:
		site := CallSite{Bind: v.Bind, Path: path, Loop: loop}
		return r.invoke(scope, v.Bind, site, v.Args, func(a map[string]any) (any, error) {
			return r.in.Invoker.InvokeTool(r.ctx, site, v.Uses, a)
		})
	case *InvokeAgent:
		site := CallSite{Bind: v.Bind, Path: path, Loop: loop}
		return r.invoke(scope, v.Bind, site, v.Args, func(a map[string]any) (any, error) {
			return r.in.Invoker.InvokeAgent(r.ctx, site, v.Agent, a)
		})
	case *InvokeWorkflow:
		site := CallSite{Bind: v.Bind, Path: path, Loop: loop}
		return r.invoke(scope, v.Bind, site, v.Args, func(a map[string]any) (any, error) {
			return r.in.Invoker.InvokeWorkflow(r.ctx, site, v.Workflow, a)
		})
	case *Let:
		val, err := evalValue(scope, v.Value)
		if err != nil {
			return err
		}
		if v.Bind != "" {
			scope[v.Bind] = val
		}
		return nil
	case *Branch:
		return r.execBranch(scope, v, path, loop)
	case *Fork:
		return r.execFork(scope, v, path, loop)
	case *Loop:
		return r.execLoop(scope, v, path, loop)
	case *While:
		return r.execWhile(scope, v, path, loop)
	case *Retry:
		return r.execRetry(scope, v, path, loop)
	case *Graph:
		return r.execGraph(scope, v, path, loop)
	case *Return:
		val, err := evalValue(scope, v.Value)
		if err != nil {
			return err
		}
		r.output = val
		r.done = true
		return nil
	case *Approval:
		site := CallSite{Bind: v.Bind, Path: path, Loop: loop}
		info := ApprovalInfo{Description: v.Description, RedactKeys: v.RedactKeys}
		return r.invoke(scope, v.Bind, site, v.Args, func(a map[string]any) (any, error) {
			return r.in.Invoker.InvokeApproval(r.ctx, site, info, a)
		})
	default:
		return fmt.Errorf("execir: unknown node %T", n)
	}
}

// invoke runs one leaf with durable memoization and suspend handling (issue
// #258). A memoized result is replayed WITHOUT re-invoking (no duplicate side
// effect on resume). An [ErrSuspend] halts the walk cleanly on the sequential
// path and is a loud error inside a concurrent construct (Tier B).
func (r *runner) invoke(scope map[string]any, bind string, site CallSite, args map[string]Value, call func(map[string]any) (any, error)) error {
	key := CallKey(site)
	if v, ok := r.sess.getMemo(key); ok {
		if bind != "" {
			scope[bind] = v
		}
		return nil
	}
	a, err := evalArgs(scope, args)
	if err != nil {
		return err
	}
	res, err := call(a)
	if err != nil {
		if errors.Is(err, ErrSuspend) {
			r.sess.markSuspended(key)
			r.done = true // halt this walk; the run is now waiting on a human
			// Propagate the suspend signal up to the nearest concurrency boundary
			// (Fork/Graph/parallel Loop cancels its siblings) and on to the top,
			// where RunResumable maps it to a clean Suspended state (#270).
			return err
		}
		return err
	}
	r.sess.putMemo(key, res)
	if bind != "" {
		scope[bind] = res
	}
	return nil
}

func (r *runner) execBranch(scope map[string]any, b *Branch, path, loop []int) error {
	cond, err := evalExpr(scope, b.Cond)
	if err != nil {
		return err
	}
	taken := 0
	if cond {
		taken = 1
	}
	if err := r.sess.checkControl(controlKey("br:", path, loop), taken); err != nil {
		return err
	}
	if cond {
		return r.execAll(scope, b.Then, extend(path, 0), loop)
	}
	return r.execAll(scope, b.Else, extend(path, 1), loop)
}

// execFork runs statically-known branches concurrently and joins before
// returning. Each branch executes in its own child scope so branches cannot race
// on shared writes; after the join, each branch's published binding (ForkBranch.
// Bind) is copied into the parent scope. A Return inside a Fork branch is not
// permitted to escape the join — the surface never lowers one there — so branch
// runners do not propagate r.done.
func (r *runner) execFork(scope map[string]any, f *Fork, path, loop []int) error {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	type result struct {
		bind string
		val  any
		ok   bool
	}
	results := make([]result, len(f.Branches))
	errs := make([]error, len(f.Branches))
	sem := make(chan struct{}, r.in.maxConc())
	var wg sync.WaitGroup
	for i, br := range f.Branches {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, br ForkBranch) {
			defer wg.Done()
			defer func() { <-sem }()
			child := childScope(scope)
			sub := &runner{in: r.in, ctx: ctx, sess: r.sess}
			if err := sub.execAll(child, br.Nodes, extend(path, i), loop); err != nil {
				errs[i] = err
				// A genuine failure stops the sibling branches; a suspend does NOT
				// (issue #275). A suspending branch lets its siblings run to their
				// own natural pause — a completed sibling is memoized (S7: replayed,
				// never reissued), and a sibling that resolved its own gate on resume
				// runs to completion instead of being cancelled and forced to
				// re-suspend. The whole fork still pauses at the join.
				if !errors.Is(err, ErrSuspend) {
					cancel()
				}
				return
			}
			val, ok := child[br.Bind]
			results[i] = result{bind: br.Bind, val: val, ok: ok}
		}(i, br)
	}
	wg.Wait()
	if err := classifyConcurrent(r.sess.isSuspended(), errs); err != nil {
		return err
	}
	for _, res := range results {
		if res.bind != "" && res.ok {
			scope[res.bind] = res.val
		}
	}
	return nil
}

func (r *runner) execLoop(scope map[string]any, l *Loop, path, loop []int) error {
	items, err := evalCollection(scope, l.Collection)
	if err != nil {
		return err
	}
	if max := r.in.maxIters(); len(items) > max {
		return fmt.Errorf("execir: loop over %d items exceeds the maximum of %d iterations (raise limits.maxLoopIterations)", len(items), max)
	}
	if err := r.sess.checkControl(controlKey("loop:", path, loop), len(items)); err != nil {
		return err
	}
	if l.Parallel {
		return r.execLoopParallel(scope, l, items, path, loop)
	}
	// A sequential loop shares the enclosing scope and runner: the loop variable
	// and any body binding write to it (last iteration wins and escapes, like the
	// straight-line body the type checker walks), and a Return inside the body
	// fires on this runner — it returns from the workflow and halts the loop,
	// rather than being swallowed as a per-iteration no-op. This is the SAME
	// scope/Return rule the checker uses for sequential control flow; the parallel
	// path below is the sole exception, and `return` is not lowered into it.
	for idx, item := range items {
		scope[l.Var] = item
		if err := r.execAll(scope, l.Body, path, extend(loop, idx)); err != nil {
			return err
		}
		if r.done {
			break
		}
	}
	return nil
}

// execWhile runs a bounded condition-driven loop (#288, ADR 002 §6). It is the
// durable, adversary-resistant counterpart to the surface `while … limit N`:
//
//   - The iteration ceiling is effectiveMax = min(Limit, MaxLoopIterations). The
//     body runs AT MOST that many times even if Cond never becomes false, so a
//     `limit 3` never runs a fourth body on malformed carried state. The per-loop
//     Limit is primary; the global cap is only an additional backstop.
//   - Cond is evaluated before each iteration over the enclosing scope (pure, like
//     a Branch). Each iteration's decision is recorded per (path, iteration) so a
//     durable resume replays the SAME iteration history and a divergent condition
//     is a loud error rather than a silently different run.
//   - Iteration i folds into the loop-index vector (extend(loop, i)), so an
//     effectful leaf in iteration i memoizes under a stable, iteration-specific
//     CallSite and is never reissued across a resume.
//   - The body shares the enclosing scope (loop-carried state; last write wins),
//     and a Return in the body halts the workflow and the loop.
func (r *runner) execWhile(scope map[string]any, w *While, path, loop []int) error {
	max := r.in.maxIters()
	if w.Limit > 0 && w.Limit < max {
		max = w.Limit
	}
	for i := 0; i < max; i++ {
		iter := extend(loop, i)
		cond, err := evalExpr(scope, w.Cond)
		if err != nil {
			return err
		}
		taken := 0
		if cond {
			taken = 1
		}
		if err := r.sess.checkControl(controlKey("while:", path, iter), taken); err != nil {
			return err
		}
		if !cond {
			return nil
		}
		if err := r.execAll(scope, w.Body, path, iter); err != nil {
			return err
		}
		if r.done {
			return nil
		}
	}
	return nil
}

// RetryExhaustedError terminates a run when a `retry until` loop exhausts its attempts with the
// success condition still false (#361). It is the explicit, deterministic failure that `while`
// lacks: rather than falling through when the bound is reached, the run ends as not-successful.
type RetryExhaustedError struct {
	Limit int
}

func (e *RetryExhaustedError) Error() string {
	return fmt.Sprintf("retry until: exhausted %d attempts with the condition still false", e.Limit)
}

// execRetry runs a bounded retry-until loop (#361). The body runs up to Limit times; the SUCCESS
// condition is checked AFTER each attempt (so the body always runs at least once), and the loop
// exits as soon as it becomes true. Exhausting the attempts with the condition still false is a
// terminal RetryExhaustedError — the visible failure a bounded agent-retry loop wants, versus
// while's silent success on exhaustion. Each attempt's control decision is recorded so a durable
// resume replays the same number of attempts (S7).
func (r *runner) execRetry(scope map[string]any, rt *Retry, path, loop []int) error {
	max := r.in.maxIters()
	if rt.Limit > 0 && rt.Limit < max {
		max = rt.Limit
	}
	for i := 0; i < max; i++ {
		iter := extend(loop, i)
		if err := r.execAll(scope, rt.Body, path, iter); err != nil {
			return err
		}
		if r.done {
			return nil
		}
		cond, err := evalExpr(scope, rt.Cond)
		if err != nil {
			return err
		}
		taken := 0
		if cond {
			taken = 1
		}
		if err := r.sess.checkControl(controlKey("retry:", path, iter), taken); err != nil {
			return err
		}
		if cond {
			return nil
		}
	}
	return &RetryExhaustedError{Limit: rt.Limit}
}

// execLoopParallel is dynamic fan-out: one iteration per collection element,
// running with bounded concurrency. Each iteration has an isolated child scope,
// so iterations never race, and a body binding does not escape the loop.
func (r *runner) execLoopParallel(scope map[string]any, l *Loop, items []any, path, loop []int) error {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	errs := make([]error, len(items))
	sem := make(chan struct{}, r.in.maxConc())
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item any) {
			defer wg.Done()
			defer func() { <-sem }()
			child := childScope(scope)
			child[l.Var] = item
			sub := &runner{in: r.in, ctx: ctx, sess: r.sess}
			if err := sub.execAll(child, l.Body, path, extend(loop, i)); err != nil {
				errs[i] = err
				// A genuine failure stops the other iterations; a suspend does NOT
				// (issue #275), so the iterations not waiting on a human run to
				// completion and are memoized (S7) rather than cancelled and re-run.
				if !errors.Is(err, ErrSuspend) {
					cancel()
				}
			}
		}(i, item)
	}
	wg.Wait()
	return classifyConcurrent(r.sess.isSuspended(), errs)
}

// execGraph runs a general needs-DAG (issue #256/#257): each node runs as soon
// as ITS predecessors complete, with bounded concurrency, publishing its result
// into scope under its id. This is the join-accurate counterpart to Fork's
// all-branches barrier — in `A,B roots; C[A]; D[A,B]; E[C]`, D runs when A and B
// finish, not when C/E do — mirroring the engine DAG's depsReady scheduling
// (internal/engine/execution_dag.go) minus checkpoint/HITL. A Return does not
// occur inside a Graph node (its nodes are Invoke*/Approval), so r.done is not
// propagated from here.
func (r *runner) execGraph(scope map[string]any, g *Graph, path, loop []int) error {
	n := len(g.Nodes)
	if n == 0 {
		return nil
	}
	var mu sync.Mutex // guards scope writes, completed, running, firstErr, suspended
	completed := make(map[string]struct{}, n)
	running := make(map[int]struct{}, n)
	var firstErr error
	var suspended bool
	sem := make(chan struct{}, r.in.maxConc())
	completion := make(chan struct{}, n)
	var wg sync.WaitGroup
	// On the first failure OR suspend, cancel in-flight siblings so the graph stops
	// promptly — matching the engine DAG, which cancels the run context on a step
	// error / HITL interrupt rather than letting siblings run to completion. A
	// completed sibling is already memoized (replayed on resume); a cancelled one
	// re-runs (#270).
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()

	// rank maps a node's authored index to its CANONICAL (id-sorted) rank, used as
	// the node's CallSite.Path segment. Authored order in a Graph is semantically
	// neutral — reordering independent steps is a digest-preserving edit
	// (#256 encodeGraph sorts by id) — so keying the address on authored index
	// would make the frozen identity disagree with the digest and shift under a
	// no-op reorder. Ranking by id (tie-broken by index for determinism if two
	// ids ever coincide) makes Path stable under exactly the reorderings the
	// digest calls equal, which is the property #258 memoization needs.
	rank := canonicalRanks(g.Nodes)

	ready := func(gn GraphNode) bool {
		for _, dep := range gn.Needs {
			if _, ok := completed[dep]; !ok {
				return false
			}
		}
		return true
	}

	// trySchedule launches every currently-runnable node. Caller holds mu.
	trySchedule := func() {
		if firstErr != nil || suspended {
			return
		}
		for i, gn := range g.Nodes {
			if _, done := completed[gn.ID]; done {
				continue
			}
			if _, run := running[i]; run {
				continue
			}
			if !ready(gn) {
				continue
			}
			running[i] = struct{}{}
			wg.Add(1)
			go func(i int, gn GraphNode) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// Snapshot the scope so arg evaluation (which reads predecessor
				// outputs, all already published under mu) never races a
				// concurrent sibling's publish.
				mu.Lock()
				local := childScope(scope)
				mu.Unlock()
				sub := &runner{in: r.in, ctx: ctx, sess: r.sess}
				err := sub.exec(local, gn.Run, extend(path, rank[i]), loop)
				mu.Lock()
				switch {
				case err == nil:
					if gn.ID != "" {
						if v, ok := local[gn.ID]; ok {
							scope[gn.ID] = v
						}
					}
					completed[gn.ID] = struct{}{}
				case errors.Is(err, ErrSuspend):
					// This node paused for a human. Stop scheduling NEW nodes, but do
					// NOT cancel the in-flight siblings (issue #275): an independent
					// sibling completes deterministically (memoized, S7), and a sibling
					// that resolved its own gate on resume runs to completion instead
					// of being cancelled and forced to re-suspend. This node is NOT
					// marked completed, so on resume it re-runs and its gate resolves.
					suspended = true
				case isCanceled(err) && (suspended || firstErr != nil):
					// A sibling abandoned by the cancel above — re-runs on resume.
				default:
					if firstErr == nil {
						firstErr = err
						cancel()
					}
				}
				delete(running, i)
				mu.Unlock()
				completion <- struct{}{}
			}(i, gn)
		}
	}

	mu.Lock()
	trySchedule()
	for {
		allDone := len(completed) == n
		stuck := firstErr == nil && !suspended && !allDone && len(running) == 0
		if firstErr != nil || suspended || allDone || stuck {
			mu.Unlock()
			break
		}
		mu.Unlock()
		<-completion
		mu.Lock()
		trySchedule()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// A genuine node error wins over a concurrent suspend (#275): siblings are no
	// longer cancelled by a suspend, so firstErr is a real fault that must abort
	// the graph rather than be masked by the pause.
	if firstErr != nil {
		return firstErr
	}
	if suspended {
		return ErrSuspend
	}
	if len(completed) != n {
		return fmt.Errorf("execir: graph has no runnable step (unsatisfiable needs or cycle)")
	}
	return nil
}

// canonicalRanks returns, for each node's authored index, its position when the
// nodes are ordered by id (ties broken by authored index). Two Graphs that
// denote the same DAG with independent steps reordered therefore assign every id
// the same rank — so a node's CallSite.Path is invariant under the
// digest-preserving reorderings #256 canonicalizes away.
func canonicalRanks(nodes []GraphNode) []int {
	order := make([]int, len(nodes))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		if nodes[order[a]].ID != nodes[order[b]].ID {
			return nodes[order[a]].ID < nodes[order[b]].ID
		}
		return order[a] < order[b]
	})
	rank := make([]int, len(nodes))
	for pos, idx := range order {
		rank[idx] = pos
	}
	return rank
}

// childScope shallow-copies a scope so a nested block (loop iteration, fork
// branch) can bind names without mutating or racing the parent.
func childScope(parent map[string]any) map[string]any {
	child := make(map[string]any, len(parent)+1)
	for k, v := range parent {
		child[k] = v
	}
	return child
}
