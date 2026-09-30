package project

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/schema"
	"github.com/Terfyn/terfyn/internal/spec"
)

// canonicalGraph renders the identity-bearing content of a graph (resource
// envelopes by name, plus project metadata/spec) as JSON. Source positions and
// other `json:"-"` diagnostic fields are excluded, matching ADR 003's rule that
// positions are never identity.
func canonicalGraph(t *testing.T, g *spec.ProjectGraph) string {
	t.Helper()
	type view struct {
		Meta         spec.Metadata
		Spec         spec.ProjectSpec
		Agents       map[string]*spec.AgentResource
		Tools        map[string]*spec.ToolResource
		Workflows    map[string]*spec.WorkflowResource
		Policies     map[string]*spec.PolicyResource
		Environments map[string]*spec.EnvironmentResource
	}
	v := view{
		Meta: g.Meta, Spec: g.Spec,
		Agents: g.Agents, Tools: g.Tools, Workflows: g.Workflows,
		Policies: g.Policies, Environments: g.Environments,
	}
	// Imports are a loading detail (auto-discovered from the .agent sources on
	// disk), not identity — exclude them from the comparison.
	v.Spec.Imports = nil
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExport_RoundTripsThroughLoader is the issue #507 acceptance criterion: the exported directory
// is a real, loadable project. It reloads through the USER-FACING LoadProject (not the codec-only
// LoadYAMLResources), so a regression that makes the output non-loadable can no longer hide — and it
// asserts an equivalent graph, modulo the project name, which .agent cannot author (ADR 007).
func TestExport_RoundTripsThroughLoader(t *testing.T) {
	root := t.TempDir()
	// A realistic fixture: a declared tool, two tool calls, an agent step, a multi-step dependency
	// chain with interpolated args, a policy + effects clause, and an object return — the constructs
	// most likely to expose a raise -> print -> parse -> lower gap through the user-facing loader. It
	// is deliberately a linear chain: raise linearizes the step DAG and emits no `parallel` block, so a
	// parallel workflow does NOT round-trip its concurrency (it comes back as a chain) — a known,
	// documented raise/migrate limitation covered separately by TestExport_ParallelStepsSerialize.
	writeFile(t, root, "src/pr.agent", `
tool github {
    type native
    safety {
        trusted true
        sideEffects false
    }
    operations {
        get_pr { effects { github.read } }
        post_comment { effects { github.write external.visible } }
    }
}

policy guarded {
    effects {
        permit { github.read github.write external.visible }
    }
}

agent Reviewer {
    model openai/gpt-5
    grants {
        tool.github.get_pr
    }
}

workflow Review(input: PullRequest) -> Report policy guarded
    effects { github.read github.write external.visible }
{
    pr = github.get_pr(input)
    review = Reviewer(pr)
    github.post_comment(review)
    return review
}
`)

	g1, err := LoadProject(root)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	out := filepath.Join(t.TempDir(), "exported")
	if err := WriteAgentProjectDir(out, g1); err != nil {
		t.Fatalf("export: %v", err)
	}

	g2, err := LoadProject(out)
	if err != nil {
		t.Fatalf("reload exported project through the user-facing loader: %v", err)
	}

	// The project name is not identity under ADR 007 (a reloaded .agent project is named after its
	// directory), so align it before comparing the resource content.
	g2.Meta.Name = g1.Meta.Name
	if a, b := canonicalGraph(t, g1), canonicalGraph(t, g2); a != b {
		t.Fatalf("graph changed across export round-trip:\n before: %s\n after:  %s", a, b)
	}
}

// TestExport_ParallelStepsSerialize pins a KNOWN limitation the export inherits from raise/migrate:
// raise linearizes the step DAG and emits no `parallel` block, so a workflow's parallel steps come
// back from export → reload as a sequential chain — the concurrency is lost, not preserved and not
// refused. This is why TestExport_RoundTripsThroughLoader uses a linear fixture. The test documents
// the behavior so a future fix (raise emitting `parallel`) flips it deliberately, not by accident.
func TestExport_ParallelStepsSerialize(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.agent", `
agent A { model openai/gpt-5 }
agent B { model openai/gpt-5 }

workflow Fan(input: Thing) -> Thing {
    parallel {
        a = A(input)
        b = B(input)
    }
    return a
}
`)
	g1, err := LoadProject(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// In the source both parallel steps fork from the workflow input — neither depends on the other.
	if got := stepNeeds(t, g1, "Fan", "b"); len(got) != 0 {
		t.Fatalf("precondition: source step b should have no needs (parallel with a), got %v", got)
	}

	out := filepath.Join(t.TempDir(), "exported")
	if err := WriteAgentProjectDir(out, g1); err != nil {
		t.Fatalf("export: %v", err)
	}
	g2, err := LoadProject(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// After the round-trip the parallel block is gone: b now depends on a (a sequential chain), so the
	// two steps no longer run concurrently. If this ever changes to preserve `parallel`, update the
	// docs/CHANGELOG that call the limitation out, and revisit the linear round-trip fixture.
	if got := stepNeeds(t, g2, "Fan", "b"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("expected reloaded step b to be serialized after a (concurrency lost), got needs %v", got)
	}
}

// stepNeeds returns the declared needs of a workflow step by id, failing the test if absent.
func stepNeeds(t *testing.T, g *spec.ProjectGraph, workflow, stepID string) []string {
	t.Helper()
	wf := g.Workflows[workflow]
	if wf == nil {
		t.Fatalf("workflow %q not found", workflow)
	}
	for _, s := range wf.Spec.Steps {
		if s.ID == stepID {
			return s.Needs
		}
	}
	t.Fatalf("step %q not found in workflow %q", stepID, workflow)
	return nil
}

// TestWriteAgentProjectDir_ReExportSmallerGraphLeavesNoLeftovers proves the output directory is a
// closed set: re-exporting a smaller graph into a directory a larger graph was exported to overwrites
// the consolidated project.agent, so an orphaned resource cannot reload. Re-exporting into a dir this
// function already wrote is allowed (its own project.agent is not "foreign").
func TestWriteAgentProjectDir_ReExportSmallerGraphLeavesNoLeftovers(t *testing.T) {
	base := &spec.ProjectGraph{
		Meta: spec.Metadata{Name: "demo"},
		Agents: map[string]*spec.AgentResource{
			"A": {APIVersion: spec.APIVersionV0, Kind: spec.KindAgent, Metadata: spec.Metadata{Name: "A"}, Spec: spec.AgentSpec{Model: "openai/gpt-5"}},
			"B": {APIVersion: spec.APIVersionV0, Kind: spec.KindAgent, Metadata: spec.Metadata{Name: "B"}, Spec: spec.AgentSpec{Model: "openai/gpt-5"}},
		},
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := WriteAgentProjectDir(out, base); err != nil {
		t.Fatalf("first export: %v", err)
	}

	smaller := &spec.ProjectGraph{
		Meta:   spec.Metadata{Name: "demo"},
		Agents: map[string]*spec.AgentResource{"A": base.Agents["A"]},
	}
	if err := WriteAgentProjectDir(out, smaller); err != nil {
		t.Fatalf("re-export: %v", err)
	}

	g, err := LoadProject(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := g.Agents["B"]; ok {
		t.Fatalf("agent B leaked across a re-export of a smaller graph")
	}
	if _, ok := g.Agents["A"]; !ok {
		t.Fatalf("agent A missing after re-export")
	}
}

// TestWriteAgentProjectDir_WritesSchemasAndTypedRoundTrips proves the exported .agent project keeps
// typed I/O: the schemas the source resolved are re-emitted under DIR/schemas/, so the reloaded
// project resolves the same types instead of silently degrading to `any` (gradual typing).
func TestWriteAgentProjectDir_WritesSchemasAndTypedRoundTrips(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.agent", `
agent Reviewer {
    model openai/gpt-5
    input Ticket
    output Handoff
}

workflow Run(input: Ticket) -> Handoff {
    r = Reviewer(input)
    return r
}
`)
	writeFile(t, root, "schemas/Ticket.json", `{"type":"object","properties":{"id":{"type":"string"}}}`)
	writeFile(t, root, "schemas/Handoff.json", `{"type":"object","properties":{"summary":{"type":"string"}}}`)

	g1, err := LoadProject(root)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	// Precondition: the source resolved the agent's types, so there is something to preserve.
	if in := g1.Agents["Reviewer"].Spec.Input; in == nil || in.Resolved == nil {
		t.Fatalf("precondition: Reviewer input should have resolved; got %+v", in)
	}

	out := filepath.Join(t.TempDir(), "exported")
	if err := WriteAgentProjectDir(out, g1); err != nil {
		t.Fatalf("export: %v", err)
	}

	// The schema files the type refs resolve against were written under the export dir.
	for _, ref := range []string{"schemas/Ticket.json", "schemas/Handoff.json"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(ref))); err != nil {
			t.Fatalf("expected exported %s: %v", ref, err)
		}
	}

	g2, err := LoadProject(out)
	if err != nil {
		t.Fatalf("reload exported project: %v", err)
	}
	in := g2.Agents["Reviewer"].Spec.Input
	if in == nil || in.Schema != "schemas/Ticket.json" || in.Resolved == nil {
		t.Fatalf("reloaded Reviewer input lost its type: %+v", in)
	}
	if out := g2.Agents["Reviewer"].Spec.Output; out == nil || out.Schema != "schemas/Handoff.json" || out.Resolved == nil {
		t.Fatalf("reloaded Reviewer output lost its type: %+v", out)
	}
}

// TestWriteAgentProjectDir_BooleanRootSchemasRoundTrip is the issue #549 review regression: a project
// typed with Draft 2020-12 boolean root schemas (`true` = accepts everything, `false` = accepts
// nothing) must keep them across export and reload. Raw == nil used to mean both "unresolved" and
// "boolean", so export silently omitted schemas/Any.json and schemas/Never.json and the reloaded
// project either failed to resolve the type or degraded `false` to untyped (accept-everything)
// gradual typing.
func TestWriteAgentProjectDir_BooleanRootSchemasRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.agent", `
agent Anything {
    model openai/gpt-5
    input Any
    output Any
}

agent Impossible {
    model openai/gpt-5
    input Any
    output Never
}

workflow Run(input: Any) -> Any {
    r = Anything(input)
    return r
}
`)
	writeFile(t, root, "schemas/Any.json", "true\n")
	writeFile(t, root, "schemas/Never.json", "false\n")

	g1, err := LoadProject(root)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	// Both boolean forms are collected as resolved schemas (not skipped as "unresolved").
	got := collectResolvedSchemas(g1)
	if v, ok := got["schemas/Any.json"]; !ok || v != true {
		t.Fatalf("collectResolvedSchemas[Any] = %#v, %v; want true", v, ok)
	}
	if v, ok := got["schemas/Never.json"]; !ok || v != false {
		t.Fatalf("collectResolvedSchemas[Never] = %#v, %v; want false", v, ok)
	}

	check := func(label string, g *spec.ProjectGraph) {
		t.Helper()
		anyIn := g.Agents["Impossible"].Spec.Input
		if anyIn == nil || anyIn.Schema != "schemas/Any.json" || anyIn.Resolved == nil {
			t.Fatalf("%s: Any input lost its type: %+v", label, anyIn)
		}
		if raw, ok := anyIn.Resolved.Schema(); !ok || raw != true {
			t.Fatalf("%s: Any schema = %#v, %v; want true", label, raw, ok)
		}
		if res := anyIn.Resolved.Lookup(nil); res.Impossible || res.Missing {
			t.Fatalf("%s: true schema must stay unconstrained, got %+v", label, res)
		}
		neverOut := g.Agents["Impossible"].Spec.Output
		if neverOut == nil || neverOut.Schema != "schemas/Never.json" || neverOut.Resolved == nil {
			t.Fatalf("%s: Never output lost its type: %+v", label, neverOut)
		}
		if raw, ok := neverOut.Resolved.Schema(); !ok || raw != false {
			t.Fatalf("%s: Never schema = %#v, %v; want false", label, raw, ok)
		}
		if res := neverOut.Resolved.Lookup(nil); !res.Impossible {
			t.Fatalf("%s: false schema must stay impossible (never), not degrade to untyped: %+v", label, res)
		}
		if in := g.Workflows["Run"].Spec.Input; in == nil || in.Schema != "schemas/Any.json" || in.Resolved == nil {
			t.Fatalf("%s: workflow input lost its type: %+v", label, in)
		} else if raw, ok := in.Resolved.Schema(); !ok || raw != true {
			t.Fatalf("%s: workflow Any schema = %#v, %v; want true", label, raw, ok)
		}
	}
	check("source", g1)

	out := filepath.Join(t.TempDir(), "exported")
	if err := WriteAgentProjectDir(out, g1); err != nil {
		t.Fatalf("export: %v", err)
	}
	for ref, want := range map[string]string{"schemas/Any.json": "true\n", "schemas/Never.json": "false\n"} {
		body, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(ref)))
		if err != nil {
			t.Fatalf("exported %s missing (boolean schema dropped as unresolved): %v", ref, err)
		}
		if string(body) != want {
			t.Fatalf("exported %s = %q, want %q", ref, body, want)
		}
	}

	// The exported false schema still rejects every instance; true still accepts them.
	neverPath := filepath.Join(out, "schemas", "Never.json")
	for _, inst := range []string{`null`, `{}`, `[]`, `0`, `""`, `false`} {
		if err := schema.Validate(neverPath, []byte(inst)); err == nil {
			t.Fatalf("exported false schema accepted instance %s", inst)
		}
	}
	if err := schema.Validate(filepath.Join(out, "schemas", "Any.json"), []byte(`{"x":1}`)); err != nil {
		t.Fatalf("exported true schema rejected an instance: %v", err)
	}

	g2, err := LoadProject(out)
	if err != nil {
		t.Fatalf("reload exported project: %v", err)
	}
	check("reloaded", g2)

	// Re-exporting the reloaded project is a fixed point (the form survives the whole lifecycle).
	out2 := filepath.Join(t.TempDir(), "exported2")
	if err := WriteAgentProjectDir(out2, g2); err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if before, after := snapshotExportDir(t, out), snapshotExportDir(t, out2); !equalSnapshots(before, after) {
		t.Fatalf("re-export of reloaded boolean-schema project differs: %v vs %v", keysOf(before), keysOf(after))
	}
}

func equalSnapshots(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || string(v) != string(w) {
			return false
		}
	}
	return true
}

// TestWriteAgentProjectDir_RefusesForeignAgentSource proves export refuses a directory that already
// holds a foreign .agent file (LoadProject would merge it and duplicate every resource).
func TestWriteAgentProjectDir_RefusesForeignAgentSource(t *testing.T) {
	out := t.TempDir()
	writeFile(t, out, "existing.agent", "workflow W() { return }")
	g := &spec.ProjectGraph{Meta: spec.Metadata{Name: "demo"}}
	err := WriteAgentProjectDir(out, g)
	if err == nil {
		t.Fatalf("expected export into a .agent-containing directory to be refused")
	}
	if !strings.Contains(err.Error(), ".agent") {
		t.Fatalf("expected the refusal to mention .agent sources, got: %v", err)
	}
}

// TestExportYAML_Deterministic proves the stream form is byte-stable, so a
// re-export of an unchanged graph produces no diff.
func TestExportYAML_Deterministic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "project.yaml", minimalProjectYAML)
	writeFile(t, root, "a.agent", `
agent A { model openai/gpt-5 }
agent B { model openai/gpt-5 }
workflow W(input: X) { return input.x }
`)
	g, _, err := LoadYAMLResources(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ExportYAML(g)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExportYAML(g)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("ExportYAML is not deterministic")
	}
}

func typedExportGraph() *spec.ProjectGraph {
	doc := &schema.Document{Raw: map[string]any{"type": "object"}}
	return &spec.ProjectGraph{
		Meta: spec.Metadata{Name: "demo"},
		Agents: map[string]*spec.AgentResource{"A": {
			APIVersion: spec.APIVersionV0, Kind: spec.KindAgent,
			Metadata: spec.Metadata{Name: "A"},
			Spec: spec.AgentSpec{Model: "openai/gpt-5", Input: &spec.AgentIO{
				Schema:   "schemas/T.json",
				Resolved: doc,
			}},
		}},
	}
}

func snapshotExportDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertExportUnchanged(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	after := snapshotExportDir(t, dir)
	if len(after) != len(before) {
		t.Fatalf("export tree changed: before %d files, after %d files (%v vs %v)", len(before), len(after), keysOf(before), keysOf(after))
	}
	for rel, want := range before {
		got, ok := after[rel]
		if !ok {
			t.Fatalf("failed re-export deleted %s", rel)
		}
		if string(got) != string(want) {
			t.Fatalf("failed re-export changed %s", rel)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func restoreExportHooks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		exportMarshalIndent = json.MarshalIndent
		exportMkdirAll = os.MkdirAll
		exportWriteFile = os.WriteFile
		exportRename = os.Rename
		exportCommitAgent = writeFileAtomic
	})
}

func successfulTypedExport(t *testing.T) (out string, g *spec.ProjectGraph, before map[string][]byte) {
	t.Helper()
	g = typedExportGraph()
	out = filepath.Join(t.TempDir(), "out")
	if err := WriteAgentProjectDir(out, g); err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(out, "schemas", "T.json")
	if _, err := os.ReadFile(schemaPath); err != nil {
		t.Fatalf("first export missing schema: %v", err)
	}
	return out, g, snapshotExportDir(t, out)
}

// TestFailedReExportPreservesPreviousSchemas is the issue #561 repro: a failed re-export
// (unencodable schema value) must not delete or mutate the previous successful schemas/.
func TestFailedReExportPreservesPreviousSchemas(t *testing.T) {
	out, g, before := successfulTypedExport(t)
	g.Agents["A"].Spec.Input.Resolved.Raw.(map[string]any)["unencodable"] = make(chan int)
	if err := WriteAgentProjectDir(out, g); err == nil {
		t.Fatal("unexpected success")
	}
	assertExportUnchanged(t, out, before)
}

func TestFailedReExportPreservesPreviousSchemas_Mkdir(t *testing.T) {
	restoreExportHooks(t)
	out, g, before := successfulTypedExport(t)
	exportMkdirAll = func(path string, perm os.FileMode) error {
		if filepath.Base(path) == exportSchemasDir {
			return errors.New("injected mkdir failure")
		}
		return os.MkdirAll(path, perm)
	}
	if err := WriteAgentProjectDir(out, g); err == nil {
		t.Fatal("unexpected success")
	}
	assertExportUnchanged(t, out, before)
}

func TestFailedReExportPreservesPreviousSchemas_SchemaWrite(t *testing.T) {
	restoreExportHooks(t)
	out, g, before := successfulTypedExport(t)
	exportWriteFile = func(name string, data []byte, perm os.FileMode) error {
		return errors.New("injected schema write failure")
	}
	if err := WriteAgentProjectDir(out, g); err == nil {
		t.Fatal("unexpected success")
	}
	assertExportUnchanged(t, out, before)
}

func TestFailedReExportPreservesPreviousSchemas_SourceCommit(t *testing.T) {
	restoreExportHooks(t)
	out, g, before := successfulTypedExport(t)
	exportCommitAgent = func(target string, data []byte) error {
		return errors.New("injected source commit failure")
	}
	if err := WriteAgentProjectDir(out, g); err == nil {
		t.Fatal("unexpected success")
	}
	assertExportUnchanged(t, out, before)
}

// TestWriteAgentProjectDir_ReExportDropsOrphanedSchemas proves a successful smaller re-export
// still replaces schemas/ so an unused type file cannot reload.
func TestWriteAgentProjectDir_ReExportDropsOrphanedSchemas(t *testing.T) {
	out, g, _ := successfulTypedExport(t)
	g.Agents["A"].Spec.Input = nil
	if err := WriteAgentProjectDir(out, g); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "schemas", "T.json")); !os.IsNotExist(err) {
		t.Fatalf("orphaned schema survived successful re-export: %v", err)
	}
}
