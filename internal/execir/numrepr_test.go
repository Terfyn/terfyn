package execir

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/Terfyn/terfyn/internal/jsonnum"
)

// echoStub echoes a tool's args back as its result (so the leaf output carries the operands into
// the memo) and suspends the first call to suspendUses.
type echoStub struct {
	suspendUses string
	suspended   bool
	calls       map[string]int
}

func (e *echoStub) InvokeTool(_ context.Context, _ CallSite, uses string, args map[string]any) (any, error) {
	if e.calls == nil {
		e.calls = map[string]int{}
	}
	e.calls[uses]++
	if !e.suspended && uses == e.suspendUses {
		e.suspended = true
		return nil, ErrSuspend
	}
	return map[string]any{"echo": args}, nil
}
func (e *echoStub) InvokeAgent(context.Context, CallSite, string, map[string]any) (any, error) {
	return nil, nil
}
func (e *echoStub) InvokeWorkflow(context.Context, CallSite, string, map[string]any) (any, error) {
	return nil, nil
}
func (e *echoStub) InvokeApproval(_ context.Context, _ CallSite, _ ApprovalInfo, args map[string]any) (any, error) {
	return args, nil
}

func ref(p ...string) Ref { return Ref{Path: p} }

// bigIntProgram compares the input pair before the gate and the memoized pair after it.
func bigIntProgram() *Program {
	eq := func(x, y Value) Expr { return BinOp{Op: "==", X: Leaf{V: x}, Y: Leaf{V: y}} }
	return &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&InvokeTool{Bind: "seen", Uses: "tool.t.echo", Args: map[string]Value{"a": ref("input", "a"), "b": ref("input", "b")}},
		&Branch{Cond: eq(ref("input", "a"), ref("input", "b")), Then: []Node{&Return{Value: Lit{V: "equal-input"}}}},
		&InvokeTool{Bind: "gate", Uses: "tool.t.gate"},
		&Branch{Cond: eq(ref("seen", "echo", "a"), ref("seen", "echo", "b")), Then: []Node{&Return{Value: Lit{V: "equal-memo"}}}},
		&Return{Value: Lit{V: "distinct"}},
	}}
}

// TestSerializedInputAndRunStateKeepIntegersDistinct drives the interpreter with SERIALIZED JSON
// (input, then the persisted RunState as a checkpoint would carry it) rather than Go int64
// literals, so it proves the decode -> compare -> encode -> decode -> compare chain, not just the
// comparator agreeing with itself.
func TestSerializedInputAndRunStateKeepIntegersDistinct(t *testing.T) {
	t.Parallel()
	var input map[string]any
	if err := jsonnum.Unmarshal([]byte(`{"a":9007199254740992,"b":9007199254740993}`), &input); err != nil {
		t.Fatal(err)
	}
	stub := &echoStub{suspendUses: "tool.t.gate"}
	in := &Interp{Invoker: stub}
	prog := bigIntProgram()

	out, st, err := in.RunResumable(context.Background(), prog, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Suspended || out != nil {
		t.Fatalf("a != b must fall through to the gate and suspend; out=%v suspended=%v", out, st.Suspended)
	}

	// Serialize the durable state exactly as the engine checkpoints it, and decode it back.
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var restored RunState
	if err := jsonnum.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	echo := restored.Memo[firstKey(restored.Memo)].(map[string]any)["echo"].(map[string]any)
	if echo["a"] != int64(9007199254740992) || echo["b"] != int64(9007199254740993) {
		t.Fatalf("memo integers changed across encode/decode: %#v (%s)", echo, raw)
	}

	// Resume from the restored state: same branch as an uninterrupted run, no re-invocation.
	out, st2, err := in.RunResumable(context.Background(), prog, input, &restored)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Suspended || out != "distinct" {
		t.Fatalf("resumed run took the wrong branch: out=%v", out)
	}
	if stub.calls["tool.t.echo"] != 1 {
		t.Fatalf("memoized leaf re-invoked on resume: %d", stub.calls["tool.t.echo"])
	}

	// Equal integers above 2^53 still take the equal arm.
	var same map[string]any
	if err := jsonnum.Unmarshal([]byte(`{"a":9007199254740993,"b":9007199254740993}`), &same); err != nil {
		t.Fatal(err)
	}
	out, _, err = (&Interp{Invoker: &echoStub{}}).RunResumable(context.Background(), prog, same, nil)
	if err != nil || out != "equal-input" {
		t.Fatalf("equal large ints: out=%v err=%v", out, err)
	}
}

func firstKey(m map[string]any) string {
	for k := range m {
		return k
	}
	return ""
}

// TestLiveAndReplayedValuesAreTheSameRepresentation: a tool result carrying a whole-valued
// float64 (or json.Number) is canonicalized BEFORE it is memoized and bound, so the live run and a
// run resumed from the JSON checkpoint see the identical Go value (S7 replay determinism).
func TestLiveAndReplayedValuesAreTheSameRepresentation(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&InvokeTool{Bind: "r", Uses: "tool.t.n"},
		&Return{Value: ref("r")},
	}}
	stub := &fixedResult{res: map[string]any{
		"whole": float64(1000000), "frac": 0.5, "num": json.Number("9007199254740993"), "n": 3,
	}}
	out, st, err := (&Interp{Invoker: stub}).RunResumable(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"whole": int64(1000000), "frac": 0.5, "num": int64(9007199254740993), "n": int64(3)}
	m := out.(map[string]any)
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("live %s = %#v, want %#v", k, m[k], v)
		}
	}
	raw, _ := json.Marshal(st)
	var restored RunState
	if err := jsonnum.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	replayed := restored.Memo[firstKey(restored.Memo)].(map[string]any)
	for k, v := range want {
		if replayed[k] != v {
			t.Fatalf("replayed %s = %#v, want %#v (live and resumed must match)", k, replayed[k], v)
		}
	}
}

type fixedResult struct{ res any }

func (f *fixedResult) InvokeTool(context.Context, CallSite, string, map[string]any) (any, error) {
	return f.res, nil
}
func (f *fixedResult) InvokeAgent(context.Context, CallSite, string, map[string]any) (any, error) {
	return f.res, nil
}
func (f *fixedResult) InvokeWorkflow(context.Context, CallSite, string, map[string]any) (any, error) {
	return f.res, nil
}
func (f *fixedResult) InvokeApproval(_ context.Context, _ CallSite, _ ApprovalInfo, a map[string]any) (any, error) {
	return a, nil
}

// TestFloatInputAndSmallIntegersUnaffected: ordinary numbers keep their meaning.
func TestFloatInputAndSmallIntegersUnaffected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want any
	}{
		{`{"a":1,"b":1.0}`, "equal-input"}, // 1 == 1.0
		{`{"a":1.5,"b":1.5}`, "equal-input"},
		{`{"a":0.1,"b":0.2}`, "distinct"},
		{`{"a":-3,"b":-3}`, "equal-input"},
	} {
		var input map[string]any
		if err := jsonnum.Unmarshal([]byte(tc.in), &input); err != nil {
			t.Fatal(err)
		}
		st := &echoStub{}
		out, _, err := (&Interp{Invoker: st}).RunResumable(context.Background(), bigIntProgram(), input, nil)
		if err != nil {
			t.Fatal(err)
		}
		// With no suspend configured the gate tool returns, so a != b reaches the memo compare.
		if out != tc.want {
			t.Fatalf("%s: out=%v, want %v", tc.in, out, tc.want)
		}
	}
}

// TestTypedLeafResultsMatchReplay: a producer returning Go types outside the
// map[string]any/[]any/int64/float64 set (a uint64, a float32, a typed slice such
// as workspace.grep's []map[string]any, a struct) is bound live as exactly the
// value a checkpoint resume replays, so branches agree live and on resume (S7).
func TestTypedLeafResultsMatchReplay(t *testing.T) {
	t.Parallel()
	type hit struct {
		Line uint32 `json:"line"`
	}
	eq := func(x Value, y any) Expr { return BinOp{Op: "==", X: Leaf{V: x}, Y: Leaf{V: Lit{V: y}}} }
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&InvokeTool{Bind: "r", Uses: "tool.t.n"},
		&Branch{Cond: eq(ref("r", "u"), int64(5)), Else: []Node{&Return{Value: Lit{V: "u-mismatch"}}}},
		&Branch{Cond: eq(ref("r", "f"), 0.1), Else: []Node{&Return{Value: Lit{V: "f-mismatch"}}}},
		&Return{Value: ref("r")},
	}}
	stub := &fixedResult{res: map[string]any{
		"u": uint64(5), "f": float32(0.1), "big": uint64(9007199254740993),
		"matches": []map[string]any{{"n": 1.0}}, "hit": hit{Line: 3}, "hits": []hit{{Line: 4}},
	}}
	out, st, err := (&Interp{Invoker: stub}).RunResumable(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"u": int64(5), "f": 0.1, "big": int64(9007199254740993),
		"matches": []any{map[string]any{"n": int64(1)}},
		"hit":     map[string]any{"line": int64(3)}, "hits": []any{map[string]any{"line": int64(4)}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("live = %#v\nwant %#v", out, want)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var restored RunState
	if err := jsonnum.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if replayed := restored.Memo[firstKey(restored.Memo)]; !reflect.DeepEqual(replayed, out) {
		t.Fatalf("replayed %#v != live %#v", replayed, out)
	}
}

// A leaf result with no JSON encoding cannot be checkpointed, so there is no
// replayed value for it to equal: the leaf fails instead of flowing on live.
func TestUnencodableLeafResultFails(t *testing.T) {
	t.Parallel()
	prog := &Program{Workflow: "W", Params: []string{"input"}, Body: []Node{
		&InvokeTool{Bind: "r", Uses: "tool.t.n"},
		&Return{Value: ref("r")},
	}}
	for _, res := range []any{math.NaN(), map[string]any{"x": math.Inf(1)}, make(chan int)} {
		if _, _, err := (&Interp{Invoker: &fixedResult{res: res}}).RunResumable(context.Background(), prog, nil, nil); err == nil {
			t.Errorf("result %#v: want error", res)
		}
	}
}
