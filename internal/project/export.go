package project

import (
	"bytes"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/Terfyn/terfyn/internal/spec"
)

// ExportYAML materializes a resource graph as a multi-document YAML stream (ADR
// 003 decision 1: YAML is compilation output produced on demand, never written
// by default). The stream is deterministic — Project first, then Agents, Tools,
// Policies, Workflows, Environments, each sorted by name — so re-exporting an
// unchanged graph is byte-stable. Source positions are excluded (they are
// `yaml:"-"`), matching the ADR rule that positions are diagnostic metadata,
// never identity.
//
// The emitted Project clears spec.imports: every resource is inline in the
// stream, so the import list (which named the original source files) would be
// stale. This stream is one-way YAML for inspection and handoff; a loadable
// on-disk project is produced by WriteAgentProjectDir (a .agent project, the
// sole executable source under ADR 007).
func ExportYAML(g *spec.ProjectGraph) ([]byte, error) {
	if g == nil {
		return nil, fmt.Errorf("project: nil graph")
	}
	docs := make([]any, 0, 1+len(g.Agents)+len(g.Tools)+len(g.Workflows)+len(g.Policies)+len(g.Environments))

	proj := projectResource(g)
	proj.Spec.Imports = nil
	docs = append(docs, proj)
	docs = append(docs, nonProjectResources(g)...)

	return marshalDocs(docs)
}

// resourceEntry pairs a resource with its kind and name for deterministic
// per-file emission.
type resourceEntry struct {
	kind     string
	name     string
	resource any
}

// projectResource reconstructs the Project resource envelope from the graph's
// project-level metadata and spec.
func projectResource(g *spec.ProjectGraph) *spec.ProjectResource {
	return &spec.ProjectResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindProject,
		Metadata:   g.Meta,
		Spec:       g.Spec,
	}
}

// nonProjectResources returns every non-Project resource in deterministic
// kind-then-name order.
func nonProjectResources(g *spec.ProjectGraph) []any {
	entries := nonProjectResourceEntries(g)
	docs := make([]any, 0, len(entries))
	for _, e := range entries {
		docs = append(docs, e.resource)
	}
	return docs
}

// nonProjectResourceEntries returns every non-Project resource with its kind and
// name, in deterministic kind-then-name order.
func nonProjectResourceEntries(g *spec.ProjectGraph) []resourceEntry {
	var out []resourceEntry
	for _, name := range sortedKeys(g.Agents) {
		out = append(out, resourceEntry{spec.KindAgent, name, g.Agents[name]})
	}
	for _, name := range sortedKeys(g.Tools) {
		out = append(out, resourceEntry{spec.KindTool, name, g.Tools[name]})
	}
	for _, name := range sortedKeys(g.Policies) {
		out = append(out, resourceEntry{spec.KindPolicy, name, g.Policies[name]})
	}
	for _, name := range sortedKeys(g.Workflows) {
		out = append(out, resourceEntry{spec.KindWorkflow, name, g.Workflows[name]})
	}
	for _, name := range sortedKeys(g.Environments) {
		out = append(out, resourceEntry{spec.KindEnvironment, name, g.Environments[name]})
	}
	return out
}

// marshalDocs encodes docs as a multi-document YAML stream separated by `---`.
func marshalDocs(docs []any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			_ = enc.Close()
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
