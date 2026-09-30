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
	in, _ := ictx.Input.(map[string]any)
	if in["a"] != int64(9007199254740992) || in["b"] != int64(9007199254740993) {
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

// twoP60 is float64(2^60), the value of the program literal 1152921504606846976.0.
// Its shortest encoding/json spelling, 1152921504606847000, is a DIFFERENT integer,
// so a raw float64 persisted in a checkpoint comes back changed on resume.
const twoP60 = float64(1 << 60)

// exactArmProgram branches on v == 1152921504606846976 (the int literal): "exact"
// when the value survived, "CHANGED" when a checkpoint round trip rewrote it.
func exactArmProgram(v execir.Value, arm func(string) execir.Node) execir.Node {
	return exactArmBranch(v, func(a string) []execir.Node { return []execir.Node{arm(a)} })
}

// exactArmBranch is exactArmProgram with a multi-node body per arm.
func exactArmBranch(v execir.Value, arm func(string) []execir.Node) execir.Node {
	return &execir.Branch{
		Cond: execir.BinOp{Op: "==", X: execir.Leaf{V: v}, Y: execir.Leaf{V: execir.Lit{V: int64(1152921504606846976)}}},
		Then: arm("exact"),
		Else: arm("CHANGED"),
	}
}

// runGatedVariants drives the same workflow ungated/auto-approved (fresh run straight
// through) and gated (fresh run suspends, an approve decision resumes it), and
// returns each variant's final run output and, for the gated variant, the
// interrupted checkpoint's context JSON.
func runGatedVariants(t *testing.T, graph func() *spec.ProjectGraph, wf string, progs func() map[string]*execir.Program) (auto, gated, gatedCheckpoint string) {
	t.Helper()
	ctx := context.Background()
	input := map[string]any{"topic": "hi"}

	ex, _, runID, started := newResumeExecutor(t, graph(), wf)
	ex.Executables = progs()
	if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: wf, Env: "dev", StartedAt: started, Input: input,
		Hitl: HitlRunOptions{AutoApprove: true, Actor: "alice"}}); err != nil {
		t.Fatalf("auto-approved run: %v", err)
	}
	run, _ := ex.Store.GetRun(ctx, runID)
	if run.Status != state.RunStatusSucceeded {
		t.Fatalf("auto-approved status = %q err=%q", run.Status, run.ErrorText)
	}
	auto = run.OutputJSON

	ex, _, runID, started = newResumeExecutor(t, graph(), wf)
	ex.Executables = progs()
	base := RunInput{RunID: runID, WorkflowName: wf, Env: "dev", StartedAt: started, Input: input}
	if err := ex.Run(ctx, base); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("gated run should interrupt, got %v", err)
	}
	cp, err := ex.Store.GetLatestCheckpoint(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	gatedCheckpoint = cp.ContextJSON
	resume := base
	resume.Resume = true
	resume.Hitl = HitlRunOptions{Actor: "alice", Decision: &policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: "alice"}}
	if err := ex.Run(ctx, resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	run, _ = ex.Store.GetRun(ctx, runID)
	if run.Status != state.RunStatusSucceeded {
		t.Fatalf("resumed status = %q err=%q", run.Status, run.ErrorText)
	}
	return auto, run.OutputJSON, gatedCheckpoint
}

// TestExecIRResume_FloatLiteralSubworkflowArgSurvivesNestedGate is the S7 regression for a
// subworkflow frame's input: the outer workflow passes the float literal
// 1152921504606846976.0 (float64(2^60)) to a child that runs prep(topic: input.topic), a
// HITL gate, then branches on input.topic == 1152921504606846976. The child's input is
// persisted in the nested frame (NestedRunState.Input) and fed back on resume, so it must be
// the canonical value: otherwise the resumed child sees 1152921504606847000 and takes a
// different arm than the same run without the gate.
func TestExecIRResume_FloatLiteralSubworkflowArgSurvivesNestedGate(t *testing.T) {
	t.Parallel()
	graph := func() *spec.ProjectGraph {
		g := nestedSubworkflowGraph()
		g.Tools["after"] = &spec.ToolResource{APIVersion: spec.APIVersionV0, Kind: spec.KindTool, Metadata: spec.Metadata{Name: "after"}, Spec: spec.ToolSpec{Type: "native", Safety: &spec.ToolSafety{SideEffects: spec.BoolPtr(false)}}}
		g.Workflows["inner"].Spec.Output = &spec.WorkflowOutput{Value: map[string]any{
			"arm": "${steps.after.output.echo.arm}", "topic": "${input.topic}", "prep": "${steps.prep.output.echo.topic}",
		}}
		g.Workflows["outer"].Spec.Output = &spec.WorkflowOutput{Value: map[string]any{"value": "${steps.sub.output}"}}
		return g
	}
	progs := func(gateUses string) func() map[string]*execir.Program {
		return func() map[string]*execir.Program {
			return map[string]*execir.Program{
				"outer": {Workflow: "outer", Params: []string{"input"}, Body: []execir.Node{
					&execir.InvokeWorkflow{Bind: "sub", Workflow: "inner", Args: map[string]execir.Value{"topic": execir.Lit{V: twoP60}}},
					&execir.Return{Value: bigIntRef("sub")},
				}},
				"inner": {Workflow: "inner", Params: []string{"input"}, Body: []execir.Node{
					&execir.InvokeTool{Bind: "prep", Uses: "tool.helper.echo", Args: map[string]execir.Value{"topic": bigIntRef("input", "topic")}},
					&execir.InvokeTool{Bind: "gatestep", Uses: gateUses, Args: map[string]execir.Value{"body": execir.Lit{V: "x"}}},
					exactArmBranch(bigIntRef("input", "topic"), func(arm string) []execir.Node {
						return []execir.Node{
							&execir.InvokeTool{Bind: "after", Uses: "tool.after.echo", Args: map[string]execir.Value{"arm": execir.Lit{V: arm}}},
							// The Return LowerWorkflowResource derives from the inner output.value above.
							&execir.Return{Value: execir.Object{Fields: []execir.Field{
								{Key: "arm", Val: bigIntRef("after", "echo", "arm")},
								{Key: "prep", Val: bigIntRef("prep", "echo", "topic")},
								{Key: "topic", Val: bigIntRef("input", "topic")},
							}}},
						}
					}),
				}},
			}
		}
	}

	// No gate at all: the child runs straight through.
	ctx := context.Background()
	ex, _, runID, started := newResumeExecutor(t, graph(), "outer")
	ex.Executables = progs("tool.helper.echo")()
	if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "outer", Env: "dev", StartedAt: started, Input: map[string]any{"topic": "hi"}}); err != nil {
		t.Fatalf("ungated run: %v", err)
	}
	run, _ := ex.Store.GetRun(ctx, runID)
	ungated := run.OutputJSON

	auto, gated, cpJSON := runGatedVariants(t, graph, "outer", progs("tool.publisher.echo"))
	const want = `{"value":{"arm":"exact","prep":1152921504606846976,"topic":1152921504606846976}}`
	for name, got := range map[string]string{"ungated": ungated, "auto-approved": auto, "gated+resumed": gated} {
		if got != want {
			t.Errorf("%s output = %s, want %s", name, got, want)
		}
	}

	// The persisted frame holds the canonical fixed point, which decodes to the same int64.
	var payload checkpointPayload
	if err := jsonnum.Unmarshal([]byte(cpJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Nested == nil {
		t.Fatalf("gated checkpoint has no nested frame: %s", cpJSON)
	}
	nestedIn, _ := payload.Nested.Input.(map[string]any)
	if got := nestedIn["topic"]; got != int64(1152921504606846976) {
		t.Fatalf("persisted nested.input.topic = %#v (%T), want int64(1152921504606846976)", got, got)
	}
	if strings.Contains(cpJSON, "1152921504606847000") {
		t.Fatalf("checkpoint holds encoding/json's respelling of the raw float: %s", cpJSON)
	}
}

// TestExecIRResume_FloatLiteralWholeDocumentArgSurvivesNestedGate is the same S7 regression
// for a whole-document call (#552): the callee's input document is the argument's VALUE — here
// the bare float literal 1152921504606846976.0, not an object wrapping it. It is canonicalized
// with the argument map before the document is taken from it, so the child's interpolation
// input, its interpreter input and the persisted NestedRunState.Input (a scalar) are the same
// int64 live and after a resume.
func TestExecIRResume_FloatLiteralWholeDocumentArgSurvivesNestedGate(t *testing.T) {
	t.Parallel()
	graph := func() *spec.ProjectGraph {
		g := nestedSubworkflowGraph()
		g.Tools["after"] = &spec.ToolResource{APIVersion: spec.APIVersionV0, Kind: spec.KindTool, Metadata: spec.Metadata{Name: "after"}, Spec: spec.ToolSpec{Type: "native", Safety: &spec.ToolSafety{SideEffects: spec.BoolPtr(false)}}}
		return g
	}
	progs := func(gateUses string) func() map[string]*execir.Program {
		return func() map[string]*execir.Program {
			return map[string]*execir.Program{
				"outer": {Workflow: "outer", Params: []string{"input"}, Body: []execir.Node{
					&execir.InvokeWorkflow{Bind: "sub", Workflow: "inner", WholeDocument: true, Args: map[string]execir.Value{"topic": execir.Lit{V: twoP60}}},
					&execir.Return{Value: bigIntRef("sub")},
				}},
				"inner": {Workflow: "inner", Params: []string{"topic"}, Body: []execir.Node{
					&execir.InvokeTool{Bind: "prep", Uses: "tool.helper.echo", Args: map[string]execir.Value{"topic": bigIntRef("topic")}},
					&execir.InvokeTool{Bind: "gatestep", Uses: gateUses, Args: map[string]execir.Value{"body": execir.Lit{V: "x"}}},
					exactArmBranch(bigIntRef("topic"), func(arm string) []execir.Node {
						return []execir.Node{
							&execir.InvokeTool{Bind: "after", Uses: "tool.after.echo", Args: map[string]execir.Value{"arm": execir.Lit{V: arm}}},
							&execir.Return{Value: execir.Object{Fields: []execir.Field{
								{Key: "arm", Val: bigIntRef("after", "echo", "arm")},
								{Key: "topic", Val: bigIntRef("topic")},
							}}},
						}
					}),
				}},
			}
		}
	}

	ctx := context.Background()
	ex, _, runID, started := newResumeExecutor(t, graph(), "outer")
	ex.Executables = progs("tool.helper.echo")()
	if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "outer", Env: "dev", StartedAt: started, Input: map[string]any{"topic": "hi"}}); err != nil {
		t.Fatalf("ungated run: %v", err)
	}
	run, _ := ex.Store.GetRun(ctx, runID)
	ungated := run.OutputJSON

	auto, gated, cpJSON := runGatedVariants(t, graph, "outer", progs("tool.publisher.echo"))
	const want = `{"value":{"arm":"exact","topic":1152921504606846976}}`
	for name, got := range map[string]string{"ungated": ungated, "auto-approved": auto, "gated+resumed": gated} {
		if got != want {
			t.Errorf("%s output = %s, want %s", name, got, want)
		}
	}

	var payload checkpointPayload
	if err := jsonnum.Unmarshal([]byte(cpJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Nested == nil {
		t.Fatalf("gated checkpoint has no nested frame: %s", cpJSON)
	}
	if got := payload.Nested.Input; got != int64(1152921504606846976) {
		t.Fatalf("persisted whole-document nested.input = %#v (%T), want int64(1152921504606846976)", got, got)
	}
	if payload.Nested.InputParam != "topic" {
		t.Fatalf("persisted nested.inputParam = %q, want topic", payload.Nested.InputParam)
	}
	if strings.Contains(cpJSON, "1152921504606847000") {
		t.Fatalf("checkpoint holds encoding/json's respelling of the raw float: %s", cpJSON)
	}
}

// TestExecIRResume_FloatLiteralApprovalPayloadSurvivesGate is the same regression for a
// PendingHitl.With payload: an approval node's payload is persisted as the pending gate's
// With and, on an approve resume, becomes the node's output. A float literal past 2^53 in it
// must reach the post-approval branch as the same integer as under auto-approve.
func TestExecIRResume_FloatLiteralApprovalPayloadSurvivesGate(t *testing.T) {
	t.Parallel()
	graph := func() *spec.ProjectGraph {
		// The program's two object-literal Returns are the output document itself
		// (lower.WorkflowReturnShape), so no resource output.value is needed.
		return approvalGraph()
	}
	progs := func() map[string]*execir.Program {
		return map[string]*execir.Program{
			"appr": {Workflow: "appr", Params: []string{"input"}, Body: []execir.Node{
				&execir.Approval{Bind: "gate", Args: map[string]execir.Value{"n": execir.Lit{V: twoP60}}},
				exactArmProgram(bigIntRef("gate", "n"), func(arm string) execir.Node {
					return &execir.Return{Value: execir.Object{Fields: []execir.Field{
						{Key: "arm", Val: execir.Lit{V: arm}}, {Key: "n", Val: bigIntRef("gate", "n")},
					}}}
				}),
			}},
		}
	}
	auto, gated, cpJSON := runGatedVariants(t, graph, "appr", progs)
	const want = `{"arm":"exact","n":1152921504606846976}`
	if auto != want || gated != want {
		t.Fatalf("auto-approved = %s, gated+resumed = %s, want both %s", auto, gated, want)
	}
	var payload checkpointPayload
	if err := jsonnum.Unmarshal([]byte(cpJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PendingHitl == nil || payload.PendingHitl.With["n"] != int64(1152921504606846976) {
		t.Fatalf("persisted pendingHitl.with is not the canonical value: %s", cpJSON)
	}
}

// TestExecIRResume_FloatLiteralToolGateArgsSurviveGate covers a HITL-gated uses: call: its
// arguments are persisted as PendingHitl.With and, on an approve resume, are what the tool is
// dispatched with. The tool must see the same number gated as ungated.
func TestExecIRResume_FloatLiteralToolGateArgsSurviveGate(t *testing.T) {
	t.Parallel()
	graph := func() *spec.ProjectGraph {
		// As above: the two object-literal Returns are the output document.
		return gatedTwoStepGraph()
	}
	progs := func() map[string]*execir.Program {
		return map[string]*execir.Program{
			"pub": {Workflow: "pub", Params: []string{"input"}, Body: []execir.Node{
				&execir.InvokeTool{Bind: "pub", Uses: "tool.publisher.echo", Args: map[string]execir.Value{"n": execir.Lit{V: twoP60}}},
				exactArmProgram(bigIntRef("pub", "echo", "n"), func(arm string) execir.Node {
					return &execir.Return{Value: execir.Object{Fields: []execir.Field{
						{Key: "arm", Val: execir.Lit{V: arm}}, {Key: "n", Val: bigIntRef("pub", "echo", "n")},
					}}}
				}),
			}},
		}
	}
	auto, gated, cpJSON := runGatedVariants(t, graph, "pub", progs)
	const want = `{"arm":"exact","n":1152921504606846976}`
	if auto != want || gated != want {
		t.Fatalf("auto-approved = %s, gated+resumed = %s, want both %s", auto, gated, want)
	}
	var payload checkpointPayload
	if err := jsonnum.Unmarshal([]byte(cpJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PendingHitl == nil || payload.PendingHitl.With["n"] != int64(1152921504606846976) {
		t.Fatalf("persisted pendingHitl.with is not the canonical value: %s", cpJSON)
	}
}

// TestExecIRResume_RawRunInputAndSubworkflowOutputAreCanonical covers the two other raw
// values that reach a checkpoint's interpolation context: the run input supplied as Go
// values (checkpointPayload.Input, read back as ${input.*}) and a completed subworkflow's
// output carrying an output.value float literal (the parent's checkpointed Steps, read back
// as ${steps.*}). Both must render the exact integer whether or not the run was
// interrupted by a gate.
func TestExecIRResume_RawRunInputAndSubworkflowOutputAreCanonical(t *testing.T) {
	t.Parallel()
	graph := func() *spec.ProjectGraph {
		g := gatedTwoStepGraph()
		g.Workflows["lit"] = &spec.WorkflowResource{
			APIVersion: spec.APIVersionV0, Kind: spec.KindWorkflow, Metadata: spec.Metadata{Name: "lit"},
			Spec: spec.WorkflowSpec{
				Policy: "gate",
				Steps:  []spec.WorkflowStep{{ID: "prep", Uses: "tool.helper.echo", With: map[string]any{"x": "1"}}},
				Output: &spec.WorkflowOutput{Value: map[string]any{"lit": twoP60}},
			},
		}
		// A multi-key output is the program's object-literal Return (below), whose refs
		// resolve against the input and the sub step's memoized output, which a resume
		// hydrates from the checkpoint.
		g.Workflows["pub"].Spec.Output = &spec.WorkflowOutput{Value: map[string]any{
			"topic": "${input.topic}", "lit": "${steps.sub.output.lit}",
		}}
		return g
	}
	progs := func() map[string]*execir.Program {
		return map[string]*execir.Program{
			"pub": {Workflow: "pub", Params: []string{"input"}, Body: []execir.Node{
				&execir.InvokeWorkflow{Bind: "sub", Workflow: "lit"},
				&execir.InvokeTool{Bind: "pub", Uses: "tool.publisher.echo", Args: map[string]execir.Value{"body": execir.Lit{V: "x"}}},
				&execir.Return{Value: execir.Object{Fields: []execir.Field{
					{Key: "lit", Val: bigIntRef("sub", "lit")}, {Key: "topic", Val: bigIntRef("input", "topic")},
				}}},
			}},
			"lit": {Workflow: "lit", Params: []string{"input"}, Body: []execir.Node{
				&execir.InvokeTool{Bind: "prep", Uses: "tool.helper.echo", Args: map[string]execir.Value{"x": execir.Lit{V: "1"}}},
				// output.value {lit: 1152921504606846976.0}: a raw float64 program literal.
				&execir.Return{Value: execir.Object{Fields: []execir.Field{{Key: "lit", Val: execir.Lit{V: twoP60}}}}},
			}},
		}
	}
	ctx := context.Background()
	raw := func() map[string]any { return map[string]any{"topic": twoP60} }
	const want = `{"lit":1152921504606846976,"topic":1152921504606846976}`

	ex, _, runID, started := newResumeExecutor(t, graph(), "pub")
	ex.Executables = progs()
	if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "pub", Env: "dev", StartedAt: started, Input: raw(),
		Hitl: HitlRunOptions{AutoApprove: true, Actor: "alice"}}); err != nil {
		t.Fatalf("auto-approved run: %v", err)
	}
	if run, _ := ex.Store.GetRun(ctx, runID); run.OutputJSON != want {
		t.Fatalf("auto-approved output = %s, want %s", run.OutputJSON, want)
	}

	ex, _, runID, started = newResumeExecutor(t, graph(), "pub")
	ex.Executables = progs()
	base := RunInput{RunID: runID, WorkflowName: "pub", Env: "dev", StartedAt: started, Input: raw()}
	if err := ex.Run(ctx, base); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("gated run should interrupt, got %v", err)
	}
	resume := base
	resume.Input = raw()
	resume.Resume = true
	resume.Hitl = HitlRunOptions{Actor: "alice", Decision: &policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: "alice"}}
	if err := ex.Run(ctx, resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if run, _ := ex.Store.GetRun(ctx, runID); run.Status != state.RunStatusSucceeded || run.OutputJSON != want {
		t.Fatalf("gated+resumed status = %q output = %s (err %q), want %s", run.Status, run.OutputJSON, run.ErrorText, want)
	}
}
