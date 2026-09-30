package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang/lower"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/project"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

// mainPinnedFixture is testdata/main_pinned_8741333.json: a project compiled by
// the MAIN binary at 8741333 (the released baseline) — its workflow resources,
// its serialized execution programs exactly as a deployment snapshot pins them,
// their digests, and what main's engine produced running each workflow (the run
// output and every run_steps row). The generator is
// testdata/main_pinned_8741333_gen.go.txt; it runs in a worktree at 8741333, so
// nothing here is produced by this branch's lowering or engine.
type mainPinnedFixture struct {
	Commit   string               `json:"commit"`
	Source   string               `json:"agentSource"`
	Graph    *spec.ProjectGraph   `json:"graph"`
	Programs json.RawMessage      `json:"programs"`
	Digests  map[string]string    `json:"digests"`
	Input    map[string]any       `json:"input"`
	Runs     map[string]pinnedRun `json:"runs"`
}

type pinnedRun struct {
	Status string                  `json:"status"`
	Error  string                  `json:"error,omitempty"`
	Output string                  `json:"output"`
	Steps  map[string]pinnedRunRow `json:"steps,omitempty"`
}

type pinnedRunRow struct {
	Status string `json:"status"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

func loadMainPinnedFixture(t *testing.T) mainPinnedFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "main_pinned_8741333.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx mainPinnedFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fx.Commit, "8741333") {
		t.Fatalf("fixture commit %q, want main 8741333", fx.Commit)
	}
	return fx
}

// runPinned runs wf on graph with the given programs as a pinned run (graph and
// programs hydrated from a snapshot, never re-lowered) and returns the run in
// the fixture's shape.
func runPinned(t *testing.T, graph *spec.ProjectGraph, progs map[string]*execir.Program, wf string, input map[string]any) pinnedRun {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "run.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runID := "run-" + wf
	started := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	inJSON, _ := json.Marshal(input)
	if err := st.StartRun(ctx, state.Run{RunID: runID, WorkflowName: wf, Env: "dev", Status: state.RunStatusRunning, StartedAt: started, InputJSON: string(inJSON)}); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{
		Graph: graph, ProjectRoot: t.TempDir(), Executables: progs, PinnedGraph: true,
		Tools: tools.NewRegistry(graph), Models: models.NewRegistry(graph),
		Store: st, Trace: trace.NewRecorder(st), Now: func() time.Time { return started },
	}
	_ = ex.Run(ctx, RunInput{RunID: runID, WorkflowName: wf, Env: "dev", StartedAt: started, Input: input})
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	out := pinnedRun{Status: run.Status, Error: run.ErrorText, Output: run.OutputJSON, Steps: map[string]pinnedRunRow{}}
	rows, err := st.ListRunStepsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		out.Steps[r.StepID] = pinnedRunRow{Status: r.Status, Input: r.InputJSON, Output: r.OutputJSON}
	}
	return out
}

// mainPinnedCallees maps each callee to the caller step that invoked it nested
// on main: main's nested run_steps output is the callee output document every
// caller consumed, so it is the output this binary must produce for the callee
// both nested and as a root run (#551: root and nested agree).
var mainPinnedCallees = map[string]struct{ caller, step string }{
	"P2":   {"CP2", "p"},   // lone `return {value: a}`, multi-parameter, called nested
	"PLit": {"CLit", "l"},  // lone `return {value: "lit"}`
	"Free": {"CFree", "f"}, // input-free callee
	"PMap": {"CMap", "m"},  // lone `return {value: {x: a}}` (resource value is a map)
	"YMap": {"YC", "m"},    // YAML output.value: {value: {map}}
}

// TestMainPinnedPrograms_keepMainOutputsAndBindings is the review #578
// compatibility contract: a deployment applied with main (8741333), invoked or
// resumed on this binary, gives every caller exactly what main gave it — the
// nested run_steps output of the callee, the caller's `.agent` binding of
// `<call>.value`, and a YAML consumer's `${steps.<id>.output.value}` — and a
// fresh compile of the same source on this binary agrees.
//
// The one deliberate difference is a ROOT run of a `.agent` workflow whose only
// return is a `{value: …}` literal: main's root path wrapped the return value in
// a second envelope (`{"value":{"value":"A"}}`) while the same callee called
// nested produced `{"value":"A"}` (#551). This binary produces the nested shape
// for both, for main-pinned and freshly compiled programs alike, so the root
// expectation below is main's nested output, and the test also pins that main's
// root output was the doubled one.
func TestMainPinnedPrograms_keepMainOutputsAndBindings(t *testing.T) {
	t.Parallel()
	fx := loadMainPinnedFixture(t)
	pinned, err := execir.UnmarshalPrograms(fx.Programs)
	if err != nil {
		t.Fatal(err)
	}

	// The pinned wire bytes decode and re-encode unchanged, and no main program
	// carries a bit this branch added.
	var compact bytes.Buffer
	if err := json.Compact(&compact, fx.Programs); err != nil {
		t.Fatal(err)
	}
	if again, err := execir.MarshalPrograms(pinned); err != nil || !bytes.Equal(again, compact.Bytes()) {
		t.Fatalf("main wire form does not round-trip unchanged (err %v)", err)
	}
	for name, p := range pinned {
		if p.Digest() != fx.Digests[name] {
			t.Errorf("pinned %s: digest %s, main recorded %s", name, p.Digest(), fx.Digests[name])
		}
	}

	names := make([]string, 0, len(fx.Runs))
	for name := range fx.Runs {
		names = append(names, name)
	}
	sort.Strings(names)

	check := func(label string, progs map[string]*execir.Program) {
		for _, name := range names {
			main := fx.Runs[name]
			if main.Status != state.RunStatusSucceeded {
				t.Fatalf("fixture: main run %s did not succeed: %s", name, main.Error)
			}
			got := runPinned(t, fx.Graph, progs, name, fx.Input)
			if got.Status != state.RunStatusSucceeded {
				t.Errorf("%s %s: status %q err=%q", label, name, got.Status, got.Error)
				continue
			}
			want := main.Output
			if c, ok := mainPinnedCallees[name]; ok {
				nested := fx.Runs[c.caller].Steps[c.step].Output
				want = nested
				if name != "YMap" && main.Output == nested {
					t.Errorf("fixture: main root output of %s (%s) was expected to differ from its nested output (#551)", name, main.Output)
				}
			}
			if got.Output != want {
				t.Errorf("%s %s: output %s, want %s (main binary: %s)", label, name, got.Output, want, main.Output)
			}
			for id, row := range main.Steps {
				g, ok := got.Steps[id]
				if !ok {
					t.Errorf("%s %s: no run_steps row %q", label, name, id)
					continue
				}
				if g != row {
					t.Errorf("%s %s: run_steps[%s] = %+v, main %+v", label, name, id, g, row)
				}
			}
		}
	}
	check("main-pinned", pinned)

	// A fresh compile of the same source on this binary: the `.agent` resources
	// are the ones main lowered, and a program's digest may differ from main's
	// only through a call bit the checker adds on an InvokeWorkflow node
	// (WholeDocument, ProjectValue) — never through its output shape, which is
	// not part of a program's identity. Every output and binding is the same as
	// the main-pinned run above.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.agent"), []byte(fx.Source), 0o644); err != nil {
		t.Fatal(err)
	}
	graph, fresh, err := project.LoadProjectWithExecutables(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, wf := range graph.Workflows {
		if a, b := jsonOf(t, wf), jsonOf(t, fx.Graph.Workflows[name]); a != b {
			t.Errorf("fresh resource %s differs from main's:\n%s\n%s", name, a, b)
		}
	}
	for name, wf := range fx.Graph.Workflows {
		if fresh[name] != nil {
			continue
		}
		prog, diags := lower.LowerWorkflowResource(wf) // the YAML workflows
		if derr := diags.AsError(); derr != nil {
			t.Fatal(derr)
		}
		fresh[name] = prog
	}
	for name, p := range fresh {
		if p.Digest() == fx.Digests[name] {
			continue
		}
		// Only a caller carrying a new call bit may move, and clearing those bits
		// must give back main's digest exactly.
		b, err := execir.MarshalPrograms(map[string]*execir.Program{name: p})
		if err != nil {
			t.Fatal(err)
		}
		cp, err := execir.UnmarshalPrograms(b)
		if err != nil {
			t.Fatal(err)
		}
		if !clearCallBits(cp[name].Body) {
			t.Errorf("fresh %s: digest %s differs from main's %s with no call bit (a return-shape or lowering change)", name, p.Digest(), fx.Digests[name])
		} else if got := cp[name].Digest(); got != fx.Digests[name] {
			t.Errorf("fresh %s: digest without call bits %s, want main's %s", name, got, fx.Digests[name])
		}
	}
	// In particular no callee's digest moves: PMap (`return {value: {x: a}}`),
	// P2, PLit and Free keep main's program bytes.
	for _, name := range []string{"P2", "PLit", "Free", "PMap"} {
		if got := fresh[name].Digest(); got != fx.Digests[name] {
			t.Errorf("fresh callee %s: digest %s, want main's %s", name, got, fx.Digests[name])
		}
	}
	check("fresh", fresh)
}

// clearCallBits clears WholeDocument and ProjectValue on every InvokeWorkflow
// reachable in nodes and reports whether any was set.
func clearCallBits(nodes []execir.Node) bool {
	found := false
	for _, n := range nodes {
		switch v := n.(type) {
		case *execir.InvokeWorkflow:
			if v.WholeDocument || v.ProjectValue {
				found = true
			}
			v.WholeDocument, v.ProjectValue = false, false
		case *execir.Branch:
			found = clearCallBits(v.Then) || found
			found = clearCallBits(v.Else) || found
		case *execir.Loop:
			found = clearCallBits(v.Body) || found
		case *execir.While:
			found = clearCallBits(v.Body) || found
		case *execir.Retry:
			found = clearCallBits(v.Body) || found
		case *execir.Fork:
			for _, br := range v.Branches {
				found = clearCallBits(br.Nodes) || found
			}
		case *execir.Graph:
			for _, gn := range v.Nodes {
				found = clearCallBits([]execir.Node{gn.Run}) || found
			}
		}
	}
	return found
}
