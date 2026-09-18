package cli

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/trace"
)

func extractRunID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Run ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Run ID:"))
		}
	}
	return ""
}

func TestLogs_afterRun_showsStartedAndFinished(t *testing.T) {
	db := filepath.Join(t.TempDir(), "logs-run.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	var runOut bytes.Buffer
	runCmd := NewRootCmd()
	runCmd.SetOut(&runOut)
	runCmd.SetErr(&runOut)
	runCmd.SetArgs([]string{
		"run", "workflow/demo",
		"--project", root,
		"-e", "staging",
		"--state", db,
		"--input", "topic=logs-test",
	})
	if err := runCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	runID := extractRunID(runOut.String())
	if runID == "" {
		t.Fatalf("no run id in:\n%s", runOut.String())
	}

	ResetGlobalsForTest()
	var logOut bytes.Buffer
	logCmd := NewRootCmd()
	logCmd.SetOut(&logOut)
	logCmd.SetErr(&logOut)
	logCmd.SetArgs([]string{"logs", "--project", root, "--state", db, "--run", runID})
	if err := logCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := logOut.String()
	if !strings.Contains(s, string(trace.EventRunStarted)) {
		t.Fatalf("missing %s in:\n%s", trace.EventRunStarted, s)
	}
	if !strings.Contains(s, string(trace.EventRunFinished)) {
		t.Fatalf("missing %s in:\n%s", trace.EventRunFinished, s)
	}
}

// TestLogs_isReadOnly_doesNotPrune proves `terfyn logs` no longer deletes runs: a run backdated
// well beyond the project's retentionDays still exists after a logs invocation, so running logs to
// inspect an old (or paused) run can never delete it (#391).
func TestLogs_isReadOnly_doesNotPrune(t *testing.T) {
	db := filepath.Join(t.TempDir(), "logs-readonly.db")
	root := runProjRoot(t) // runproj sets retentionDays: 30

	ResetGlobalsForTest()
	var runOut bytes.Buffer
	runCmd := NewRootCmd()
	runCmd.SetOut(&runOut)
	runCmd.SetErr(&runOut)
	runCmd.SetArgs([]string{"run", "workflow/demo", "--project", root, "-e", "staging", "--state", db, "--input", "topic=x"})
	if err := runCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	runID := extractRunID(runOut.String())
	if runID == "" {
		t.Fatalf("no run id in:\n%s", runOut.String())
	}

	// Backdate the run far beyond retentionDays so the OLD logs behavior would have pruned it.
	ctx := context.Background()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE runs SET started_at = ? WHERE run_id = ?`, "2020-01-01T00:00:00.000000000Z", runID); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	ResetGlobalsForTest()
	var logOut bytes.Buffer
	logCmd := NewRootCmd()
	logCmd.SetOut(&logOut)
	logCmd.SetErr(&logOut)
	logCmd.SetArgs([]string{"logs", "--project", root, "--state", db, "--run", runID})
	if err := logCmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// The run must still be there after logs ran.
	st2, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.GetRun(ctx, runID); err != nil {
		t.Fatalf("terfyn logs pruned a backdated run (must be read-only): %v", err)
	}
}

func TestLogs_unknownRun_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "logs-none.db")
	root := runProjRoot(t)
	st, err := sqlite.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"logs", "--project", root, "--state", db, "--run", "does-not-exist"})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestLogs_runAndWorkflowMutuallyExclusive(t *testing.T) {
	db := filepath.Join(t.TempDir(), "logs-both.db")
	root := runProjRoot(t)

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"logs", "--project", root, "--state", db, "--run", "x", "--workflow", "y"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("exit=%d err=%v", ExitCodeOf(err), err)
	}
}
