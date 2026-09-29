package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/project"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

func writeAgentProject(t *testing.T, src string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.agent"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func runAgentWorkflow(t *testing.T, root, workflow string, input map[string]any) (*sqlite.Store, string, *state.Run) {
	t.Helper()
	graph, execs, err := project.LoadProjectWithExecutables(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "run.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runID := "run-sub-return"
	started := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	inJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartRun(ctx, state.Run{
		RunID: runID, WorkflowName: workflow, Env: "dev", Status: state.RunStatusRunning,
		StartedAt: started, InputJSON: string(inJSON),
	}); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{
		Graph: graph, ProjectRoot: root, Executables: execs,
		Tools: tools.NewRegistry(graph), Models: models.NewRegistry(graph),
		Store: st, Trace: trace.NewRecorder(st),
	}
	if err := ex.Run(ctx, RunInput{
		RunID: runID, WorkflowName: workflow, Env: "dev", StartedAt: started, Input: input,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.RunStatusSucceeded {
		t.Fatalf("status %q err=%q", got.Status, got.ErrorText)
	}
	return st, runID, got
}

func runOutputValue(t *testing.T, run *state.Run) any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(run.OutputJSON), &out); err != nil {
		t.Fatalf("output json: %v %s", err, run.OutputJSON)
	}
	return out["value"]
}

func subworkflowStepOutput(t *testing.T, st *sqlite.Store, runID, stepID string) map[string]any {
	t.Helper()
	steps, err := st.ListRunStepsByRunID(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range steps {
		if steps[i].StepID == stepID {
			var out map[string]any
			if steps[i].OutputJSON == "" {
				t.Fatalf("step %q has empty output_json", stepID)
			}
			if err := json.Unmarshal([]byte(steps[i].OutputJSON), &out); err != nil {
				t.Fatalf("step %q output: %v %s", stepID, err, steps[i].OutputJSON)
			}
			return out
		}
	}
	t.Fatalf("no run_steps row for %q", stepID)
	return nil
}

// TestExecIR_nestedIdentitySubworkflowReturn is issue #551: InvokeWorkflow must
// keep the child's RunResumable return value. Rebuilding from the retired
// resource projection interpolates the inert ${input} token and fails.
func TestExecIR_nestedIdentitySubworkflowReturn(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    return Identity(input)
}
`)
	st, runID, run := runAgentWorkflow(t, root, "Main", map[string]any{"x": "y"})
	got := runOutputValue(t, run)
	want, _ := json.Marshal(map[string]any{"x": "y"})
	raw, _ := json.Marshal(got)
	if string(raw) != string(want) {
		t.Fatalf("caller-visible value %s, want %s (output %s)", raw, want, run.OutputJSON)
	}
	stepOut := subworkflowStepOutput(t, st, runID, "_t0")
	if stepOut["value"] == nil {
		t.Fatalf("subworkflow run_steps.output_json missing value envelope: %+v", stepOut)
	}
	inner, _ := json.Marshal(stepOut["value"])
	if string(inner) != string(want) {
		t.Fatalf("stored subworkflow output %s, want value=%s", inner, want)
	}
}

func TestExecIR_twoLevelIdentitySubworkflowReturn(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Mid(input: Anything) -> Anything {
    return Identity(input)
}

workflow Main(input: Anything) -> Anything {
    return Mid(input)
}
`)
	_, _, run := runAgentWorkflow(t, root, "Main", map[string]any{"x": "y"})
	got := runOutputValue(t, run)
	want, _ := json.Marshal(map[string]any{"x": "y"})
	raw, _ := json.Marshal(got)
	if string(raw) != string(want) {
		t.Fatalf("two-level identity %s, want %s", raw, want)
	}
}

func TestExecIR_nestedBranchSubworkflowReturn(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Pick(input: Anything) -> Anything {
    if input.flag {
        return input.thenVal
    } else {
        return input.elseVal
    }
}

workflow Main(input: Anything) -> Anything {
    return Pick(input)
}
`)
	_, _, run := runAgentWorkflow(t, root, "Main", map[string]any{
		"flag": true, "thenVal": "taken", "elseVal": "other",
	})
	if runOutputValue(t, run) != "taken" {
		t.Fatalf("taken branch return %v, want taken", runOutputValue(t, run))
	}
	_, _, run2 := runAgentWorkflow(t, root, "Main", map[string]any{
		"flag": false, "thenVal": "taken", "elseVal": "other",
	})
	if runOutputValue(t, run2) != "other" {
		t.Fatalf("else branch return %v, want other", runOutputValue(t, run2))
	}
}

func TestExecIR_nestedObjectLiteralSubworkflowReturn(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Pack(input: Anything) -> Anything {
    return { x: input.x, y: "z" }
}

workflow Main(input: Anything) -> Anything {
    return Pack(input)
}
`)
	_, _, run := runAgentWorkflow(t, root, "Main", map[string]any{"x": "y"})
	got, _ := json.Marshal(runOutputValue(t, run))
	want, _ := json.Marshal(map[string]any{"x": "y", "y": "z"})
	if string(got) != string(want) {
		t.Fatalf("object literal return %s, want %s", got, want)
	}
}

func TestExecIR_nestedLoopCarriedSubworkflowReturn(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Last(input: Anything) -> Anything {
    last = input.start
    for item in input.items {
        last = item
    }
    return last
}

workflow Main(input: Anything) -> Anything {
    return Last(input)
}
`)
	_, _, run := runAgentWorkflow(t, root, "Main", map[string]any{
		"start": "zero",
		"items": []any{"a", "b", "c"},
	})
	if runOutputValue(t, run) != "c" {
		t.Fatalf("loop-carried return %v, want c", runOutputValue(t, run))
	}
}

// TestExecIR_objectLiteralReturnNamedValueParam is review finding 3's repro: a
// nested callee returning a multi-key object literal must not consult the inert
// resource projection (whose `${input}` token fails to interpolate, #551).
func TestExecIR_objectLiteralReturnNamedValueParam(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Pack(value: Anything) -> Anything {
    return { doc: value, tag: "t" }
}

workflow Main(input: Anything) -> Anything {
    return Pack(input)
}
`)
	_, _, run := runAgentWorkflow(t, root, "Main", map[string]any{"x": "y"})
	if got, want := jsonOf(t, runOutputValue(t, run)), `{"doc":{"x":"y"},"tag":"t"}`; got != want {
		t.Fatalf("object literal return %s, want %s", got, want)
	}
}

// TestExecIR_multiReturnObjectLiteralRootAndNested is review finding 3's root
// half: `if … {return {r: …}} else {return {s: …}}` must return the TAKEN arm's
// object at root (the flattened projection records only the last arm) and the
// same document when the workflow runs nested.
func TestExecIR_multiReturnObjectLiteralRootAndNested(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Pick(input: Anything) -> Anything {
    if input.flag {
        return { r: input.a }
    } else {
        return { s: input.b }
    }
}

workflow Main(input: Anything) -> Anything {
    p = Pick(input)
    return { nested: p }
}
`)
	for _, tc := range []struct {
		flag bool
		want string
	}{{true, `{"r":"A"}`}, {false, `{"s":"B"}`}} {
		in := map[string]any{"flag": tc.flag, "a": "A", "b": "B"}
		_, _, run := runAgentWorkflow(t, root, "Pick", in)
		if got := run.OutputJSON; got != tc.want {
			t.Fatalf("root Pick flag=%v output %s, want %s", tc.flag, got, tc.want)
		}
		st, runID, run := runAgentWorkflow(t, root, "Main", in)
		if got, want := run.OutputJSON, `{"nested":`+tc.want+`}`; got != want {
			t.Fatalf("nested Pick flag=%v output %s, want %s", tc.flag, got, want)
		}
		if got := jsonOf(t, subworkflowStepOutput(t, st, runID, "p")); got != tc.want {
			t.Fatalf("run_steps p output %s, want %s (root and nested share one output document)", got, tc.want)
		}
	}
}

// TestExecIR_mixedAndValueKeyReturns pins the output document for the shapes the
// resource projection cannot describe on its own: a workflow mixing a scalar
// return with an object-literal return, and an object literal whose only key is
// `value`. Whatever the callee's output envelope, a `.agent` caller binds the
// callee's RETURN VALUE.
func TestExecIR_mixedAndValueKeyReturns(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Mixed(input: Anything) -> Anything {
    if input.flag {
        return input.a
    } else {
        return { s: input.b }
    }
}

workflow ValueKey(input: Anything) -> Anything {
    return { value: input.a }
}

workflow Main(input: Anything) -> Anything {
    m = Mixed(input)
    v = ValueKey(input)
    return { m: m, v: v }
}
`)
	_, _, run := runAgentWorkflow(t, root, "Main", map[string]any{"flag": true, "a": "A", "b": "B"})
	if got, want := run.OutputJSON, `{"m":"A","v":{"value":"A"}}`; got != want {
		t.Fatalf("flag=true output %s, want %s", got, want)
	}
	_, _, run = runAgentWorkflow(t, root, "Main", map[string]any{"flag": false, "a": "A", "b": "B"})
	if got, want := run.OutputJSON, `{"m":{"s":"B"},"v":{"value":"A"}}`; got != want {
		t.Fatalf("flag=false output %s, want %s", got, want)
	}
}

// TestExecIR_scalarReturnBindingAndAuditRows: a `.agent` binding of a
// scalar-returning callee is the return value, while the step's persisted output
// stays the callee's output document {value: X} (the ONE runtime value a YAML
// consumer reads), and the run_steps input records the whole document the callee
// received — not the {"value": doc} argument wrapper (review finding 4).
func TestExecIR_scalarReturnBindingAndAuditRows(t *testing.T) {
	t.Parallel()
	root := writeAgentProject(t, `
workflow Identity(value: Anything) -> Anything {
    return value
}

workflow Main(input: Anything) -> Anything {
    r = Identity(input.doc)
    n = Identity(value: input.doc)
    return { got: r, named: n }
}
`)
	st, runID, run := runAgentWorkflow(t, root, "Main", map[string]any{"doc": "hello"})
	if got, want := run.OutputJSON, `{"got":"hello","named":"hello"}`; got != want {
		t.Fatalf("output %s, want %s", got, want)
	}
	steps, err := st.ListRunStepsByRunID(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range steps {
		if s.StepID != "r" && s.StepID != "n" {
			continue
		}
		seen++
		if s.InputJSON != `"hello"` {
			t.Errorf("run_steps %s input %s, want the whole document \"hello\"", s.StepID, s.InputJSON)
		}
		if s.OutputJSON != `{"value":"hello"}` {
			t.Errorf("run_steps %s output %s, want the callee output document", s.StepID, s.OutputJSON)
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of the r/n run_steps rows", seen)
	}
}

// agentGateSrc is a `.agent` project whose Identity callee runs a HITL-gated
// publisher before returning its whole input document, and whose Main binds the
// result and then runs a second gated publish AFTER the call — so a resume
// replays the completed call from the parent's memo and must re-apply the
// value projection to the memoized output document.
const agentGateSrc = `
tool helper {
    type native
    safety {
        sideEffects false
    }
}

tool publisher {
    type native
    safety {
        sideEffects true
    }
}

policy gate {
    approvals {
        requiredFor {
            tool.publisher.echo
        }
    }
    hitl {
        interruptOn {
            publisher {
                allowedDecisions { approve reject }
            }
        }
    }
}

workflow Identity(value: Anything) -> Anything policy gate {
    p = publisher.echo(msg: "inner")
    return value
}

workflow Main(input: Anything) -> Anything policy gate {
    r = Identity(input.doc)
    q = publisher.echo(msg: "outer")
    return { got: r }
}
`

// TestExecIR_agentSubworkflowResumeProductionExecutables suspends twice (inside
// the callee, then after the call in the parent) with production executables and
// asserts the resumed binding is the callee's return value for every JSON kind,
// that the parent's memo keeps the callee's output document (the shape main
// memoized, so a checkpoint written before this change replays the same), and
// that each gated effect runs exactly once.
func TestExecIR_agentSubworkflowResumeProductionExecutables(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  any
		want string
	}{
		{"string", "hello", `"hello"`},
		{"array", []any{"a", 2.0}, `["a",2]`},
		{"null", nil, `null`},
		{"object", map[string]any{"x": "y"}, `{"x":"y"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeAgentProject(t, agentGateSrc)
			graph, execs, err := project.LoadProjectWithExecutables(root)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			ex, _, runID, started := newResumeExecutor(t, graph, "Main")
			ex.Executables = execs
			ct := &countingTools{inner: tools.NewRegistry(graph)}
			ex.Tools = ct
			ctx := context.Background()
			input := map[string]any{"doc": tc.doc}
			approve := HitlRunOptions{Actor: "alice", Decision: &policy.HitlDecisionInput{Kind: spec.HitlDecisionApprove, Actor: "alice"}}

			if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "Main", Env: "dev", StartedAt: started, Input: input}); !errors.Is(err, ErrInterrupted) {
				t.Fatalf("fresh run should interrupt at the inner gate, got %v", err)
			}
			if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "Main", Env: "dev", StartedAt: started, Input: input, Resume: true, Hitl: approve}); !errors.Is(err, ErrInterrupted) {
				t.Fatalf("first resume should interrupt at the outer gate, got %v", err)
			}
			cp, err := ex.Store.GetLatestCheckpoint(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				ExecMemo map[string]json.RawMessage `json:"execMemo"`
			}
			if err := json.Unmarshal([]byte(cp.ContextJSON), &payload); err != nil {
				t.Fatal(err)
			}
			var memoR json.RawMessage
			for k, v := range payload.ExecMemo {
				if strings.HasPrefix(k, "r|") {
					memoR = v
				}
			}
			if got, want := string(memoR), `{"value":`+tc.want+`}`; got != want {
				t.Fatalf("parent memo for r = %s, want the callee output document %s (checkpoint %s)", got, want, cp.ContextJSON)
			}
			if err := ex.Run(ctx, RunInput{RunID: runID, WorkflowName: "Main", Env: "dev", StartedAt: started, Input: input, Resume: true, Hitl: approve}); err != nil {
				t.Fatalf("second resume: %v", err)
			}
			run, _ := ex.Store.GetRun(ctx, runID)
			if run.Status != state.RunStatusSucceeded {
				t.Fatalf("status %q err=%q", run.Status, run.ErrorText)
			}
			if got, want := run.OutputJSON, `{"got":`+tc.want+`}`; got != want {
				t.Fatalf("resumed output %s, want %s", got, want)
			}
			if got := ct.count("tool.publisher.echo"); got != 2 {
				t.Fatalf("gated publishes ran %d times, want exactly 2 (one inner, one outer)", got)
			}
		})
	}
}
