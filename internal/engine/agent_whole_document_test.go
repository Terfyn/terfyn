package engine

import (
	"encoding/json"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/spec"
)

func TestUnwrapAgentWholeDocument(t *testing.T) {
	t.Parallel()
	if got := unwrapAgentWholeDocument(nil); got != nil {
		t.Fatalf("nil: got %#v", got)
	}
	named := map[string]any{"topic": "agents"}
	if got := unwrapAgentWholeDocument(named); !sameJSON(t, got, named) {
		t.Fatalf("named map must stay wrapped, got %#v", got)
	}
	multi := map[string]any{"arg0": "a", "arg1": "b"}
	if got := unwrapAgentWholeDocument(multi); !sameJSON(t, got, multi) {
		t.Fatalf("multi-arg map must stay wrapped, got %#v", got)
	}
	if got := unwrapAgentWholeDocument(map[string]any{"arg0": "hello"}); got != "hello" {
		t.Fatalf("single arg0 must unwrap, got %#v", got)
	}
	doc := map[string]any{"repo": "terfyn", "number": 550}
	got := unwrapAgentWholeDocument(map[string]any{"arg0": doc})
	if !sameJSON(t, got, doc) {
		t.Fatalf("single arg0 object must unwrap, got %#v", got)
	}
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	aa, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(aa) == string(bb)
}

// TestRun_positionalAgentArgSendsWholeDocument proves a single positional
// agent argument is the model user content (#550). A test that only inspected
// the lowered Args["arg0"] map would preserve the bug.
func TestRun_positionalAgentArgSendsWholeDocument(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "echo the input"}, spec.PolicySpec{})
	graph.Workflows["demo"].Spec.Steps[0].With = map[string]any{"arg0": "hello"}
	prog := &execir.Program{Workflow: "demo", Params: []string{"input"}, Body: []execir.Node{
		&execir.InvokeAgent{Bind: "act", Agent: "reviewer", Args: map[string]execir.Value{"arg0": execir.Lit{V: "hello"}}},
		&execir.Return{Value: execir.Ref{Path: []string{"act"}}},
	}}
	raw, err := execir.MarshalPrograms(map[string]*execir.Program{"demo": prog})
	if err != nil {
		t.Fatal(err)
	}
	hydrated, err := execir.UnmarshalPrograms(raw)
	if err != nil {
		t.Fatal(err)
	}

	mock := &models.MockClient{Content: `{"summary":"ok"}`}
	got, _, err := runAgentLoopCfg(t, graph, mock, nil, func(e *Executor) {
		e.Executables = hydrated
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("generates = %d, want 1", len(reqs))
	}
	var user string
	for _, m := range reqs[0].Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	if user != `"hello"` {
		t.Fatalf("model user content = %q, want JSON string document %q (not {\"arg0\":...})", user, `"hello"`)
	}
}

// TestRun_namedAgentWithStaysObject proves named with: keys are still marshaled
// as an object (the undefined named-single ABI is not this unwrap).
func TestRun_namedAgentWithStaysObject(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "echo the input"}, spec.PolicySpec{})
	mock := &models.MockClient{Content: `{"summary":"ok"}`}
	got, _, err := runAgentLoop(t, graph, mock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	var user string
	for _, m := range mock.Requests()[0].Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}
	if user != `{"topic":"agents"}` {
		t.Fatalf("named with must stay an object, got %q", user)
	}
}
