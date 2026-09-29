package execir

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func callShapeProgram(whole, project bool) *Program {
	return &Program{Workflow: "parent", Params: []string{"input"}, Body: []Node{
		&InvokeWorkflow{Bind: "c", Workflow: "child", WholeDocument: whole, ProjectValue: project,
			Args: map[string]Value{"value": Ref{Path: []string{"input", "doc"}}}},
		&Return{Value: Ref{Path: []string{"c"}}},
	}}
}

// The two InvokeWorkflow bits are executable identity, but only when set: a
// program with neither keeps the exact digest and wire bytes it had before the
// bits existed (pinned from the merge base), so no deployed plan goes stale.
func TestInvokeWorkflowBits_digestAndWire(t *testing.T) {
	t.Parallel()
	const baseDigest = "d601f86489eb4ba995f9b290caf21317661d361c7ddec3a35c30a7a2cf620ada"
	const baseWire = `{"formatVersion":"agentic.dev/execir/v1","programs":{"parent":{"workflow":"parent","params":["input"],"body":[{"kind":"invokeWorkflow","bind":"c","workflow":"child","args":{"value":{"kind":"ref","path":["input","doc"]}}},{"kind":"return","value":{"kind":"ref","path":["c"]}}]}}}`

	plain := callShapeProgram(false, false)
	if got := plain.Digest(); got != baseDigest {
		t.Fatalf("digest of a program without the bits changed: %s, want %s", got, baseDigest)
	}
	wire, err := MarshalPrograms(map[string]*Program{"parent": plain})
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != baseWire {
		t.Fatalf("wire form of a program without the bits changed:\n%s\nwant\n%s", wire, baseWire)
	}

	seen := map[string]bool{baseDigest: true}
	for _, tc := range []struct{ whole, project bool }{{true, false}, {false, true}, {true, true}} {
		p := callShapeProgram(tc.whole, tc.project)
		d := p.Digest()
		if seen[d] {
			t.Fatalf("whole=%v project=%v: digest %s collides with another shape", tc.whole, tc.project, d)
		}
		seen[d] = true
		b, err := MarshalPrograms(map[string]*Program{"parent": p})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "wholeDocument") != tc.whole || strings.Contains(string(b), "projectValue") != tc.project {
			t.Fatalf("whole=%v project=%v: wire %s", tc.whole, tc.project, b)
		}
		back, err := UnmarshalPrograms(b)
		if err != nil {
			t.Fatal(err)
		}
		iw := back["parent"].Body[0].(*InvokeWorkflow)
		if iw.WholeDocument != tc.whole || iw.ProjectValue != tc.project {
			t.Fatalf("round trip lost the bits: %+v", iw)
		}
		if back["parent"].Digest() != d {
			t.Fatalf("round trip changed the digest")
		}
	}
}

// wfOutputStub returns a fixed subworkflow output document and records the call
// sites it saw.
type wfOutputStub struct {
	mu    sync.Mutex
	out   any
	calls int
	sites []CallSite
}

func (s *wfOutputStub) InvokeTool(context.Context, CallSite, string, map[string]any) (any, error) {
	return nil, nil
}
func (s *wfOutputStub) InvokeAgent(context.Context, CallSite, string, map[string]any) (any, error) {
	return nil, nil
}
func (s *wfOutputStub) InvokeApproval(context.Context, CallSite, ApprovalInfo, map[string]any) (any, error) {
	return nil, nil
}
func (s *wfOutputStub) InvokeWorkflow(_ context.Context, site CallSite, _ string, _ map[string]any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.sites = append(s.sites, site)
	return s.out, nil
}

// ProjectValue binds the output document's value field, while the memo keeps the
// invoker's result (the output document) so a resume replays the same document
// and re-applies the projection. WholeDocument reaches the invoker on the
// CallSite but never changes the memo key.
func TestInvokeWorkflow_projectValueAndWholeDocument(t *testing.T) {
	t.Parallel()
	stub := &wfOutputStub{out: map[string]any{"value": []any{"a"}}}
	in := &Interp{Invoker: stub}
	out, st, err := in.RunResumable(context.Background(), callShapeProgram(true, true), map[string]any{"doc": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := out.([]any); !ok || len(got) != 1 || got[0] != "a" {
		t.Fatalf("binding = %#v, want the projected value [a]", out)
	}
	if len(stub.sites) != 1 || !stub.sites[0].WholeDocument {
		t.Fatalf("invoker sites %+v, want one whole-document site", stub.sites)
	}
	key := CallKey(CallSite{Bind: "c", Path: []int{0}})
	if CallKey(stub.sites[0]) != key {
		t.Fatalf("WholeDocument changed the call key: %q vs %q", CallKey(stub.sites[0]), key)
	}
	memo, ok := st.Memo[key].(map[string]any)
	if !ok || len(memo) != 1 {
		t.Fatalf("memo[%s] = %#v, want the raw output document", key, st.Memo[key])
	}

	// Replay from the memo: no new invocation, same projected binding.
	out2, _, err := in.RunResumable(context.Background(), callShapeProgram(true, true), map[string]any{"doc": "x"}, &RunState{Memo: st.Memo})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("memoized call re-invoked: %d calls", stub.calls)
	}
	if got, ok := out2.([]any); !ok || len(got) != 1 || got[0] != "a" {
		t.Fatalf("replayed binding = %#v, want [a]", out2)
	}

	// Without ProjectValue the binding is the whole output document.
	out3, err := (&Interp{Invoker: &wfOutputStub{out: map[string]any{"value": "v"}}}).Run(context.Background(), callShapeProgram(false, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := out3.(map[string]any); !ok || m["value"] != "v" {
		t.Fatalf("unprojected binding = %#v, want the output document", out3)
	}
}

// A projection over something that is not an output document is a program/callee
// envelope mismatch and fails loudly instead of binding nil.
func TestInvokeWorkflow_projectValueRejectsNonDocument(t *testing.T) {
	t.Parallel()
	for _, out := range []any{"scalar", nil} {
		_, err := (&Interp{Invoker: &wfOutputStub{out: out}}).Run(context.Background(), callShapeProgram(false, true), nil)
		if err == nil || !strings.Contains(err.Error(), "child") {
			t.Fatalf("projecting %#v: err = %v, want a loud error naming the callee", out, err)
		}
	}
}
