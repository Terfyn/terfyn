package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/jsonnum"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
)

func bigIntRef(p ...string) execir.Ref { return execir.Ref{Path: p} }

// TestExecIRResume_IntegersAboveTwo53SurviveCheckpoint runs the S7 scenario at the engine level
// with SERIALIZED JSON input and a real persisted checkpoint: two integers that differ only above
// 2^53 must compare unequal before the gate, be stored exactly in the checkpoint (input, step
// output, and interpreter memo), and still compare unequal — taking the same branch — after
// resume decodes that checkpoint.
func TestExecIRResume_IntegersAboveTwo53SurviveCheckpoint(t *testing.T) {
	t.Parallel()
	graph := gatedTwoStepGraph()
	// `.agent` convention: a single `value` output is the interpreter's Return value (the YAML
	// fixture's `steps.publish` is not a binding of this program).
	graph.Workflows["pub"].Spec.Output = &spec.WorkflowOutput{Value: map[string]any{"value": "${steps.prep.output}"}}
	ex, ct, runID, started := newResumeExecutor(t, graph, "pub")
	eq := func(x, y execir.Value) execir.Expr {
		return execir.BinOp{Op: "==", X: execir.Leaf{V: x}, Y: execir.Leaf{V: y}}
	}
	ex.Executables = map[string]*execir.Program{
		"pub": {Workflow: "pub", Params: []string{"input"}, Body: []execir.Node{
			&execir.InvokeTool{Bind: "prep", Uses: "tool.helper.echo", Args: map[string]execir.Value{
				"a": bigIntRef("input", "a"), "b": bigIntRef("input", "b"),
			}},
			&execir.Branch{Cond: eq(bigIntRef("input", "a"), bigIntRef("input", "b")),
				Then: []execir.Node{&execir.Return{Value: execir.Lit{V: "equal-input"}}}},
			&execir.InvokeTool{Bind: "pub", Uses: "tool.publisher.echo", Args: map[string]execir.Value{"body": execir.Lit{V: "x"}}},
			&execir.Branch{Cond: eq(bigIntRef("prep", "echo", "a"), bigIntRef("prep", "echo", "b")),
				Then: []execir.Node{&execir.Return{Value: execir.Lit{V: "equal-memo"}}}},
			&execir.Return{Value: bigIntRef("prep", "echo")},
		}},
	}
	ctx := context.Background()

	// Serialized ingress, exactly as the local runtime decodes a run's input.
	var input map[string]any
	if err := jsonnum.Unmarshal([]byte(`{"a":9007199254740992,"b":9007199254740993}`), &input); err != nil {
		t.Fatal(err)
	}
	base := RunInput{RunID: runID, WorkflowName: "pub", Env: "dev", StartedAt: started, Input: input}
	if err := ex.Run(ctx, base); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("a != b must reach the gate and interrupt, got %v", err)
	}

	cp, err := ex.Store.GetLatestCheckpoint(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"a":9007199254740992`, `"b":9007199254740993`} {
		// input + step output + interpreter memo
		if n := strings.Count(cp.ContextJSON, want); n < 3 {
			t.Fatalf("checkpoint holds %d copies of %s, want >= 3: %s", n, want, cp.ContextJSON)
		}
	}

	// Hydration is lossless: Input, the interpolation Steps, and the interpreter memo all decode to
	// the exact int64s.
	ictx, _, err := unmarshalCheckpointPayload(cp.ContextJSON, graph, graph.Workflows["pub"], cp.StepIndex)
	if err != nil {
		t.Fatal(err)
	}
	if ictx.Input["a"] != int64(9007199254740992) || ictx.Input["b"] != int64(9007199254740993) {
		t.Fatalf("checkpoint input not exact: %#v", ictx.Input)
	}
	prep := ictx.Steps["prep"].Output.(map[string]any)["echo"].(map[string]any)
	if prep["a"] != int64(9007199254740992) || prep["b"] != int64(9007199254740993) {
		t.Fatalf("checkpoint step output not exact: %#v", prep)
	}
	_, _, rs, _, err := ex.loadExecResumeState(ctx, base, graph.Workflows["pub"])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range rs.Memo {
		if m, ok := v.(map[string]any); ok {
			if e, ok := m["echo"].(map[string]any); ok && e["b"] == int64(9007199254740993) && e["a"] == int64(9007199254740992) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("interpreter memo not exact after hydration: %#v", rs.Memo)
	}

	// Resume with the input re-decoded from serialized JSON, as the runtime does from the run row.
	resume := base
	resume.Resume = true
	resume.Input = nil
	if err := jsonnum.Unmarshal([]byte(`{"a":9007199254740992,"b":9007199254740993}`), &resume.Input); err != nil {
		t.Fatal(err)
	}
	resume.Hitl = HitlRunOptions{Actor: "alice", Decision: &policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: "alice"}}
	if err := ex.Run(ctx, resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := ex.Store.GetRun(ctx, runID)
	if got.Status != state.RunStatusSucceeded {
		t.Fatalf("resume status = %q err=%q", got.Status, got.ErrorText)
	}
	if !strings.Contains(got.OutputJSON, `"a":9007199254740992`) || !strings.Contains(got.OutputJSON, `"b":9007199254740993`) {
		t.Fatalf("run output lost integer precision or took the wrong branch: %s", got.OutputJSON)
	}
	if ct.count("tool.helper.echo") != 1 || ct.count("tool.publisher.echo") != 1 {
		t.Fatalf("effects not exactly once: helper=%d publisher=%d", ct.count("tool.helper.echo"), ct.count("tool.publisher.echo"))
	}
}
