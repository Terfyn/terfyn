package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestTest_wfFixture_passes(t *testing.T) {
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"test", "--project", testdataPath(t, "wf_tests")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "happy-path") || !strings.Contains(s, "missing-topic-is-null") {
		t.Fatalf("cases:\n%s", s)
	}
	if !strings.Contains(s, "2 passed, 0 failed") {
		t.Fatalf("summary:\n%s", s)
	}
}

func TestTest_wfFixture_json(t *testing.T) {
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"test", "-o", "json", "--project", testdataPath(t, "wf_tests")})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(out.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	if int(top["failed"].(float64)) != 0 {
		t.Fatalf("%v", top)
	}
	cases, _ := top["cases"].([]any)
	if len(cases) != 2 {
		t.Fatalf("%v", cases)
	}
}

func TestTest_filterWorkflow(t *testing.T) {
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"test", "workflow/demo", "--project", testdataPath(t, "wf_tests")})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 passed") {
		t.Fatalf("%s", out.String())
	}
}

func TestTest_unknownWorkflow_exit2(t *testing.T) {
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"test", "workflow/nope", "--project", testdataPath(t, "wf_tests")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("code=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestTest_noTestsDir_message(t *testing.T) {
	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"test", "--project", testdataPath(t, "validate_ok")})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No tests found") {
		t.Fatalf("%s", out.String())
	}
}
