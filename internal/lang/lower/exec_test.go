package lower

import (
	"context"
	"sync"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// endToEndInvoker records tool calls and returns a per-uses canned result.
type endToEndInvoker struct {
	mu    sync.Mutex
	calls []string
}

func (e *endToEndInvoker) InvokeTool(_ context.Context, _ execir.CallSite, uses string, args map[string]any) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	label := uses
	if v, ok := args["v"]; ok {
		label += ":"
		if s, ok := v.(string); ok {
			label += s
		}
	}
	e.calls = append(e.calls, label)
	return map[string]any{"ok": true}, nil
}
func (e *endToEndInvoker) InvokeAgent(_ context.Context, _ execir.CallSite, a string, _ map[string]any) (any, error) {
	return map[string]any{"agent": a}, nil
}
func (e *endToEndInvoker) InvokeWorkflow(_ context.Context, _ execir.CallSite, w string, _ map[string]any) (any, error) {
	return nil, nil
}
func (e *endToEndInvoker) InvokeApproval(_ context.Context, _ execir.CallSite, _ execir.ApprovalInfo, args map[string]any) (any, error) {
	return args, nil
}

// TestExec_ParseLowerExecute is the primary acceptance path: a control-flow
// .agent workflow parses, lowers to the execution IR, and executes — the
// conditional selects an arm and the loop iterates the runtime collection.
func TestExec_ParseLowerExecute(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: Job) {
    if input.enabled {
        github.enable()
    } else {
        github.disable()
    }
    for name in input.names {
        github.touch(v: name)
    }
    return input.enabled
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	inv := &endToEndInvoker{}
	interp := &execir.Interp{Invoker: inv}
	out, err := interp.Run(context.Background(), prog, map[string]any{
		"enabled": true,
		"names":   []any{"a", "b"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != true {
		t.Fatalf("expected return true, got %v", out)
	}
	want := []string{"tool.github.enable", "tool.github.touch:a", "tool.github.touch:b"}
	if len(inv.calls) != len(want) {
		t.Fatalf("call sequence mismatch: got %v want %v", inv.calls, want)
	}
	for i := range want {
		if inv.calls[i] != want[i] {
			t.Fatalf("call[%d]=%q want %q (all: %v)", i, inv.calls[i], want[i], inv.calls)
		}
	}
}

func lowerExecOrFatal(t *testing.T, src string, workflows map[string]bool) (*execir.Program, lang.Diagnostics) {
	t.Helper()
	f, diags := lang.Parse("test.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse errors: %v", diags)
	}
	if len(f.Decls) == 0 {
		t.Fatalf("no declarations parsed")
	}
	wd, ok := f.Decls[0].(*lang.WorkflowDecl)
	if !ok {
		t.Fatalf("first decl is not a workflow")
	}
	return LowerExec(wd, workflows)
}

// TestLowerExec_ApprovalLowersToApprovalNode proves the .agent `approval` step lowers to an
// execir.Approval node carrying its bind, description, and redactKeys (#440) — the same node the YAML
// `approval:` step produces, so execution (the HITL pause) is identical.
func TestLowerExec_ApprovalLowersToApprovalNode(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: any) {
    a = svc.prepare(x: input.y)
    approval gate {
        description "Review before publishing"
        redactKeys { "secret" "token" }
        with {
            note: input.note
            draft: a
        }
    }
    return a
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	var ap *execir.Approval
	for _, n := range prog.Body {
		if a, ok := n.(*execir.Approval); ok {
			ap = a
		}
	}
	if ap == nil {
		t.Fatalf("expected an execir.Approval node, got %#v", prog.Body)
	}
	if ap.Bind != "gate" || ap.Description != "Review before publishing" {
		t.Fatalf("approval node fields: %+v", ap)
	}
	if len(ap.RedactKeys) != 2 || ap.RedactKeys[0] != "secret" || ap.RedactKeys[1] != "token" {
		t.Fatalf("approval redactKeys: %v", ap.RedactKeys)
	}
	// The review payload must lower into Args (the same path a YAML approval's with: takes) — it is the
	// reviewed data, gates the Edit decision, and becomes the node output.
	if len(ap.Args) != 2 {
		t.Fatalf("approval payload not lowered into Args: %+v", ap.Args)
	}
	if _, ok := ap.Args["note"]; !ok {
		t.Fatalf("approval Args missing 'note': %+v", ap.Args)
	}
	if _, ok := ap.Args["draft"]; !ok {
		t.Fatalf("approval Args missing 'draft': %+v", ap.Args)
	}
}

// TestLowerFile_ApprovalProjectsToWorkflowStep proves the resource projection of the .agent `approval`
// step is a spec.WorkflowStep carrying an Approval value — what effect analysis / plan risk walk. The
// approval's Needs wire it after its predecessor, preserving the DAG position.
func TestLowerFile_ApprovalProjectsToWorkflowStep(t *testing.T) {
	t.Parallel()
	f, diags := lang.Parse("t.agent", `
workflow W(input: any) {
    a = svc.prepare(x: input.y)
    approval gate {
        description "Review"
        with {
            draft: a
        }
    }
    return a
}
`)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	res, ld := LowerFile(f, Options{})
	if ld.HasErrors() {
		t.Fatalf("lower: %v", ld)
	}
	wr := res.ToGraph().Workflows["W"]
	if wr == nil {
		t.Fatalf("no workflow W")
	}
	var step *spec.WorkflowStep
	for i := range wr.Spec.Steps {
		if wr.Spec.Steps[i].ID == "gate" {
			step = &wr.Spec.Steps[i]
		}
	}
	if step == nil {
		t.Fatalf("no approval step 'gate' in projection: %+v", wr.Spec.Steps)
	}
	if !spec.StepIsApproval(*step) {
		t.Fatalf("step 'gate' is not an approval: %+v", step)
	}
	if step.Approval.Config == nil || step.Approval.Config.Description != "Review" {
		t.Fatalf("approval config not projected: %+v", step.Approval)
	}
	if len(step.Needs) != 1 || step.Needs[0] != "a" {
		t.Fatalf("approval should depend on its predecessor 'a', got needs %v", step.Needs)
	}
	// The review payload projects into the step's With (the effect/plan surface), like a call's args.
	if step.With == nil || step.With["draft"] == nil {
		t.Fatalf("approval payload not projected into step.With: %+v", step.With)
	}
}

func TestLowerExec_IfLowersToBranch(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    if input.urgent {
        github.notify()
    } else {
        github.skip()
    }
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(prog.Body) != 1 {
		t.Fatalf("expected 1 top node, got %d", len(prog.Body))
	}
	br, ok := prog.Body[0].(*execir.Branch)
	if !ok {
		t.Fatalf("expected Branch, got %T", prog.Body[0])
	}
	if _, ok := br.Cond.(execir.Leaf); !ok {
		t.Fatalf("expected a Leaf condition, got %T", br.Cond)
	}
	if len(br.Then) != 1 || len(br.Else) != 1 {
		t.Fatalf("expected one node per arm, got then=%d else=%d", len(br.Then), len(br.Else))
	}
	if it, ok := br.Then[0].(*execir.InvokeTool); !ok || it.Uses != "tool.github.notify" {
		t.Fatalf("then arm should invoke tool.github.notify, got %#v", br.Then[0])
	}
}

func TestLowerExec_WhileLowersToWhile(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: State) -> State {
    state = input
    while !state.approved limit 3 {
        state = Reviewer(state)
    }
    return state
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	// Body: Let(state=input), While, Return.
	var w *execir.While
	for _, n := range prog.Body {
		if ws, ok := n.(*execir.While); ok {
			w = ws
		}
	}
	if w == nil {
		t.Fatalf("expected a While node in %#v", prog.Body)
	}
	if w.Limit != 3 {
		t.Fatalf("limit: got %d, want 3", w.Limit)
	}
	if _, ok := w.Cond.(execir.Not); !ok {
		t.Fatalf("expected a Not condition (!state.approved), got %T", w.Cond)
	}
	if len(w.Body) != 1 {
		t.Fatalf("body: got %d nodes, want 1", len(w.Body))
	}
	if ia, ok := w.Body[0].(*execir.InvokeAgent); !ok || ia.Agent != "Reviewer" || ia.Bind != "state" {
		t.Fatalf("body should rebind state = Reviewer(state), got %#v", w.Body[0])
	}
}

func TestLowerExec_ForAndParallelFor(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: Repos) {
    for repo in input.repos {
        github.build(repo)
    }
    parallel for item in input.items {
        github.scan(item)
    }
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(prog.Body) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(prog.Body))
	}
	seq, ok := prog.Body[0].(*execir.Loop)
	if !ok || seq.Parallel {
		t.Fatalf("first node should be a sequential Loop, got %#v", prog.Body[0])
	}
	if seq.Var != "repo" {
		t.Fatalf("loop var should be repo, got %q", seq.Var)
	}
	par, ok := prog.Body[1].(*execir.Loop)
	if !ok || !par.Parallel {
		t.Fatalf("second node should be a parallel Loop, got %#v", prog.Body[1])
	}
}

func TestLowerExec_ParallelLowersToFork(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    parallel {
        sec = Security(input)
        qual = Quality(input)
    }
    return sec
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	fork, ok := prog.Body[0].(*execir.Fork)
	if !ok {
		t.Fatalf("expected Fork, got %T", prog.Body[0])
	}
	if len(fork.Branches) != 2 {
		t.Fatalf("expected 2 fork branches, got %d", len(fork.Branches))
	}
	if fork.Branches[0].Bind != "sec" {
		t.Fatalf("first branch should bind sec, got %q", fork.Branches[0].Bind)
	}
}

func TestLowerExec_NestedCallHoisted(t *testing.T) {
	t.Parallel()
	// A nested call argument must be hoisted into its own preceding Invoke and
	// referenced by a temporary binding.
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    result = Synth(github.get_pr(input.repo))
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(prog.Body) != 2 {
		t.Fatalf("expected the nested call to be hoisted (2 nodes), got %d", len(prog.Body))
	}
	inner, ok := prog.Body[0].(*execir.InvokeTool)
	if !ok || inner.Uses != "tool.github.get_pr" {
		t.Fatalf("first node should be the hoisted tool call, got %#v", prog.Body[0])
	}
	outer, ok := prog.Body[1].(*execir.InvokeAgent)
	if !ok || outer.Bind != "result" {
		t.Fatalf("second node should bind result via the agent, got %#v", prog.Body[1])
	}
	ref, ok := outer.Args["arg0"].(execir.Ref)
	if !ok || len(ref.Path) != 1 || ref.Path[0] != inner.Bind {
		t.Fatalf("outer call should reference the hoisted temp %q, got %#v", inner.Bind, outer.Args["arg0"])
	}
}

func TestLowerExec_StringTemplateArg(t *testing.T) {
	t.Parallel()
	// A single whole-string `${...}` token lowers to a bare Ref; a string with
	// embedded tokens lowers to a Template whose Parts alternate literal/Ref.
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    review = Reviewer(input)
    github.post(whole: "${review.summary}", mixed: "## Review\n${review.summary}\ndone")
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	var post *execir.InvokeTool
	for _, n := range prog.Body {
		if it, ok := n.(*execir.InvokeTool); ok && it.Uses == "tool.github.post" {
			post = it
		}
	}
	if post == nil {
		t.Fatalf("expected a tool.github.post invocation in %#v", prog.Body)
	}
	ref, ok := post.Args["whole"].(execir.Ref)
	if !ok {
		t.Fatalf("whole arg should be a bare Ref, got %#v", post.Args["whole"])
	}
	if len(ref.Path) != 2 || ref.Path[0] != "review" || ref.Path[1] != "summary" {
		t.Fatalf("whole Ref path should be [review summary], got %v", ref.Path)
	}
	tmpl, ok := post.Args["mixed"].(execir.Template)
	if !ok {
		t.Fatalf("mixed arg should be a Template, got %#v", post.Args["mixed"])
	}
	// "## Review\n" , Ref(review.summary) , "\ndone"
	if len(tmpl.Parts) != 3 {
		t.Fatalf("expected 3 template parts, got %d: %#v", len(tmpl.Parts), tmpl.Parts)
	}
	if _, ok := tmpl.Parts[0].(execir.Lit); !ok {
		t.Fatalf("part 0 should be a Lit, got %#v", tmpl.Parts[0])
	}
	if r, ok := tmpl.Parts[1].(execir.Ref); !ok || len(r.Path) != 2 || r.Path[1] != "summary" {
		t.Fatalf("part 1 should be Ref(review.summary), got %#v", tmpl.Parts[1])
	}
	if _, ok := tmpl.Parts[2].(execir.Lit); !ok {
		t.Fatalf("part 2 should be a Lit, got %#v", tmpl.Parts[2])
	}
}

func TestLowerExec_WorkflowCalleeClassification(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    x = Sub(input)
}
`, map[string]bool{"Sub": true})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if _, ok := prog.Body[0].(*execir.InvokeWorkflow); !ok {
		t.Fatalf("Sub should classify as InvokeWorkflow, got %T", prog.Body[0])
	}
}

func TestLowerExec_ReturnInParallelForRejected(t *testing.T) {
	t.Parallel()
	_, diags := lowerExecOrFatal(t, `
workflow W(input: Batch) {
    parallel for item in input.items {
        return item
    }
}
`, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected a diagnostic for return inside a parallel loop body")
	}
}

func TestLowerExec_CallInConditionDiagnosed(t *testing.T) {
	t.Parallel()
	_, diags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    if github.check(input) {
        github.act()
    }
}
`, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected a call-in-condition diagnostic")
	}
}

// TestExec_ObjectReturn covers object-literal returns (#440): `return { a: …, b: … }` executes to the
// flat multi-field map, and scalar-value fields (literals, input refs) resolve.
func TestExec_ObjectReturn(t *testing.T) {
	t.Parallel()
	prog, diags := lowerExecOrFatal(t, `
workflow W(input: any) {
    return { product: input.x, subject: "hello", count: 3 }
}
`, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	interp := &execir.Interp{Invoker: &endToEndInvoker{}}
	out, err := interp.Run(context.Background(), prog, map[string]any{"x": "USB-C hub"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected object output, got %T: %v", out, out)
	}
	if m["product"] != "USB-C hub" || m["subject"] != "hello" {
		t.Fatalf("object output fields wrong: %v", m)
	}
	if len(m) != 3 {
		t.Fatalf("expected 3 fields, got %d: %v", len(m), m)
	}
}
