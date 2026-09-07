package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExport_OutputDirIsLoadable is the issue #507 acceptance, end to end through the CLI: the
// directory `export --output` writes must be a project `validate --project` accepts. Before the fix
// export wrote a project.yaml the ADR-007 loader rejects, so `validate` failed exit 2.
func TestExport_OutputDirIsLoadable(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "main.agent"), []byte(`
agent Reviewer {
    model openai/gpt-5
    output Report
}

workflow Run(input: Ticket) -> Report {
    r = Reviewer(input)
    return r
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "exported")

	// export --project src --output out
	ResetGlobalsForTest()
	exportCmd := NewRootCmd()
	var exportOut bytes.Buffer
	exportCmd.SetOut(&exportOut)
	exportCmd.SetErr(&exportOut)
	exportCmd.SetArgs([]string{"export", "--project", src, "--output", out})
	if err := exportCmd.Execute(); err != nil {
		t.Fatalf("export: %v\n%s", err, exportOut.String())
	}

	// It wrote a .agent project, not the project.yaml the loader now refuses.
	if _, err := os.Stat(filepath.Join(out, "project.agent")); err != nil {
		t.Fatalf("expected exported project.agent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "project.yaml")); err == nil {
		t.Fatal("export must not write a project.yaml the loader rejects")
	}

	// validate --project out must succeed (the regression this issue is about).
	ResetGlobalsForTest()
	validateCmd := NewRootCmd()
	var validateOut bytes.Buffer
	validateCmd.SetOut(&validateOut)
	validateCmd.SetErr(&validateOut)
	validateCmd.SetArgs([]string{"validate", "--project", out})
	if err := validateCmd.Execute(); err != nil {
		t.Fatalf("validate of the exported project failed (issue #507 regression): %v\n%s", err, validateOut.String())
	}
	if !strings.Contains(validateOut.String(), "Validation successful") {
		t.Fatalf("expected a successful validate, got: %s", validateOut.String())
	}
}
