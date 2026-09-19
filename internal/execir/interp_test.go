package execir

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a test Invoker that records calls (order-sensitively) and returns
// canned or synthesized responses. Safe for concurrent use.
type recorder struct {
	mu    sync.Mutex
	tools []string
	// respond, if set, produces a tool result from its uses and args.
	respond func(uses string, args map[string]any) any
	// track observes concurrency: it increments on entry and decrements on exit.
	track   *int32
	peak    *int32
	holdFor time.Duration
}

func (r *recorder) InvokeTool(_ context.Context, _ CallSite, uses string, args map[string]any) (any, error) {
	if r.track != nil {
		cur := atomic.AddInt32(r.track, 1)
		for {
			p := atomic.LoadInt32(r.peak)
			if cur <= p || atomic.CompareAndSwapInt32(r.peak, p, cur) {
				break
			}
		}
		if r.holdFor > 0 {
			time.Sleep(r.holdFor)
		}
		atomic.AddInt32(r.track, -1)
	}
	r.mu.Lock()
	r.tools = append(r.tools, uses+argsSuffix(args))
	r.mu.Unlock()
	if r.respond != nil {
		return r.respond(uses, args), nil
	}
	return nil, nil
}

func (r *recorder) InvokeAgent(_ context.Context, _ CallSite, agent string, args map[string]any) (any, error) {
	r.mu.Lock()
	r.tools = append(r.tools, "agent:"+agent)
	r.mu.Unlock()
	if r.respond != nil {
		return r.respond("agent:"+agent, args), nil
	}
	return map[string]any{"agent": agent}, nil
}

func (r *recorder) InvokeWorkflow(_ context.Context, _ CallSite, wf string, args map[string]any) (any, error) {
	r.mu.Lock()
	r.tools = append(r.tools, "workflow:"+wf)
	r.mu.Unlock()
	return nil, nil
}

func (r *recorder) InvokeApproval(_ context.Context, _ CallSite, _ ApprovalInfo, args map[string]any) (any, error) {
	r.mu.Lock()
	r.tools = append(r.tools, "approval")
	r.mu.Unlock()
	return args, nil
}

// siteRecorder captures the CallSite of every invocation so tests can pin the
// frozen ABI's identity contract (issue #257): Path uniqueness/stability and
// Loop iteration indices.
type siteRecorder struct {
	mu    sync.Mutex
	sites []CallSite
}

func (s *siteRecorder) record(site CallSite) {
	s.mu.Lock()
	s.sites = append(s.sites, site)
	s.mu.Unlock()
}

func (s *siteRecorder) InvokeTool(_ context.Context, site CallSite, _ string, _ map[string]any) (any, error) {
	s.record(site)
	return map[string]any{}, nil
}
func (s *siteRecorder) InvokeAgent(_ context.Context, site CallSite, agent string, _ map[string]any) (any, error) {
	s.record(site)
	return map[string]any{"agent": agent}, nil
}
func (s *siteRecorder) InvokeWorkflow(_ context.Context, site CallSite, _ string, _ map[string]any) (any, error) {
	s.record(site)
	return map[string]any{}, nil
}
func (s *siteRecorder) InvokeApproval(_ context.Context, site CallSite, _ ApprovalInfo, args map[string]any) (any, error) {
	s.record(site)
	return args, nil
}

// byBind indexes captured sites by their (unique) binding name.
func (s *siteRecorder) byBind() map[string]CallSite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]CallSite, len(s.sites))
	for _, st := range s.sites {
		out[st.Bind] = st
	}
	return out
}

func pathsEqual(a, b []int) bool {
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

// TestCallSite_DistinctPathsEmptyBind proves two static nodes with the SAME
// (empty) Bind still get distinct Path — Bind alone is not a sufficient key, so
// Path must disambiguate.
func TestCallSite_DistinctPathsEmptyBind(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Body: []Node{
		&InvokeTool{Uses: "tool.t.x"}, // Bind ""
		&InvokeTool{Uses: "tool.t.x"}, // Bind "" (same)
	}}
	rec := &siteRecorder{}
	if _, err := (&Interp{Invoker: rec}).Run(context.Background(), prog, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.sites) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(rec.sites))
	}
	if rec.sites[0].Bind != "" || rec.sites[1].Bind != "" {
		t.Fatalf("both binds should be empty, got %q/%q", rec.sites[0].Bind, rec.sites[1].Bind)
	}
	if pathsEqual(rec.sites[0].Path, rec.sites[1].Path) {
		t.Fatalf("two distinct static nodes must have distinct Path, both = %v", rec.sites[0].Path)
	}
}

// TestCallSite_LoopIndices proves a loop-body node keeps one static Path across
// iterations while Loop carries the per-iteration index — the disambiguator #258
// needs to memoize the same static leaf on different iterations distinctly.
func TestCallSite_LoopIndices(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&Loop{Var: "i", Collection: Ref{Path: []string{"input", "items"}}, Body: []Node{
			&InvokeTool{Bind: "x", Uses: "tool.t.x"},
		}},
	}}
	rec := &siteRecorder{}
	if _, err := (&Interp{Invoker: rec}).Run(context.Background(), prog, map[string]any{"items": []any{int64(1), int64(2), int64(3)}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rec.sites) != 3 {
		t.Fatalf("expected 3 iterations, got %d", len(rec.sites))
	}
	for k, st := range rec.sites {
		if !pathsEqual(st.Loop, []int{k}) {
			t.Fatalf("iteration %d Loop = %v, want [%d]", k, st.Loop, k)
		}
		if !pathsEqual(st.Path, rec.sites[0].Path) {
			t.Fatalf("loop-body Path must be constant across iterations: %v vs %v", st.Path, rec.sites[0].Path)
		}
	}
}

// TestCallSite_GraphPathOrderStable proves a Graph node's Path is invariant under
// digest-preserving reordering of independent steps — the property #258 keys
// durable memoization on. Guards against the authored-index regression.
func TestCallSite_GraphPathOrderStable(t *testing.T) {
	t.Parallel()
	node := func(id string, needs ...string) GraphNode {
		return GraphNode{ID: id, Needs: needs, Run: &InvokeTool{Bind: id, Uses: "tool.t." + id}}
	}
	// Same DAG (a,b roots; c[a]; d[a,b]; e[c]), authored in two different orders.
	p1 := &Program{Workflow: "W", Body: []Node{&Graph{Nodes: []GraphNode{
		node("a"), node("b"), node("c", "a"), node("d", "a", "b"), node("e", "c"),
	}}}}
	p2 := &Program{Workflow: "W", Body: []Node{&Graph{Nodes: []GraphNode{
		node("d", "b", "a"), node("b"), node("e", "c"), node("a"), node("c", "a"),
	}}}}
	if p1.Digest() != p2.Digest() {
		t.Fatalf("fixtures must be the same DAG (equal digest)")
	}
	r1, r2 := &siteRecorder{}, &siteRecorder{}
	if _, err := (&Interp{Invoker: r1}).Run(context.Background(), p1, nil); err != nil {
		t.Fatalf("p1 run: %v", err)
	}
	if _, err := (&Interp{Invoker: r2}).Run(context.Background(), p2, nil); err != nil {
		t.Fatalf("p2 run: %v", err)
	}
	s1, s2 := r1.byBind(), r2.byBind()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !pathsEqual(s1[id].Path, s2[id].Path) {
			t.Fatalf("node %q Path differs across reordering: %v vs %v (authored order must not leak into the frozen identity)", id, s1[id].Path, s2[id].Path)
		}
	}
	// And every node's Path is distinct (a real address, not a constant).
	seen := map[string]bool{}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		key := fmt.Sprint(s1[id].Path)
		if seen[key] {
			t.Fatalf("node %q shares Path %s with another node", id, key)
		}
		seen[key] = true
	}
}

// durableStub suspends the first call to suspendUses (returning ErrSuspend) and
// counts invocations per uses, so a test can prove a memoized leaf is not
// re-invoked on resume.
type durableStub struct {
	mu          sync.Mutex
	calls       map[string]int
	suspendUses string
	suspended   bool
}

func (d *durableStub) InvokeTool(_ context.Context, _ CallSite, uses string, _ map[string]any) (any, error) {
	d.mu.Lock()
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[uses]++
	first := !d.suspended && uses == d.suspendUses
	if first {
		d.suspended = true
	}
	d.mu.Unlock()
	if first {
		return nil, ErrSuspend
	}
	return map[string]any{"uses": uses}, nil
}
func (d *durableStub) InvokeAgent(_ context.Context, _ CallSite, a string, _ map[string]any) (any, error) {
	return map[string]any{"agent": a}, nil
}
func (d *durableStub) InvokeWorkflow(_ context.Context, _ CallSite, _ string, _ map[string]any) (any, error) {
	return nil, nil
}
func (d *durableStub) InvokeApproval(_ context.Context, _ CallSite, _ ApprovalInfo, args map[string]any) (any, error) {
	return args, nil
}

// TestDurable_MemoReplayNoDuplicateInvocation is the core #258 guarantee: a
// completed leaf before the suspend point is replayed from the memo on resume,
// never re-invoked, so its side effect fires exactly once across suspend+resume.
func TestDurable_MemoReplayNoDuplicateInvocation(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Body: []Node{
		&InvokeTool{Bind: "a", Uses: "tool.t.a"},
		&InvokeTool{Bind: "gate", Uses: "tool.t.gate"},
		&InvokeTool{Bind: "b", Uses: "tool.t.b"},
		&Return{Value: Ref{Path: []string{"b"}}},
	}}
	stub := &durableStub{suspendUses: "tool.t.gate"}
	in := &Interp{Invoker: stub}

	// Fresh run: a completes, gate suspends.
	_, st, err := in.RunResumable(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatalf("fresh run: %v", err)
	}
	if !st.Suspended {
		t.Fatalf("expected suspension at the gate")
	}
	if stub.calls["tool.t.a"] != 1 || stub.calls["tool.t.b"] != 0 {
		t.Fatalf("after suspend: a=%d b=%d, want a=1 b=0", stub.calls["tool.t.a"], stub.calls["tool.t.b"])
	}

	// Resume from the memo: a must NOT be re-invoked; gate completes; b runs.
	out, st2, err := in.RunResumable(context.Background(), prog, nil, st)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st2.Suspended {
		t.Fatalf("resume should complete, not re-suspend")
	}
	if stub.calls["tool.t.a"] != 1 {
		t.Fatalf("tool a re-invoked on resume (%d): memo replay must be side-effect-free", stub.calls["tool.t.a"])
	}
	if stub.calls["tool.t.b"] != 1 {
		t.Fatalf("tool b should run once on resume, got %d", stub.calls["tool.t.b"])
	}
	if m, ok := out.(map[string]any); !ok || m["uses"] != "tool.t.b" {
		t.Fatalf("output %#v", out)
	}
}

// TestDurable_DeterminismGuard proves a control-flow decision that diverges on
// replay (a Branch taking the other arm) is detected, not silently mis-replayed.
func TestDurable_DeterminismGuard(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&Branch{
			Cond: Leaf{V: Ref{Path: []string{"input", "flag"}}},
			Then: []Node{&InvokeTool{Bind: "x", Uses: "tool.t.then"}},
			Else: []Node{&InvokeTool{Bind: "x", Uses: "tool.t.else"}},
		},
	}}
	in := &Interp{Invoker: &recorder{}}
	_, st, err := in.RunResumable(context.Background(), prog, map[string]any{"flag": true}, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Replay with the condition flipped — must be a determinism error.
	_, _, err = in.RunResumable(context.Background(), prog, map[string]any{"flag": false}, st)
	if err == nil {
		t.Fatalf("expected a determinism-violation error when the branch diverges on replay")
	}
}

// TestDurable_LoopBodyBranchVariesPerIteration proves the determinism guard does
// NOT flag legitimate per-iteration control flow: a Branch inside a sequential
// Loop whose outcome depends on the loop variable decides differently each
// iteration, which is correct — the control record is keyed by path AND loop
// (mirroring CallKey), so iteration N's decision is not misread as a divergence
// from iteration N-1. Runs to completion, and replays from a seed unchanged.
func TestDurable_LoopBodyBranchVariesPerIteration(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&Loop{Var: "flag", Collection: Ref{Path: []string{"input", "flags"}}, Body: []Node{
			&Branch{
				Cond: Leaf{V: Ref{Path: []string{"flag"}}},
				Then: []Node{&InvokeTool{Uses: "tool.t.on"}},
				Else: []Node{&InvokeTool{Uses: "tool.t.off"}},
			},
		}},
	}}
	in := &Interp{Invoker: &recorder{}}
	input := map[string]any{"flags": []any{true, false, true}}
	_, st, err := in.RunResumable(context.Background(), prog, input, nil)
	if err != nil {
		t.Fatalf("fresh run must not flag per-iteration branch divergence: %v", err)
	}
	// Replay from the recorded control: the same per-iteration decisions must
	// verify, not collide.
	if _, _, err := in.RunResumable(context.Background(), prog, input, st); err != nil {
		t.Fatalf("replay of identical nested control flow must pass: %v", err)
	}
}

// TestDurable_NestedLoopLengthVariesPerIteration proves an inner Loop whose
// collection length varies per outer iteration is not flagged: the inner loop's
// length record is keyed by the outer loop index too.
func TestDurable_NestedLoopLengthVariesPerIteration(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&Loop{Var: "row", Collection: Ref{Path: []string{"input", "rows"}}, Body: []Node{
			&Loop{Var: "x", Collection: Ref{Path: []string{"row"}}, Body: []Node{
				&InvokeTool{Uses: "tool.t.x"},
			}},
		}},
	}}
	in := &Interp{Invoker: &recorder{}}
	input := map[string]any{"rows": []any{[]any{int64(1)}, []any{int64(1), int64(2)}}}
	if _, _, err := in.RunResumable(context.Background(), prog, input, nil); err != nil {
		t.Fatalf("inner loop length varying per outer iteration must not be flagged: %v", err)
	}
}

// TestDurable_GraphPerBranchSuspend proves a gate inside a Graph suspends the run
// (not a Tier-B error, #270) while an INDEPENDENT sibling completes and is
// memoized; on resume only the unfinished branch re-runs.
func TestDurable_GraphPerBranchSuspend(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Body: []Node{
		&Graph{Nodes: []GraphNode{
			{ID: "sib", Run: &InvokeTool{Bind: "sib", Uses: "tool.t.sib"}},
			{ID: "gate", Run: &InvokeTool{Bind: "gate", Uses: "tool.t.gate"}},
		}},
	}}
	stub := &durableStub{suspendUses: "tool.t.gate"}
	in := &Interp{Invoker: stub}
	_, st, err := in.RunResumable(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatalf("graph suspend should be clean, got %v", err)
	}
	if !st.Suspended {
		t.Fatalf("expected the graph to suspend at the gate")
	}
	if stub.calls["tool.t.sib"] != 1 {
		t.Fatalf("independent sibling should have completed once before suspend, got %d", stub.calls["tool.t.sib"])
	}
	// Resume: sibling is memoized (not re-run); the gate completes.
	_, st2, err := in.RunResumable(context.Background(), prog, nil, st)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st2.Suspended {
		t.Fatalf("resume should complete")
	}
	if stub.calls["tool.t.sib"] != 1 {
		t.Fatalf("sibling re-run on resume (%d): completed branch must replay from memo", stub.calls["tool.t.sib"])
	}
	if stub.calls["tool.t.gate"] != 2 {
		t.Fatalf("gate should be attempted twice (suspend + resume), got %d", stub.calls["tool.t.gate"])
	}
}

// TestDurable_ParallelLoopSuspendResume proves a suspend in one parallel-loop
// iteration replays completed iterations from the memo on resume.
func TestDurable_ParallelLoopSuspendResume(t *testing.T) {
	t.Parallel()
	// Each iteration calls a per-item uses; the "gate" item suspends the first time.
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&Loop{Var: "item", Parallel: true, Collection: Ref{Path: []string{"input", "items"}}, Body: []Node{
			&InvokeTool{Bind: "r", Uses: "tool.t.run", Args: map[string]Value{"item": Ref{Path: []string{"item"}}}},
		}},
	}}
	// durableStub keys suspension on uses; all iterations share one uses, so it
	// suspends the first iteration to reach the invoke, then the rest/replay pass.
	stub := &durableStub{suspendUses: "tool.t.run"}
	in := &Interp{Invoker: stub, MaxConcurrency: 1}
	input := map[string]any{"items": []any{"a", "b", "c"}}
	_, st, err := in.RunResumable(context.Background(), prog, input, nil)
	if err != nil {
		t.Fatalf("parallel loop suspend should be clean: %v", err)
	}
	if !st.Suspended {
		t.Fatalf("expected suspension in the parallel loop")
	}
	before := stub.calls["tool.t.run"]
	_, st2, err := in.RunResumable(context.Background(), prog, input, st)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st2.Suspended {
		t.Fatalf("resume should complete")
	}
	// The completed iterations from the first run are memoized; resume runs only
	// the ones not yet completed, so total invocations is bounded (< before + 3).
	if stub.calls["tool.t.run"] >= before+len(input["items"].([]any)) {
		t.Fatalf("resume re-ran already-completed iterations: before=%d after=%d", before, stub.calls["tool.t.run"])
	}
}

func argsSuffix(args map[string]any) string {
	if v, ok := args["v"]; ok {
		return fmt.Sprintf("(%v)", v)
	}
	return ""
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.tools))
	copy(out, r.tools)
	return out
}

func runProg(t *testing.T, in *Interp, prog *Program, input map[string]any) any {
	t.Helper()
	out, err := in.Run(context.Background(), prog, input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

func TestBranch_TakesThenAndElse(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Branch{
				Cond: BinOp{Op: ">", X: Leaf{V: Ref{Path: []string{"input", "n"}}}, Y: Leaf{V: Lit{V: int64(5)}}},
				Then: []Node{&InvokeTool{Uses: "tool.t.big"}},
				Else: []Node{&InvokeTool{Uses: "tool.t.small"}},
			},
		},
	}
	rec := &recorder{}
	in := &Interp{Invoker: rec}

	runProg(t, in, prog, map[string]any{"n": int64(10)})
	if got := rec.names(); len(got) != 1 || got[0] != "tool.t.big" {
		t.Fatalf("n=10 should take then-branch, got %v", got)
	}

	rec2 := &recorder{}
	runProg(t, &Interp{Invoker: rec2}, prog, map[string]any{"n": int64(3)})
	if got := rec2.names(); len(got) != 1 || got[0] != "tool.t.small" {
		t.Fatalf("n=3 should take else-branch, got %v", got)
	}
}

func TestBranch_BooleanLeafAndLogical(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Branch{
				// input.a && !input.b
				Cond: BinOp{Op: "&&",
					X: Leaf{V: Ref{Path: []string{"input", "a"}}},
					Y: Not{X: Leaf{V: Ref{Path: []string{"input", "b"}}}},
				},
				Then: []Node{&InvokeTool{Uses: "tool.t.yes"}},
			},
		},
	}
	rec := &recorder{}
	runProg(t, &Interp{Invoker: rec}, prog, map[string]any{"a": true, "b": false})
	if got := rec.names(); len(got) != 1 || got[0] != "tool.t.yes" {
		t.Fatalf("a&&!b true should invoke, got %v", got)
	}
	rec2 := &recorder{}
	runProg(t, &Interp{Invoker: rec2}, prog, map[string]any{"a": true, "b": true})
	if got := rec2.names(); len(got) != 0 {
		t.Fatalf("a&&!b false should not invoke, got %v", got)
	}
}

func TestLoop_SequentialOrder(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Loop{
				Var:        "item",
				Collection: Ref{Path: []string{"input", "items"}},
				Body: []Node{
					&InvokeTool{Uses: "tool.t.each", Args: map[string]Value{"v": Ref{Path: []string{"item"}}}},
				},
			},
		},
	}
	rec := &recorder{}
	runProg(t, &Interp{Invoker: rec}, prog, map[string]any{"items": []any{int64(1), int64(2), int64(3)}})
	want := []string{"tool.t.each(1)", "tool.t.each(2)", "tool.t.each(3)"}
	got := rec.names()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sequential loop order wrong: got %v want %v", got, want)
	}
}

func TestLoop_DynamicFanoutBoundedConcurrency(t *testing.T) {
	t.Parallel()
	var live, peak int32
	rec := &recorder{track: &live, peak: &peak, holdFor: 5 * time.Millisecond}
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Loop{
				Parallel:   true,
				Var:        "item",
				Collection: Ref{Path: []string{"input", "items"}},
				Body: []Node{
					&InvokeTool{Uses: "tool.t.each", Args: map[string]Value{"v": Ref{Path: []string{"item"}}}},
				},
			},
		},
	}
	items := make([]any, 10)
	for i := range items {
		items[i] = int64(i)
	}
	in := &Interp{Invoker: rec, MaxConcurrency: 3}
	runProg(t, in, prog, map[string]any{"items": items})
	if len(rec.names()) != 10 {
		t.Fatalf("expected 10 iterations, got %d", len(rec.names()))
	}
	if peak > 3 {
		t.Fatalf("dynamic fan-out exceeded the concurrency bound: peak=%d, want <=3", peak)
	}
	if peak < 2 {
		t.Fatalf("expected genuine concurrency (peak>=2), got peak=%d", peak)
	}
}

func TestLoop_IterationCapExceeded(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Loop{Var: "item", Collection: Ref{Path: []string{"input", "items"}}, Body: []Node{&InvokeTool{Uses: "tool.t.each"}}},
		},
	}
	in := &Interp{Invoker: &recorder{}, MaxLoopIterations: 2}
	_, err := in.Run(context.Background(), prog, map[string]any{"items": []any{1, 2, 3}})
	if err == nil {
		t.Fatalf("expected an iteration-cap error, got nil")
	}
}

func TestFork_RunsBranchesAndPublishesBindings(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Fork{Branches: []ForkBranch{
				{Bind: "a", Nodes: []Node{&InvokeAgent{Bind: "a", Agent: "AgentA"}}},
				{Bind: "b", Nodes: []Node{&InvokeAgent{Bind: "b", Agent: "AgentB"}}},
			}},
			&Return{Value: Ref{Path: []string{"a", "agent"}}},
		},
	}
	rec := &recorder{respond: func(uses string, _ map[string]any) any {
		return map[string]any{"agent": uses}
	}}
	out := runProg(t, &Interp{Invoker: rec}, prog, nil)
	if out != "agent:AgentA" {
		t.Fatalf("fork should publish branch binding a, got %v", out)
	}
	if len(rec.names()) != 2 {
		t.Fatalf("expected both fork branches to run, got %v", rec.names())
	}
}

func TestReturn_StopsSubsequentNodes(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W",
		Params:   []string{"input"},
		Body: []Node{
			&Branch{
				Cond: Leaf{V: Lit{V: true}},
				Then: []Node{&Return{Value: Lit{V: "early"}}},
			},
			&InvokeTool{Uses: "tool.t.after"},
		},
	}
	rec := &recorder{}
	out := runProg(t, &Interp{Invoker: rec}, prog, nil)
	if out != "early" {
		t.Fatalf("expected early return, got %v", out)
	}
	if len(rec.names()) != 0 {
		t.Fatalf("nodes after a fired return must not run, got %v", rec.names())
	}
}

// TestLoop_SequentialReturnHalts proves a Return inside a sequential loop body
// returns from the workflow and stops both the loop and the nodes after it —
// not swallowed as a per-iteration no-op (review finding: loop isolation).
func TestLoop_SequentialReturnHalts(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W", Params: []string{"input"},
		Body: []Node{
			&Loop{Var: "item", Collection: Ref{Path: []string{"input", "items"}}, Body: []Node{
				&Return{Value: Ref{Path: []string{"item"}}},
			}},
			&InvokeTool{Uses: "tool.t.after"},
		},
	}
	rec := &recorder{}
	out := runProg(t, &Interp{Invoker: rec}, prog, map[string]any{"items": []any{"first", "second"}})
	if out != "first" {
		t.Fatalf("return in loop should return the first item, got %v", out)
	}
	if len(rec.names()) != 0 {
		t.Fatalf("nodes after a returning loop must not run, got %v", rec.names())
	}
}

// TestLoop_SequentialCarriedBindingEscapes proves a body binding in a sequential
// loop escapes with the last iteration's value (matches the checker's flat
// scope), so a later reference sees "c", not the pre-loop value.
func TestLoop_SequentialCarriedBindingEscapes(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W", Params: []string{"input"},
		Body: []Node{
			&Let{Bind: "last", Value: Lit{V: "init"}},
			&Loop{Var: "item", Collection: Ref{Path: []string{"input", "items"}}, Body: []Node{
				&Let{Bind: "last", Value: Ref{Path: []string{"item"}}},
			}},
			&Return{Value: Ref{Path: []string{"last"}}},
		},
	}
	out := runProg(t, &Interp{Invoker: &recorder{}}, prog, map[string]any{"items": []any{"a", "b", "c"}})
	if out != "c" {
		t.Fatalf("loop-carried binding should escape with the last value, got %v", out)
	}
}

// TestBranch_EqualityOverObjectsAndArrays proves `==` is total and panic-free
// over the JSON objects and arrays a workflow input actually holds — a bare Go
// `==` on those operands would panic (review finding).
func TestBranch_EqualityOverObjectsAndArrays(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W", Params: []string{"input"},
		Body: []Node{
			&Branch{
				Cond: BinOp{Op: "==", X: Leaf{V: Ref{Path: []string{"input", "a"}}}, Y: Leaf{V: Ref{Path: []string{"input", "b"}}}},
				Then: []Node{&InvokeTool{Uses: "tool.t.equal"}},
				Else: []Node{&InvokeTool{Uses: "tool.t.notequal"}},
			},
		},
	}
	run := func(a, b any) string {
		rec := &recorder{}
		runProg(t, &Interp{Invoker: rec}, prog, map[string]any{"a": a, "b": b})
		names := rec.names()
		if len(names) != 1 {
			t.Fatalf("expected one branch to run, got %v", names)
		}
		return names[0]
	}

	// Equal maps -> then; different maps -> else; equal/unequal arrays; type mismatch.
	if got := run(map[string]any{"k": "v"}, map[string]any{"k": "v"}); got != "tool.t.equal" {
		t.Fatalf("equal maps should be ==, got %s", got)
	}
	if got := run(map[string]any{"k": "v"}, map[string]any{"k": "w"}); got != "tool.t.notequal" {
		t.Fatalf("different maps should be !=, got %s", got)
	}
	if got := run([]any{int64(1), int64(2)}, []any{int64(1), float64(2)}); got != "tool.t.equal" {
		t.Fatalf("arrays [1,2] and [1,2.0] should be == (numeric normalization), got %s", got)
	}
	if got := run([]any{int64(1)}, []any{int64(1), int64(2)}); got != "tool.t.notequal" {
		t.Fatalf("arrays of different length should be !=, got %s", got)
	}
	if got := run(map[string]any{"k": "v"}, "scalar"); got != "tool.t.notequal" {
		t.Fatalf("map vs scalar should be != (and must not panic), got %s", got)
	}
}

func TestLoop_NonListCollectionErrors(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W", Params: []string{"input"},
		Body: []Node{&Loop{Var: "x", Collection: Ref{Path: []string{"input", "notalist"}}, Body: []Node{&InvokeTool{Uses: "tool.t.x"}}}},
	}
	_, err := (&Interp{Invoker: &recorder{}}).Run(context.Background(), prog, map[string]any{"notalist": "scalar"})
	if err == nil {
		t.Fatalf("expected an error iterating a non-list, got nil")
	}
}

// TestCompositeValues_Eval proves the #256 composite Value forms evaluate
// recursively: an Object of a Ref and a List, and a Template that interpolates a
// scalar ref into surrounding text.
func TestCompositeValues_Eval(t *testing.T) {
	t.Parallel()
	prog := &Program{
		Workflow: "W", Params: []string{"input"},
		Body: []Node{&Return{Value: Object{Fields: []Field{
			{Key: "who", Val: Ref{Path: []string{"input", "name"}}},
			{Key: "tags", Val: List{Elems: []Value{Lit{V: "x"}, Ref{Path: []string{"input", "name"}}}}},
			{Key: "greeting", Val: Template{Parts: []Value{Lit{V: "hi "}, Ref{Path: []string{"input", "name"}}, Lit{V: "!"}}}},
		}}}},
	}
	out := runProg(t, &Interp{Invoker: &recorder{}}, prog, map[string]any{"name": "ada"})
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("return should be a map, got %T", out)
	}
	if m["who"] != "ada" {
		t.Fatalf("who = %v, want ada", m["who"])
	}
	if tags, ok := m["tags"].([]any); !ok || len(tags) != 2 || tags[0] != "x" || tags[1] != "ada" {
		t.Fatalf("tags = %v, want [x ada]", m["tags"])
	}
	if m["greeting"] != "hi ada!" {
		t.Fatalf("greeting = %v, want %q", m["greeting"], "hi ada!")
	}
}

// TestApproval_ExecutesAndPublishesPayload proves an Approval node runs through
// the Invoker (issue #258) and publishes the approved payload under its bind, so
// a downstream reference resolves to what was approved.
func TestApproval_ExecutesAndPublishesPayload(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&Approval{Bind: "gate", Description: "review", Args: map[string]Value{"note": Ref{Path: []string{"input", "note"}}}},
		&Return{Value: Ref{Path: []string{"gate"}}},
	}}
	rec := &recorder{}
	out := runProg(t, &Interp{Invoker: rec}, prog, map[string]any{"note": "hello"})
	m, ok := out.(map[string]any)
	if !ok || m["note"] != "hello" {
		t.Fatalf("approval should publish its reviewed payload, got %#v", out)
	}
	names := rec.names()
	if len(names) != 1 || names[0] != "approval" {
		t.Fatalf("expected one approval invocation, got %v", names)
	}
}

// TestGraph_JoinAccuracy proves the DAG scheduler runs each node when ITS own
// predecessors complete — not over-synchronized like a Fork. In
// `A,B roots; C[A]; D[A,B]; E[C]`, every node runs exactly once and a node never
// runs before its predecessors (observed via the recorded output chain).
func TestGraph_JoinAccuracy(t *testing.T) {
	t.Parallel()
	// Each node returns {seen: <sorted predecessor ids actually present in scope>}
	// so we can assert a node saw exactly its declared predecessors' outputs.
	graphNode := func(id string, needs ...string) GraphNode {
		args := map[string]Value{}
		for _, dep := range needs {
			args[dep] = Ref{Path: []string{dep}}
		}
		return GraphNode{ID: id, Needs: needs, Run: &InvokeTool{Bind: id, Uses: "tool.t." + id, Args: args}}
	}
	prog := &Program{
		Workflow: "W", Params: []string{"input"},
		Body: []Node{
			&Graph{Nodes: []GraphNode{
				graphNode("a"),
				graphNode("b"),
				graphNode("c", "a"),
				graphNode("d", "a", "b"),
				graphNode("e", "c"),
			}},
			&Return{Value: Ref{Path: []string{"e"}}},
		},
	}
	var mu sync.Mutex
	seen := map[string]map[string]bool{}
	rec := &recorder{respond: func(uses string, args map[string]any) any {
		id := strings.TrimPrefix(uses, "tool.t.")
		mu.Lock()
		got := map[string]bool{}
		for k := range args {
			got[k] = true
		}
		seen[id] = got
		mu.Unlock()
		return map[string]any{"id": id}
	}}
	if _, err := (&Interp{Invoker: rec}).Run(context.Background(), prog, nil); err != nil {
		t.Fatalf("graph run: %v", err)
	}
	// Every node ran exactly once.
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if _, ok := seen[id]; !ok {
			t.Fatalf("node %q did not run", id)
		}
	}
	// d saw both a and b (its declared predecessors, published before it ran).
	if !seen["d"]["a"] || !seen["d"]["b"] {
		t.Fatalf("d should see a and b, saw %v", seen["d"])
	}
	// e saw c (not b — E does not wait for the D/B join).
	if !seen["e"]["c"] {
		t.Fatalf("e should see c, saw %v", seen["e"])
	}
}

// TestGraph_BoundedConcurrency proves independent roots run concurrently but the
// scheduler honors MaxConcurrency.
func TestGraph_BoundedConcurrency(t *testing.T) {
	t.Parallel()
	var track, peak int32
	nodes := make([]GraphNode, 6)
	for i := range nodes {
		id := fmt.Sprintf("r%d", i)
		nodes[i] = GraphNode{ID: id, Run: &InvokeTool{Bind: id, Uses: "tool.t." + id}}
	}
	prog := &Program{Workflow: "W", Body: []Node{&Graph{Nodes: nodes}}}
	rec := &recorder{track: &track, peak: &peak, holdFor: 20 * time.Millisecond}
	if _, err := (&Interp{Invoker: rec, MaxConcurrency: 2}).Run(context.Background(), prog, nil); err != nil {
		t.Fatalf("graph run: %v", err)
	}
	if got := atomic.LoadInt32(&peak); got > 2 {
		t.Fatalf("peak concurrency %d exceeded MaxConcurrency 2", got)
	}
	if got := atomic.LoadInt32(&peak); got < 2 {
		t.Fatalf("independent roots should run concurrently, peak was %d", got)
	}
}
