package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/project"
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
