package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/spec"
)

// TestAgentInputDocument pins the runtime boundary: the input shape is the
// explicit WholeDocument bit, never the with: key name (#550).
func TestAgentInputDocument(t *testing.T) {
	t.Parallel()
	whole := spec.WorkflowStep{ID: "s", Agent: "a", WholeDocument: true}
	named := spec.WorkflowStep{ID: "s", Agent: "a"}

	if got, err := agentInputDocument(whole, map[string]any{"arg0": "hello"}); err != nil || got != "hello" {
		t.Fatalf("whole-document string: got %#v err=%v", got, err)
	}
	doc := map[string]any{"repo": "terfyn", "number": 550}
	got, err := agentInputDocument(whole, map[string]any{"arg0": doc})
	if err != nil || !sameJSON(t, got, doc) {
		t.Fatalf("whole-document object: got %#v err=%v", got, err)
	}

	// A named call whose field is literally arg0 stays an object.
	lit := map[string]any{"arg0": "v"}
	if got, err := agentInputDocument(named, lit); err != nil || !sameJSON(t, got, lit) {
		t.Fatalf("named arg0 must stay an object, got %#v err=%v", got, err)
	}
	// Multi-argument positional calls (no whole-document bit) stay an object.
	multi := map[string]any{"arg0": "a", "arg1": "b"}
	if got, err := agentInputDocument(named, multi); err != nil || !sameJSON(t, got, multi) {
		t.Fatalf("multi-arg map must stay an object, got %#v err=%v", got, err)
	}
	if got, err := agentInputDocument(named, nil); err != nil || got != nil {
		t.Fatalf("nil with: got %#v err=%v", got, err)
	}

	// The bit on a step without exactly the one placeholder argument is a
	// representation violation, refused instead of silently sent as an object.
	for name, with := range map[string]map[string]any{
		"multi":     {"arg0": "a", "arg1": "b"},
		"wrong key": {"topic": "x"},
		"empty":     nil,
	} {
		if _, err := agentInputDocument(whole, with); err == nil {
			t.Fatalf("%s: whole-document step with %v must be refused", name, with)
		}
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

// runInvokeAgentProgram runs a one-node program (an InvokeAgent shaped as given)
// after a marshal/unmarshal round trip — the form a pinned resume executes — and
// returns the user content the model actually received.
func runInvokeAgentProgram(t *testing.T, node *execir.InvokeAgent) string {
	t.Helper()
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "echo the input"}, spec.PolicySpec{})
	prog := &execir.Program{Workflow: "demo", Params: []string{"input"}, Body: []execir.Node{
		node,
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
	return user
}

// TestRun_positionalAgentArgSendsWholeDocument proves a single positional agent
// argument is the model user content (#550). A test that only inspected the
// lowered Args["arg0"] map would preserve the bug.
func TestRun_positionalAgentArgSendsWholeDocument(t *testing.T) {
	user := runInvokeAgentProgram(t, &execir.InvokeAgent{
		Bind: "act", Agent: "reviewer", WholeDocument: true,
		Args: map[string]execir.Value{"arg0": execir.Lit{V: "hello"}},
	})
	if user != `"hello"` {
		t.Fatalf("model user content = %q, want JSON string document %q (not {\"arg0\":...})", user, `"hello"`)
	}
}

// TestRun_namedArg0AgentCallStaysObject is the review's counterexample: an
// explicitly named call whose input field is literally arg0 is NOT a positional
// call and must reach the model as an object.
func TestRun_namedArg0AgentCallStaysObject(t *testing.T) {
	user := runInvokeAgentProgram(t, &execir.InvokeAgent{
		Bind: "act", Agent: "reviewer",
		Args: map[string]execir.Value{"arg0": execir.Lit{V: "hello"}},
	})
	if user != `{"arg0":"hello"}` {
		t.Fatalf("named arg0 call must stay an object, got %q", user)
	}
}

// TestRun_multiPositionalAgentCallStaysObject: with no defined field-order ABI for
// several positional arguments (the checker warns), the call is not a whole
// document; the placeholder-keyed map is the input, never a silent unwrap.
func TestRun_multiPositionalAgentCallStaysObject(t *testing.T) {
	user := runInvokeAgentProgram(t, &execir.InvokeAgent{
		Bind: "act", Agent: "reviewer",
		Args: map[string]execir.Value{"arg0": execir.Lit{V: "a"}, "arg1": execir.Lit{V: "b"}},
	})
	if user != `{"arg0":"a","arg1":"b"}` {
		t.Fatalf("multi-arg positional call must stay an object, got %q", user)
	}
}

// TestRun_wholeDocumentBitWithExtraArgsFails: a hydrated program that carries the
// bit on a call with more than the one placeholder argument is refused at runtime.
func TestRun_wholeDocumentBitWithExtraArgsFails(t *testing.T) {
	graph := agentLoopGraph(t, spec.AgentSpec{Instructions: "echo the input"}, spec.PolicySpec{})
	prog := &execir.Program{Workflow: "demo", Params: []string{"input"}, Body: []execir.Node{
		&execir.InvokeAgent{Bind: "act", Agent: "reviewer", WholeDocument: true, Args: map[string]execir.Value{
			"arg0": execir.Lit{V: "a"}, "arg1": execir.Lit{V: "b"},
		}},
	}}
	mock := &models.MockClient{Content: `{"summary":"ok"}`}
	got, _, err := runAgentLoopCfg(t, graph, mock, nil, func(e *Executor) {
		e.Executables = map[string]*execir.Program{"demo": prog}
	})
	if err == nil && got.Status == "succeeded" {
		t.Fatal("whole-document call with extra arguments must not succeed")
	}
	if err != nil && !strings.Contains(err.Error(), "whole-document") {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(mock.Requests()); n != 0 {
		t.Fatalf("model must not be called, got %d generates", n)
	}
}

// TestRun_namedAgentWithStaysObject proves named with: keys are still marshaled
// as an object.
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
