package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/project"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
)

// These tests run YAML subworkflow graphs through the PRODUCTION executables
// (project.BuildExecutables — the path config.ResolveGraph uses), where every
// YAML workflow has a lowered program with the conventional single parameter
// `input`. With Executables == nil the engine lowers on the fly and the tests
// would pass even if the runtime guessed call/return shapes from a program's
// parameter list or output keys (#551/#552 review).

func yamlWF(name string, steps []spec.WorkflowStep, output map[string]any) *spec.WorkflowResource {
	wf := &spec.WorkflowResource{
		APIVersion: spec.APIVersionV0, Kind: spec.KindWorkflow, Metadata: spec.Metadata{Name: name},
		Spec: spec.WorkflowSpec{Steps: steps},
	}
	if output != nil {
		wf.Spec.Output = &spec.WorkflowOutput{Value: output}
	}
	return wf
}

func yamlSubworkflowGraph(workflows ...*spec.WorkflowResource) *spec.ProjectGraph {
	g := &spec.ProjectGraph{
		Tools:     map[string]*spec.ToolResource{"echo": nativeTool("echo")},
		Policies:  map[string]*spec.PolicyResource{"default": {Spec: spec.PolicySpec{}}},
		Workflows: map[string]*spec.WorkflowResource{},
	}
	for _, wf := range workflows {
		g.Workflows[wf.Metadata.Name] = wf
	}
	return g
}

// runYAMLProd runs wf on g with production executables and returns the run and
// the executor (for run_steps inspection).
func runYAMLProd(t *testing.T, g *spec.ProjectGraph, wf string, input map[string]any) (*Executor, *state.Run) {
	t.Helper()
	ex, _, runID, started := newResumeExecutor(t, g, wf)
	ex.Executables = project.BuildExecutables(g)
	for name := range g.Workflows {
		if ex.Executables[name] == nil {
			t.Fatalf("BuildExecutables lowered no program for %q", name)
		}
	}
	err := ex.Run(context.Background(), RunInput{RunID: runID, WorkflowName: wf, Env: "dev", StartedAt: started, Input: input})
	run, gerr := ex.Store.GetRun(context.Background(), runID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if err != nil {
		t.Fatalf("run %s: %v (status %q, error %q)", wf, err, run.Status, run.ErrorText)
	}
	return ex, run
}

func runOutput(t *testing.T, run *state.Run) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(run.OutputJSON), &out); err != nil {
		t.Fatalf("output json: %v %s", err, run.OutputJSON)
	}
	return out
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runStepRow(t *testing.T, ex *Executor, runID, stepID string) state.RunStep {
	t.Helper()
	lister, ok := ex.Store.(interface {
		ListRunStepsByRunID(context.Context, string) ([]state.RunStep, error)
	})
	if !ok {
		t.Fatalf("store %T cannot list run steps", ex.Store)
	}
	steps, err := lister.ListRunStepsByRunID(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.StepID == stepID {
			return s
		}
	}
	t.Fatalf("no run_steps row %q", stepID)
	return state.RunStep{}
}

// echoChild is a YAML callee whose output uses the single-value key:
// output.value = {value: <echoed topic>}.
func echoChild() *spec.WorkflowResource {
	return yamlWF("child",
		[]spec.WorkflowStep{{ID: "echo", Uses: "tool.echo.echo", With: map[string]any{"msg": "${input.topic}"}}},
		map[string]any{"value": "${steps.echo.output.echo.msg}"})
}

// echoParent consumes the child's documented step output both through
// ${steps.call.output.value} (in a later step's with: and in its own output) and
// as the whole ${steps.call.output}.
func echoParent() *spec.WorkflowResource {
	return yamlWF("parent",
		[]spec.WorkflowStep{
			{ID: "call", Workflow: "child", With: map[string]any{"topic": "${input.topic}"}},
			{ID: "fin", Uses: "tool.echo.echo", With: map[string]any{"from": "${steps.call.output.value}"}},
		},
		map[string]any{
			"v":     "${steps.call.output.value}",
			"w":     "${steps.call.output}",
			"final": "${steps.fin.output.echo.from}",
		})
}

func echoTop() *spec.WorkflowResource {
	return yamlWF("top",
		[]spec.WorkflowStep{{ID: "p", Workflow: "parent", With: map[string]any{"topic": "${input.topic}"}}},
		map[string]any{"mv": "${steps.p.output.v}", "mw": "${steps.p.output.w}", "mfinal": "${steps.p.output.final}"})
}

// TestYAMLSubworkflow_outputValueRootAndNestedAgree is review finding 1: the
// step output of a YAML workflow: step is the callee's output.value document
// ({value: X}), so ${steps.call.output.value} is X — in a later step, in the
// caller's output, and identically whether the caller runs as root or nested.
// These are the values the merge base (main) produces.
func TestYAMLSubworkflow_outputValueRootAndNestedAgree(t *testing.T) {
	t.Parallel()
	g := yamlSubworkflowGraph(echoChild(), echoParent(), echoTop())

	ex, run := runYAMLProd(t, g, "parent", map[string]any{"topic": "hi"})
	if got, want := jsonOf(t, runOutput(t, run)), `{"final":"hi","v":"hi","w":{"value":"hi"}}`; got != want {
		t.Fatalf("root parent output %s, want %s", got, want)
	}
	if got := runStepRow(t, ex, run.RunID, "call").OutputJSON; got != `{"value":"hi"}` {
		t.Fatalf("run_steps call output %s, want the callee output document", got)
	}

	ex, run = runYAMLProd(t, g, "top", map[string]any{"topic": "hi"})
	if got, want := jsonOf(t, runOutput(t, run)), `{"mfinal":"hi","mv":"hi","mw":{"value":"hi"}}`; got != want {
		t.Fatalf("nested parent output (via top) %s, want %s — root and nested must agree", got, want)
	}
	if got := runStepRow(t, ex, run.RunID, "p/call").OutputJSON; got != `{"value":"hi"}` {
		t.Fatalf("run_steps p/call output %s, want the callee output document", got)
	}
	if got := runStepRow(t, ex, run.RunID, "p").OutputJSON; got != `{"final":"hi","v":"hi","w":{"value":"hi"}}` {
		t.Fatalf("run_steps p output %s, want the nested parent's output document", got)
	}
}

// TestYAMLSubworkflow_oneKeyWithIsTheDocument is review finding 2: a YAML
// `with:` map is the callee's whole input document whatever its keys, so a
// one-key `with: {input: …}` or `{arg0: …}` must not be unwrapped even though the
// callee's production program declares the single parameter `input`.
func TestYAMLSubworkflow_oneKeyWithIsTheDocument(t *testing.T) {
	t.Parallel()
	t.Run("input key", func(t *testing.T) {
		t.Parallel()
		child := yamlWF("child", nil, map[string]any{"got": "${input.input.x}"})
		parent := yamlWF("parent",
			[]spec.WorkflowStep{{ID: "c", Workflow: "child", With: map[string]any{"input": map[string]any{"x": "${input.topic}"}}}},
			map[string]any{"got": "${steps.c.output.got}"})
		ex, run := runYAMLProd(t, yamlSubworkflowGraph(child, parent), "parent", map[string]any{"topic": "hello"})
		if got := jsonOf(t, runOutput(t, run)); got != `{"got":"hello"}` {
			t.Fatalf("output %s, want {\"got\":\"hello\"}", got)
		}
		if got := runStepRow(t, ex, run.RunID, "c").InputJSON; got != `{"input":{"x":"hello"}}` {
			t.Fatalf("run_steps c input %s, want the with: document", got)
		}
	})
	t.Run("arg0 key", func(t *testing.T) {
		t.Parallel()
		child := yamlWF("child", nil, map[string]any{"got": "${input.arg0}"})
		parent := yamlWF("parent",
			[]spec.WorkflowStep{{ID: "c", Workflow: "child", With: map[string]any{"arg0": "${input.topic}"}}},
			map[string]any{"got": "${steps.c.output.got}"})
		_, run := runYAMLProd(t, yamlSubworkflowGraph(child, parent), "parent", map[string]any{"topic": "hello"})
		if got := jsonOf(t, runOutput(t, run)); got != `{"got":"hello"}` {
			t.Fatalf("output %s, want {\"got\":\"hello\"}", got)
		}
	})
	t.Run("input schema requires input", func(t *testing.T) {
		t.Parallel()
		child := yamlWF("child", nil, map[string]any{"got": "${input.input.x}"})
		child.Spec.Input = &spec.WorkflowInput{Schema: "./child-input.json"}
		parent := yamlWF("parent",
			[]spec.WorkflowStep{{ID: "c", Workflow: "child", With: map[string]any{"input": map[string]any{"x": "${input.topic}"}}}},
			map[string]any{"got": "${steps.c.output.got}"})
		g := yamlSubworkflowGraph(child, parent)
		ex, _, runID, started := newResumeExecutor(t, g, "parent")
		ex.Executables = project.BuildExecutables(g)
		ex.ProjectRoot = t.TempDir()
		schema := `{"type":"object","required":["input"],"properties":{"input":{"type":"object","required":["x"]}}}`
		if err := os.WriteFile(filepath.Join(ex.ProjectRoot, "child-input.json"), []byte(schema), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ex.Run(context.Background(), RunInput{RunID: runID, WorkflowName: "parent", Env: "dev", StartedAt: started, Input: map[string]any{"topic": "hello"}}); err != nil {
			t.Fatalf("run: %v", err)
		}
		run, _ := ex.Store.GetRun(context.Background(), runID)
		if got := jsonOf(t, runOutput(t, run)); got != `{"got":"hello"}` {
			t.Fatalf("output %s, want {\"got\":\"hello\"}", got)
		}
	})
}

// TestYAMLSubworkflow_resumeKeepsOutputValue suspends inside a YAML callee at a
// HITL gate (production executables), resumes, and asserts the caller still sees
// the documented step output, and that the gated effect ran exactly once.
func TestYAMLSubworkflow_resumeKeepsOutputValue(t *testing.T) {
	t.Parallel()
	child := echoChild()
	child.Spec.Policy = "gate"
	child.Spec.Steps = append([]spec.WorkflowStep{{ID: "pub", Uses: "tool.publisher.echo", With: map[string]any{"msg": "${input.topic}"}}}, child.Spec.Steps...)
	parent := echoParent()
	parent.Spec.Policy = "gate"
	top := echoTop()
	top.Spec.Policy = "gate"
	g := yamlSubworkflowGraph(child, parent, top)
	g.Tools["publisher"] = &spec.ToolResource{APIVersion: spec.APIVersionV0, Kind: spec.KindTool, Metadata: spec.Metadata{Name: "publisher"}, Spec: spec.ToolSpec{Type: "native", Safety: &spec.ToolSafety{SideEffects: spec.BoolPtr(true)}}}
	g.Policies["gate"] = &spec.PolicyResource{Spec: spec.PolicySpec{
		Approvals: &spec.PolicyApprovals{RequiredFor: []string{"tool.publisher.echo"}},
		Hitl: &spec.HitlPolicy{InterruptOn: map[string]spec.HitlInterruptValue{
			"publisher": {Enabled: true, Config: &spec.HitlInterruptConfig{AllowedDecisions: []spec.HitlDecisionKind{spec.HitlDecisionApprove, spec.HitlDecisionReject}}},
		}},
	}}

	for _, wf := range []string{"parent", "top"} {
		t.Run(wf, func(t *testing.T) {
			t.Parallel()
			ex, ct, runID, started := newResumeExecutor(t, g, wf)
			ex.Executables = project.BuildExecutables(g)
			ctx := context.Background()
			input := map[string]any{"topic": "hi"}
			if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: wf, Env: "dev", StartedAt: started, Input: input}); !errors.Is(err, ErrInterrupted) {
				t.Fatalf("fresh run should interrupt at the child gate, got %v", err)
			}
			if err := ex.Run(ctx, RunInput{
				RunID: runID, WorkflowName: wf, Env: "dev", StartedAt: started, Input: input,
				Resume: true, Hitl: HitlRunOptions{Actor: "alice", Decision: &policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: "alice"}},
			}); err != nil {
				t.Fatalf("resume: %v", err)
			}
			run, _ := ex.Store.GetRun(ctx, runID)
			if run.Status != state.RunStatusSucceeded {
				t.Fatalf("status %q err=%q", run.Status, run.ErrorText)
			}
			want := `{"final":"hi","v":"hi","w":{"value":"hi"}}`
			if wf == "top" {
				want = `{"mfinal":"hi","mv":"hi","mw":{"value":"hi"}}`
			}
			if got := jsonOf(t, runOutput(t, run)); got != want {
				t.Fatalf("resumed output %s, want %s", got, want)
			}
			if got := ct.count("tool.publisher.echo"); got != 1 {
				t.Fatalf("gated effect ran %d times, want exactly 1", got)
			}
		})
	}
}
