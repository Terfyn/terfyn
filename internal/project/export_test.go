package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	writeFile(t, root, "src/pr.agent", `
agent Reviewer {
    model openai/gpt-5
    grants {
        tool.github.read_pr
    }
}

workflow Review(input: PullRequest) -> Review {
    pr = Reviewer(input)
    return pr
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
