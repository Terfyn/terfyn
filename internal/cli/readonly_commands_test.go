package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogs_missingStateFileFailsWithoutCreatingDatabase(t *testing.T) {
	root := runProjRoot(t)
	statePath := filepath.Join(t.TempDir(), "missing", "state.db")
	if err := os.Mkdir(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"logs", "--project", root, "--state", statePath})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing state error")
	}
	if ExitCodeOf(err) == 0 {
		t.Fatalf("error=%v has zero exit code", err)
	}
	if !strings.Contains(err.Error(), "open sqlite") {
		t.Fatalf("error=%v, want state database open diagnostic", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("missing state database was created: stat error=%v", statErr)
	}
}

func TestAuditVerify_missingStateFileFailsWithoutCreatingDatabase(t *testing.T) {
	root := runProjRoot(t)
	statePath := filepath.Join(t.TempDir(), "missing", "state.db")
	if err := os.Mkdir(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}

	ResetGlobalsForTest()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"audit", "verify", "--project", root, "--state", statePath})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing state error")
	}
	if ExitCodeOf(err) == 0 {
		t.Fatalf("error=%v has zero exit code", err)
	}
	if !strings.Contains(err.Error(), "open sqlite") {
		t.Fatalf("error=%v, want state database open diagnostic", err)
	}
	if strings.Contains(out.String(), "OK") {
		t.Fatalf("missing state was reported as successfully verified:\n%s", out.String())
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("missing state database was created: stat error=%v", statErr)
	}
}
