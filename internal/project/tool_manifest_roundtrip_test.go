package project

import (
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/tools"
)

// ADR 003 acceptance for the closed-world capability manifest (#204 / PR #251 review): a
// declared-but-empty operations: {} manifest must round-trip through export → load unchanged, so the
// exported project agrees with plan/apply identity and CheckToolCall. This exercises the .agent export
// path (issue #507) reloaded through the user-facing LoadProject — the closed-empty manifest must
// survive raise → print → parse → lower, not just the YAML codec.
func TestExport_ClosedEmptyManifestRoundTrips(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "project.yaml", `apiVersion: agentic.dev/v0
kind: Project
metadata:
  name: demo
spec:
  imports:
    - resources
`)
	writeFile(t, root, "resources/tool-locked.yaml", `apiVersion: agentic.dev/v0
kind: Tool
metadata:
  name: locked
spec:
  type: native
  operations: {}
`)

	g, _, err := LoadYAMLResources(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m := tools.ManifestFor(g, "locked"); !m.IsClosed() || m.Allows("delete_repo") {
		t.Fatalf("loaded closed-empty tool not closed: closed=%v allows=%v", m.IsClosed(), m.Allows("delete_repo"))
	}

	// Export the graph as a .agent project, then reload it through the user-facing loader.
	out := t.TempDir()
	if err := WriteAgentProjectDir(out, g); err != nil {
		t.Fatalf("export: %v", err)
	}
	reloaded, err := LoadProject(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	m := tools.ManifestFor(reloaded, "locked")
	if !m.IsClosed() {
		t.Fatal("export → load reopened the closed-empty manifest")
	}
	if m.Allows("delete_repo") {
		t.Fatal("reloaded manifest must still deny operations outside it")
	}

	// The exported YAML must carry the operations key, not drop it.
	yamlBytes, err := ExportYAML(g)
	if err != nil {
		t.Fatalf("ExportYAML: %v", err)
	}
	if !strings.Contains(string(yamlBytes), "operations:") {
		t.Fatalf("exported YAML dropped the closed-empty manifest:\n%s", yamlBytes)
	}
}
