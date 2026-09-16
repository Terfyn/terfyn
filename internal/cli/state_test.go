package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/apply"
	"github.com/Terfyn/terfyn/internal/plan"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
)

func TestStateList_emptyStore(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state-empty.db")
	st, err := sqlite.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "list", "--project", testdataPath(t, "validate_ok"), "--state", db})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "No applied resources") {
		t.Fatalf("got:\n%s", s)
	}
}

func TestStateList_afterApply_table(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	root = copyPlanFixture(t, root)
	db := filepath.Join(t.TempDir(), "state-apply.db")

	g := &Global{ProjectRoot: root}
	graph, _, err := prepareProjectGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.NewPlanner(st).ComputePlan(ctx, "local", graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.NewApplier(st).ApplyPlan(ctx, "local", graph, pl, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "list", "--project", root, "--state", db})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, needle := range []string{"Policy", "default", "Tool", "helper", "Project", "plan_project", "SPEC_HASH"} {
		if !strings.Contains(s, needle) {
			t.Fatalf("missing %q in:\n%s", needle, s)
		}
	}
	if !strings.Contains(s, "Applied project: plan_project") {
		t.Fatalf("missing applied project line:\n%s", s)
	}
}

func TestStateList_afterApply_json(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	root = copyPlanFixture(t, root)
	db := filepath.Join(t.TempDir(), "state-json.db")

	g := &Global{ProjectRoot: root}
	graph, _, err := prepareProjectGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.NewPlanner(st).ComputePlan(ctx, "local", graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.NewApplier(st).ApplyPlan(ctx, "local", graph, pl, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "list", "-o", "json", "--project", root, "--state", db})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(out.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	if top["environment"] != "local" {
		t.Fatalf("%v", top["environment"])
	}
	res, ok := top["resources"].([]any)
	if !ok || len(res) < 3 {
		t.Fatalf("resources: %v", top["resources"])
	}
	ap, ok := top["appliedProject"].(map[string]any)
	if !ok || ap["projectName"] != "plan_project" {
		t.Fatalf("appliedProject: %v", top["appliedProject"])
	}
}

func TestStateShow_afterApply(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	root = copyPlanFixture(t, root)
	db := filepath.Join(t.TempDir(), "state-show.db")

	g := &Global{ProjectRoot: root}
	graph, _, err := prepareProjectGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.NewPlanner(st).ComputePlan(ctx, "local", graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.NewApplier(st).ApplyPlan(ctx, "local", graph, pl, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "show", "Policy/default", "--project", root, "--state", db})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Kind:        Policy") || !strings.Contains(s, "Name:        default") {
		t.Fatalf("got:\n%s", s)
	}
	if !strings.Contains(s, "Spec hash:") {
		t.Fatalf("got:\n%s", s)
	}
	if !strings.Contains(s, "Normalized spec") {
		t.Fatalf("got:\n%s", s)
	}
}

func TestStateShow_json_fullSpec(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	root = copyPlanFixture(t, root)
	db := filepath.Join(t.TempDir(), "state-showj.db")

	g := &Global{ProjectRoot: root}
	graph, _, err := prepareProjectGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.NewPlanner(st).ComputePlan(ctx, "local", graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.NewApplier(st).ApplyPlan(ctx, "local", graph, pl, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "show", "-o", "json", "policy/default", "--project", root, "--state", db})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(out.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	res := top["resource"].(map[string]any)
	if res["kind"] != "Policy" {
		t.Fatalf("%v", res)
	}
	jsonStr, ok := res["normalizedSpecJson"].(string)
	if !ok || !strings.Contains(jsonStr, "maxTotalCostUsd") {
		t.Fatalf("normalizedSpecJson: %v", res["normalizedSpecJson"])
	}
}

func TestStateShow_notFound_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state-nope.db")
	st, err := sqlite.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"state", "show", "Agent/missing", "--project", testdataPath(t, "validate_ok"), "--state", db})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("code=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestStateShow_wrongArgCount_exit2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state-args.db")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"state", "show", "--project", testdataPath(t, "validate_ok"), "--state", db})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if ExitCodeOf(err) != ExitValidationError {
		t.Fatalf("code=%d err=%v", ExitCodeOf(err), err)
	}
}

func TestStateList_missingParentDir(t *testing.T) {
	parentDir := filepath.Join(t.TempDir(), "nested")
	db := filepath.Join(parentDir, "state.db")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "list", "--project", testdataPath(t, "validate_ok"), "--state", db})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing state database error, got nil")
	}
	if ExitCodeOf(err) == 0 {
		t.Fatalf("error=%v has zero exit code", err)
	}
	if !strings.Contains(err.Error(), "state: open sqlite") {
		t.Fatalf("error=%v, want 'state: open sqlite' diagnostic", err)
	}
	if _, statErr := os.Stat(parentDir); !os.IsNotExist(statErr) {
		t.Fatalf("parent directory was created: stat error=%v", statErr)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("state database file was created: stat error=%v", statErr)
	}
}

func TestStateList_missingDBFile(t *testing.T) {
	parentDir := t.TempDir()
	db := filepath.Join(parentDir, "state.db")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "list", "--project", testdataPath(t, "validate_ok"), "--state", db})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing state database error, got nil")
	}
	if ExitCodeOf(err) == 0 {
		t.Fatalf("error=%v has zero exit code", err)
	}
	if !strings.Contains(err.Error(), "state: open sqlite") {
		t.Fatalf("error=%v, want 'state: open sqlite' diagnostic", err)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("state database file was created: stat error=%v", statErr)
	}
}

func TestStateShow_missingParentDir(t *testing.T) {
	parentDir := filepath.Join(t.TempDir(), "nested")
	db := filepath.Join(parentDir, "state.db")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "show", "Agent/missing", "--project", testdataPath(t, "validate_ok"), "--state", db})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing state database error, got nil")
	}
	if ExitCodeOf(err) == 0 {
		t.Fatalf("error=%v has zero exit code", err)
	}
	if !strings.Contains(err.Error(), "state: open sqlite") {
		t.Fatalf("error=%v, want 'state: open sqlite' diagnostic", err)
	}
	if _, statErr := os.Stat(parentDir); !os.IsNotExist(statErr) {
		t.Fatalf("parent directory was created: stat error=%v", statErr)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("state database file was created: stat error=%v", statErr)
	}
}

func TestStateShow_missingDBFile(t *testing.T) {
	parentDir := t.TempDir()
	db := filepath.Join(parentDir, "state.db")

	ResetGlobalsForTest()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"state", "show", "Agent/missing", "--project", testdataPath(t, "validate_ok"), "--state", db})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected missing state database error, got nil")
	}
	if ExitCodeOf(err) == 0 {
		t.Fatalf("error=%v has zero exit code", err)
	}
	if !strings.Contains(err.Error(), "state: open sqlite") {
		t.Fatalf("error=%v, want 'state: open sqlite' diagnostic", err)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("state database file was created: stat error=%v", statErr)
	}
}

func TestStateInspection_doesNotAlterExistingDB(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	root = copyPlanFixture(t, root)
	db := filepath.Join(t.TempDir(), "state-inspect.db")

	g := &Global{ProjectRoot: root}
	graph, _, err := prepareProjectGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := plan.NewPlanner(st).ComputePlan(ctx, "local", graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.NewApplier(st).ApplyPlan(ctx, "local", graph, pl, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	beforeBytes, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(beforeBytes)

	ResetGlobalsForTest()
	cmdList := NewRootCmd()
	var listOut bytes.Buffer
	cmdList.SetOut(&listOut)
	cmdList.SetErr(&listOut)
	cmdList.SetArgs([]string{"state", "list", "--project", root, "--state", db})
	if err := cmdList.Execute(); err != nil {
		t.Fatalf("state list failed: %v", err)
	}
	if !strings.Contains(listOut.String(), "Policy") {
		t.Fatalf("unexpected state list output:\n%s", listOut.String())
	}

	ResetGlobalsForTest()
	cmdShow := NewRootCmd()
	var showOut bytes.Buffer
	cmdShow.SetOut(&showOut)
	cmdShow.SetErr(&showOut)
	cmdShow.SetArgs([]string{"state", "show", "Policy/default", "--project", root, "--state", db})
	if err := cmdShow.Execute(); err != nil {
		t.Fatalf("state show failed: %v", err)
	}
	if !strings.Contains(showOut.String(), "Policy") {
		t.Fatalf("unexpected state show output:\n%s", showOut.String())
	}

	afterBytes, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	afterHash := sha256.Sum256(afterBytes)

	if beforeHash != afterHash {
		t.Fatalf("database content altered by inspection: before=%x, after=%x", beforeHash, afterHash)
	}
}
