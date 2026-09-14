package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/config"
	"github.com/Terfyn/terfyn/internal/plan"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/project"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
)

func runProjRoot(t *testing.T) string {
	t.Helper()
	return clearSnapshotRoots(t, filepath.Join("..", "runtime", "local", "testdata", "runproj"))
}

func runPolicyRoot(t *testing.T) string {
	t.Helper()
	return clearSnapshotRoots(t, filepath.Join("testdata", "run_policy"))
}

func clearSnapshotRoots(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(config.SnapshotPath(abs))
	_ = os.Remove(policy.SnapshotPath(abs))
	return abs
}

func runSafetyRoot(t *testing.T) string {
	t.Helper()
	return clearSnapshotRoots(t, filepath.Join("testdata", "run_safety"))
}

func runTwoGatesRoot(t *testing.T) string {
	t.Helper()
	return clearSnapshotRoots(t, filepath.Join("testdata", "run_two_gates"))
}

func runNestedGateRoot(t *testing.T) string {
	t.Helper()
	return clearSnapshotRoots(t, filepath.Join("testdata", "run_nested_gate"))
}

// TestRun_verbose_streamsEventsToStderr proves `terfyn run --verbose` streams trace events to
// stderr live (#450) while stdout stays clean for the run summary / -o json.
func TestRun_verbose_streamsEventsToStderr(t *testing.T) {
	db := filepath.Join(t.TempDir(), "verbose.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	var out, errb bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"run", "workflow/demo", "--project", root, "-e", "staging", "--state", db, "--input", "topic=x", "--no-color", "--verbose"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	stderr := errb.String()
	for _, want := range []string{"tool_selection", "tool_execution", "llm_completion"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing streamed %q:\n%s", want, stderr)
		}
	}
	// stdout carries only the run summary — the live stream must not leak into it.
	if strings.Contains(out.String(), "tool_execution") || strings.Contains(out.String(), "llm_completion") {
		t.Fatalf("streamed events leaked into stdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Status: succeeded") {
		t.Fatalf("stdout missing run summary:\n%s", out.String())
	}
}

// TestRun_withoutVerbose_noStream proves the stream is opt-in: no trace-event lines appear without
// --verbose.
func TestRun_withoutVerbose_noStream(t *testing.T) {
	db := filepath.Join(t.TempDir(), "quiet.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	var out, errb bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"run", "workflow/demo", "--project", root, "-e", "staging", "--state", db, "--input", "topic=x"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if strings.Contains(errb.String(), "tool_execution") || strings.Contains(errb.String(), "llm_completion") {
		t.Fatalf("events streamed without --verbose:\n%s", errb.String())
	}
}

func TestRun_demo_integration_succeeds(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-cli.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/demo",
		"--project", root,
		"-e", "staging",
		"--state", db,
		"--input", "topic=from-cli",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Run ID:") || !strings.Contains(s, "Status: succeeded") {
		t.Fatalf("unexpected output:\n%s", s)
	}
}

func TestRun_safetyOnly_interruptsAwaitingHitl(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-safety.db")
	root := runSafetyRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/echo",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "Status: interrupted") {
		t.Fatalf("expected interrupted:\n%s", out.String())
	}
}

func TestRun_safetyOnly_withApprove_succeeds(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-safety-ok.db")
	root := runSafetyRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/echo",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
		"--approve", "tool.helper.echo",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Status: succeeded") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRun_policyGated_interruptThenResumeApprove(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-pol.db")
	root := runPolicyRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/gated",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Status: interrupted") {
		t.Fatalf("expected interrupted:\n%s", out.String())
	}
	runID := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Run ID:") {
			runID = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Run ID:"))
		}
	}
	if runID == "" {
		t.Fatal("missing run id")
	}

	out.Reset()
	cmd2 := NewRootCmd()
	cmd2.SetOut(&out)
	cmd2.SetErr(&out)
	cmd2.SetArgs([]string{
		"run", "--resume", runID,
		"--project", root,
		"--state", db,
		"--decision", "approve",
	})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Status: succeeded") {
		t.Fatalf("expected succeeded:\n%s", out.String())
	}
}

// TestRun_decisionIsOneShot_secondGateReInterrupts proves a single --decision resolves exactly the
// gate that was presented and is NOT replayed to a later gate in the same run (issue #406). The
// workflow has two gated echo calls; resuming the first interrupt with --decision approve must land
// on the SECOND gate (still interrupted, with the resume hint) instead of auto-approving the rest of
// the run — otherwise --decision would be indistinguishable from --auto-approve.
func TestRun_decisionIsOneShot_secondGateReInterrupts(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-2gate.db")
	root := runTwoGatesRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/twogated",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Status: interrupted") {
		t.Fatalf("expected first gate to interrupt:\n%s", out.String())
	}
	runID := ""
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Run ID:") {
			runID = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Run ID:"))
		}
	}
	if runID == "" {
		t.Fatal("missing run id")
	}

	out.Reset()
	cmd2 := NewRootCmd()
	cmd2.SetOut(&out)
	cmd2.SetErr(&out)
	cmd2.SetArgs([]string{
		"run", "--resume", runID,
		"--project", root,
		"--state", db,
		"--decision", "approve",
	})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	s := out.String()
	// The one --decision approved the first gate; the second gate must re-interrupt, not be
	// auto-approved. A non-interactive resume exits 0 with the "Resume with:" hint.
	if !strings.Contains(s, "Status: interrupted") {
		t.Fatalf("expected second gate to re-interrupt (decision must be one-shot):\n%s", s)
	}
	if strings.Contains(s, "Status: succeeded") {
		t.Fatalf("single --decision was replayed to the second gate (run finished):\n%s", s)
	}
	if !strings.Contains(s, "--resume "+runID) {
		t.Fatalf("expected resume hint for the still-gated run:\n%s", s)
	}
}

// TestRun_nestedSubworkflowGate_interruptsCleanlyThenResumes proves that a gate
// inside a subworkflow (parent → child, gate in child) interrupts with exit 0,
// "Status: interrupted", and the resume hint — not exit 1 with "checkpoint has no
// pending approval gate" (issue #381). The engine stores the gate under the nested
// frame, so the CLI's checkpoint lookup must walk the nested chain. Resuming with
// --decision approve then drives the run to succeed.
func TestRun_nestedSubworkflowGate_interruptsCleanlyThenResumes(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-nested.db")
	root := runNestedGateRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/parent",
		"--project", root,
		"--state", db,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run of a nested-gate workflow must exit 0, got err=%v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "Status: interrupted") {
		t.Fatalf("expected nested gate to interrupt cleanly:\n%s", s)
	}
	if strings.Contains(s, "no pending approval gate") {
		t.Fatalf("CLI failed to find the gate inside the subworkflow (#381):\n%s", s)
	}
	runID := ""
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Run ID:") {
			runID = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Run ID:"))
		}
	}
	if runID == "" {
		t.Fatalf("missing run id:\n%s", s)
	}
	if !strings.Contains(s, "--resume "+runID) {
		t.Fatalf("expected resume hint for the interrupted nested-gate run:\n%s", s)
	}

	out.Reset()
	cmd2 := NewRootCmd()
	cmd2.SetOut(&out)
	cmd2.SetErr(&out)
	cmd2.SetArgs([]string{
		"run", "--resume", runID,
		"--project", root,
		"--state", db,
		"--decision", "approve",
	})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Status: succeeded") {
		t.Fatalf("expected nested-gate run to succeed after approve:\n%s", out.String())
	}
}

func TestRun_decisionWithoutResume_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-decision.db")
	root := runPolicyRoot(t)
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"run", "workflow/gated",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
		"--decision", "approve",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d want %d err=%v", ExitCodeOf(err), ExitValidationError, err)
	}
}

func TestRun_hitlRejectViaResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-reject.db")
	root := runPolicyRoot(t)
	runID := runPolicyInterrupted(t, root, db)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "--resume", runID,
		"--project", root,
		"--state", db,
		"--decision", "reject",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected rejection error")
	}
	if ExitCodeOf(err) != ExitExecutionError {
		t.Fatalf("exit=%d want %d err=%v", ExitCodeOf(err), ExitExecutionError, err)
	}
}

func TestRun_hitlEditViaResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-edit.db")
	root := runPolicyRoot(t)
	runID := runPolicyInterrupted(t, root, db)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "--resume", runID,
		"--project", root,
		"--state", db,
		"--decision", "edit",
		"--decision-edit-json", `{"topic":"edited"}`,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume edit: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Status: succeeded") {
		t.Fatalf("expected succeeded:\n%s", out.String())
	}
}

func runPolicyInterrupted(t *testing.T, root, db string) string {
	t.Helper()
	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/gated",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("interrupt run: %v\n%s", err, out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Run ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Run ID:"))
		}
	}
	t.Fatal("missing run id")
	return ""
}

func TestRun_withApprove_succeeds(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-ok.db")
	root := runPolicyRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/gated",
		"--project", root,
		"--state", db,
		"--input", "topic=x",
		"--approve", "tool.helper.echo",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Status: succeeded") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestRun_badWorkflowRef_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-bad.db")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"run", "demo", "--project", ".", "--state", db})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestRun_badInputPair_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-inp.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"run", "workflow/demo", "--project", root, "--state", db, "--input", "notakeyvalue"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestParseInputPair(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantKey   string
		wantValue string
		wantError bool
	}{
		{name: "value whitespace", input: "key=  value  ", wantKey: "key", wantValue: "  value  "},
		{name: "whitespace-only value", input: "key=   ", wantKey: "key", wantValue: "   "},
		{name: "value contains equals", input: "key=a=b", wantKey: "key", wantValue: "a=b"},
		{name: "key whitespace", input: "  key  =value", wantKey: "key", wantValue: "value"},
		{name: "empty value", input: "key=", wantError: true},
		{name: "empty key", input: "=value", wantError: true},
		{name: "missing delimiter", input: "key", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, err := parseInputPair(tt.input)
			if tt.wantError {
				if err == nil {
					t.Fatalf("parseInputPair(%q) succeeded with key=%q value=%q", tt.input, key, value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInputPair(%q): %v", tt.input, err)
			}
			if key != tt.wantKey || value != tt.wantValue {
				t.Fatalf("parseInputPair(%q) = key=%q value=%q, want key=%q value=%q", tt.input, key, value, tt.wantKey, tt.wantValue)
func TestRun_inputFile_succeeds(t *testing.T) {
	db := filepath.Join(t.TempDir(), "run-file.db")
	root := runProjRoot(t)
	f := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(f, []byte(`{"topic":"from-file"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "workflow/demo",
		"--project", root,
		"-e", "staging",
		"--state", db,
		"--input-file", f,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "succeeded") {
		t.Fatal(out.String())
	}
}

func TestBuildRunInputJSON_requiresObject(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		want      string
		wantError bool
	}{
		{name: "object", content: `{"topic":"from-file"}`, want: `{"topic":"from-file"}`},
		{name: "empty object", content: `{}`},
		{name: "null", content: `null`, wantError: true},
		{name: "array", content: `[]`, wantError: true},
		{name: "scalar", content: `42`, wantError: true},
		{name: "malformed", content: `{`, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(f, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := buildRunInputJSON(f, nil)
			if tt.wantError {
				if err == nil {
					t.Fatalf("buildRunInputJSON(%q) succeeded with %q", tt.content, got)
				}
				if !strings.Contains(err.Error(), "run: input-file must be a JSON object") {
					t.Fatalf("error = %v, want object validation error", err)
				}
				return
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRun_resume_missingRun_exit1(t *testing.T) {
	db := filepath.Join(t.TempDir(), "resume-missing.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "--resume", "does-not-exist",
		"--project", root,
		"--state", db,
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitGenericFailure {
		t.Fatalf("exit=%d err=%v out=%s", ExitCodeOf(err), err, out.String())
	}
}

func TestRun_resume_withWorkflowArg_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "resume-bad-args.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetArgs([]string{
		"run", "workflow/demo", "--resume", "some-id",
		"--project", root,
		"--state", db,
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestRun_resume_conflictingEnvironment_exit2(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "resume-env-conflict.db")
	root := runProjRoot(t)

	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	graph, err := project.LoadProject(root)
	if err != nil {
		t.Fatal(err)
	}
	spec.NormalizeProjectGraph(graph)
	graph, err = spec.ApplyEnvironment(graph, "staging")
	if err != nil {
		t.Fatal(err)
	}
	wf := graph.Workflows["demo"]
	wfHash, err := plan.WorkflowSpecHash(wf)
	if err != nil {
		t.Fatal(err)
	}

	runID := "cli-resume-env-conflict"
	started := time.Now().UTC().Add(-time.Hour)
	if err := st.StartRun(ctx, state.Run{
		RunID: runID, WorkflowName: "demo", Env: "dev", Status: state.RunStatusRunning,
		StartedAt: started, InputJSON: `{"topic":"env-conflict"}`, TotalCostUSD: 0,
		WorkflowSpecHash: wfHash, EnvironmentName: "staging",
	}); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"run", "--resume", runID,
		"--project", root,
		"-e", "prod",
		"--state", db,
	})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d want %d err=%v\n%s", ExitCodeOf(err), ExitValidationError, err, out.String())
	}
}

func TestClassifyRunError_maxCostDeniedExit5(t *testing.T) {
	d := &policy.DeniedError{
		Reason:  policy.ReasonMaxCost,
		Message: "policy: cost $0.0500 exceeds ceiling $0.0400",
		Extra:   map[string]any{"maxTotalCostUsd": 0.04, "accumulatedUsd": 0.05},
	}
	wrapped := fmt.Errorf("engine: step %q: %w", "act", d)
	got, ok := policy.AsDenied(wrapped)
	if !ok || got.Reason != policy.ReasonMaxCost {
		t.Fatalf("AsDenied=%v ok=%v (DeniedError must remain unwrapable)", got, ok)
	}
	if code := classifyRunError(wrapped); code != ExitPolicyDenied {
		t.Fatalf("classifyRunError=%d want %d", code, ExitPolicyDenied)
	}
	if ExitCodeOf(NewExitError(classifyRunError(wrapped), wrapped)) != ExitPolicyDenied {
		t.Fatalf("ExitCodeOf=%d want %d", ExitCodeOf(NewExitError(classifyRunError(wrapped), wrapped)), ExitPolicyDenied)
	}
}
