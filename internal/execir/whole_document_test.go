package execir

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func wholeDocProgram(whole bool) *Program {
	return &Program{Workflow: "w", Body: []Node{
		&InvokeAgent{Bind: "a", Agent: "Reviewer", WholeDocument: whole, Args: map[string]Value{"arg0": Ref{Path: []string{"input"}}}},
		&Graph{Nodes: []GraphNode{{ID: "g", Run: &InvokeAgent{Bind: "g", Agent: "Reviewer", WholeDocument: whole, Args: map[string]Value{"arg0": Lit{V: "x"}}}}}},
	}}
}

// TestSerialize_WholeDocumentRoundTrip: the call-shape bit survives the pinned
// wire form (top-level and inside a Graph node), and false stays absent from the
// wire so existing serialized programs are byte-identical.
func TestSerialize_WholeDocumentRoundTrip(t *testing.T) {
	t.Parallel()
	for _, whole := range []bool{true, false} {
		raw, err := MarshalPrograms(map[string]*Program{"w": wholeDocProgram(whole)})
		if err != nil {
			t.Fatal(err)
		}
		if has := strings.Contains(string(raw), "wholeDocument"); has != whole {
			t.Fatalf("whole=%v: wire contains wholeDocument = %v: %s", whole, has, raw)
		}
		got, err := UnmarshalPrograms(raw)
		if err != nil {
			t.Fatal(err)
		}
		top := got["w"].Body[0].(*InvokeAgent)
		inner := got["w"].Body[1].(*Graph).Nodes[0].Run.(*InvokeAgent)
		if top.WholeDocument != whole || inner.WholeDocument != whole {
			t.Fatalf("whole=%v: hydrated top=%v graph=%v", whole, top.WholeDocument, inner.WholeDocument)
		}
		if got["w"].Digest() != wholeDocProgram(whole).Digest() {
			t.Fatalf("whole=%v: digest changed across round trip", whole)
		}
	}
}

// TestDigest_WholeDocumentIsIdentity: two calls with the same Args but different
// shapes execute differently, so they must not share a digest.
func TestDigest_WholeDocumentIsIdentity(t *testing.T) {
	t.Parallel()
	if wholeDocProgram(true).Digest() == wholeDocProgram(false).Digest() {
		t.Fatal("WholeDocument must be part of the digest")
	}
}

type shapeRecorder struct {
	mu     sync.Mutex
	shapes map[string]bool
}

func (s *shapeRecorder) InvokeTool(context.Context, CallSite, string, map[string]any) (any, error) {
	return nil, nil
}
func (s *shapeRecorder) InvokeAgent(_ context.Context, site CallSite, _ string, _ map[string]any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shapes[site.Bind] = site.WholeDocument
	return nil, nil
}
func (s *shapeRecorder) InvokeWorkflow(context.Context, CallSite, string, map[string]any) (any, error) {
	return nil, nil
}
func (s *shapeRecorder) InvokeApproval(context.Context, CallSite, ApprovalInfo, map[string]any) (any, error) {
	return nil, nil
}

// TestInterp_CarriesWholeDocumentOnCallSite: the interpreter hands the node's
// explicit shape bit to the Invoker; CallKey (memo identity) ignores it.
func TestInterp_CarriesWholeDocumentOnCallSite(t *testing.T) {
	t.Parallel()
	rec := &shapeRecorder{shapes: map[string]bool{}}
	prog := &Program{Workflow: "w", Body: []Node{
		&InvokeAgent{Bind: "pos", Agent: "A", WholeDocument: true, Args: map[string]Value{"arg0": Lit{V: "x"}}},
		&InvokeAgent{Bind: "named", Agent: "A", Args: map[string]Value{"arg0": Lit{V: "x"}}},
	}}
	if _, err := (&Interp{Invoker: rec}).Run(context.Background(), prog, nil); err != nil {
		t.Fatal(err)
	}
	if !rec.shapes["pos"] || rec.shapes["named"] {
		t.Fatalf("shapes = %v", rec.shapes)
	}
	a := CallSite{Bind: "b", Path: []int{0}}
	b := a
	b.WholeDocument = true
	if CallKey(a) != CallKey(b) {
		t.Fatal("CallKey must not depend on call shape")
	}
}
