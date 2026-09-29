package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/lang/raise"
	"github.com/Terfyn/terfyn/internal/spec"
)

// Export IO seams tests can replace to inject failures. Production uses the stdlib defaults.
var (
	exportMarshalIndent = json.MarshalIndent
	exportMkdirAll      = os.MkdirAll
	exportWriteFile     = os.WriteFile
	exportRename        = os.Rename
	exportCommitAgent   = writeFileAtomic
)

// exportAgentFile is the single consolidated .agent source WriteAgentProjectDir writes.
const exportAgentFile = "project.agent"

// exportSchemasDir holds the JSON Schema files the exported .agent's type refs resolve against.
const exportSchemasDir = "schemas"

// WriteAgentProjectDir writes the graph as a loadable .agent project under dir (issue #507): a single
// consolidated project.agent holding every resource, plus a schemas/ directory with one JSON Schema
// file per resolved agent input/output and workflow input. Under ADR 007 .agent is the sole executable
// source, so — unlike a project.yaml directory, which LoadProject now refuses — this is a directory
// LoadProject(dir) can re-execute (`terfyn validate/plan/apply/run --project dir`). The source is
// re-raised from the graph via the same lossless-or-refuses path `terfyn migrate --to-agent` uses:
// a construct with no .agent authoring form is an error, never a silent lossy write.
//
// Some things are NOT preserved, all inherited from the graph model and raise — the same behavior
// `terfyn migrate --to-agent` has, not new here:
//
//   - the project's metadata.name — .agent has no project-name authoring surface, so a reloaded
//     project takes its name from the export directory;
//   - a workflow's declared return type (`-> Type`) — the graph's WorkflowOutput stores only the
//     return value, not its schema, and raise emits no `-> Type`, so a reloaded workflow is untyped
//     on its output (its inputs and every agent input/output are preserved);
//   - a workflow's `parallel` concurrency — raise linearizes the step DAG into a valid topological
//     order and emits no `parallel` block, so parallel steps come back as a sequential chain (the
//     result is behavior-equivalent for a pure DAG but no longer runs those steps concurrently).
//
// Fixing the latter two needs the graph to persist the return schema / a fan-out marker and raise to
// emit them; until then export inherits the gap. Positions and the import list are likewise not identity.
//
// dir is treated as generated output and must form a CLOSED set on reload:
//
//   - a dir that already holds a FOREIGN .agent file (anything other than our own project.agent) is
//     refused — LoadProject scans the whole tree for .agent and would merge it alongside the export,
//     duplicating resources;
//   - the schemas/ directory is fully replaced on a successful re-export, so a graph with fewer
//     types leaves no orphaned schema file that would reload;
//   - re-exporting into a directory this function already wrote is allowed (project.agent and
//     schemas/ are overwritten in place);
//   - a failed re-export never mutates the previous successful export (issue #561): the complete
//     tree is staged in a sibling temp directory and swapped into place only after every schema
//     file and project.agent are generated. Marshal, mkdir, schema-write, or final source-commit
//     failures leave the prior project.agent and schemas/ byte-for-byte intact.
func WriteAgentProjectDir(dir string, g *spec.ProjectGraph) error {
	if g == nil {
		return fmt.Errorf("project: nil graph")
	}
	file, unsupported := raise.Graph(g)
	if len(unsupported) > 0 {
		return fmt.Errorf("project: cannot export %q to .agent — %d construct(s) have no .agent authoring form: %s", g.Meta.Name, len(unsupported), formatUnsupported(unsupported))
	}
	source := lang.Print(file)
	// Self-check: the generated source must parse, or we would write a broken project. The forward
	// spec round-trip (raise then re-lower reproduces the graph) is proven in internal/lang/raise.
	if _, diags := lang.Parse(exportAgentFile, source); diags.HasErrors() {
		return fmt.Errorf("project: generated .agent did not parse (internal error):\n%s", diags.Error())
	}

	target := filepath.Join(dir, exportAgentFile)
	if err := refuseForeignAgentSources(dir, target); err != nil {
		return err
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(absDir), ".terfyn-export-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if err := writeExportedSchemas(staging, g); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(staging, exportAgentFile), []byte(source)); err != nil {
		return err
	}
	return commitStagedExport(dir, staging)
}

// commitStagedExport swaps a fully written staging tree into dir. schemas/ is moved aside (not
// deleted) until project.agent lands; any failure restores the previous schemas/ so a re-export
// cannot destroy a loadable project. On success the backup is removed, so a smaller graph leaves
// no orphaned schema files.
func commitStagedExport(dir, staging string) error {
	if err := exportMkdirAll(dir, 0o755); err != nil {
		return err
	}
	destSchemas := filepath.Join(dir, exportSchemasDir)
	stagSchemas := filepath.Join(staging, exportSchemasDir)
	destAgent := filepath.Join(dir, exportAgentFile)
	stagAgent := filepath.Join(staging, exportAgentFile)
	backup := destSchemas + ".terfyn-bak"
	_ = os.RemoveAll(backup)

	destHas := dirExists(destSchemas)
	stagHas := dirExists(stagSchemas)
	restore := func() {
		_ = os.RemoveAll(destSchemas)
		if destHas {
			_ = exportRename(backup, destSchemas)
		}
	}
	if destHas {
		if err := exportRename(destSchemas, backup); err != nil {
			return err
		}
	}
	if stagHas {
		if err := exportRename(stagSchemas, destSchemas); err != nil {
			restore()
			return err
		}
	}
	data, err := os.ReadFile(stagAgent)
	if err != nil {
		restore()
		return err
	}
	if err := exportCommitAgent(destAgent, data); err != nil {
		restore()
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// writeFileAtomic writes data to target via a temp file in the same directory and a rename, so a
// reader never observes a partially written file and a failed write leaves the prior file intact.
func writeFileAtomic(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".terfyn-export-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, target)
}

// refuseForeignAgentSources refuses dir when it already holds any .agent file other than the export
// target itself: LoadProject would merge that foreign source alongside the exported one and duplicate
// every resource. A dir that does not yet exist (or holds only our own project.agent) is fine.
func refuseForeignAgentSources(dir, target string) error {
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	agents, err := discoverAgentFiles(dirAbs)
	if err != nil {
		// The directory does not exist yet (or is unreadable): nothing foreign to merge.
		return nil
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	for _, a := range agents {
		aAbs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		if aAbs == targetAbs {
			continue
		}
		return fmt.Errorf("project: refusing to export into %q: it already contains .agent source %q, which LoadProject would merge alongside the exported project (duplicate resources) — export to an empty or non-source directory", dir, a)
	}
	return nil
}

// writeExportedSchemas (re)writes dir/schemas with one JSON Schema file per resolved typed I/O in the
// graph, so the exported .agent's type refs (input/output <Type> -> schemas/<Type>.json) resolve on
// reload. The directory is fully replaced so a re-export never leaves an orphaned schema behind. A
// type whose backing schema was never resolved (untyped/gradual typing, #193) is skipped — the
// reloaded project is untyped there too, matching the source.
func writeExportedSchemas(dir string, g *spec.ProjectGraph) error {
	schemas := collectResolvedSchemas(g)
	if len(schemas) == 0 {
		return nil
	}
	encoded := make(map[string][]byte, len(schemas))
	for _, ref := range sortedKeys(schemas) {
		body, err := exportMarshalIndent(schemas[ref], "", "  ")
		if err != nil {
			return fmt.Errorf("project: marshal schema %q: %w", ref, err)
		}
		encoded[ref] = append(body, '\n')
	}
	schemasDir := filepath.Join(dir, exportSchemasDir)
	if err := exportMkdirAll(schemasDir, 0o755); err != nil {
		return err
	}
	for _, ref := range sortedKeys(encoded) {
		path := filepath.Join(dir, filepath.FromSlash(ref))
		if err := exportWriteFile(path, encoded[ref], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// collectResolvedSchemas maps each canonical schema ref (schemas/<Type>.json) to its resolved schema
// object, over every typed agent input/output and workflow input in the graph. Only refs that follow
// the convention and carry a resolved document are included; the same Type used by several resources
// resolves to one file.
func collectResolvedSchemas(g *spec.ProjectGraph) map[string]map[string]any {
	out := map[string]map[string]any{}
	addAgentIO := func(io *spec.AgentIO) {
		if io == nil || io.Resolved == nil || io.Resolved.Raw == nil {
			return
		}
		if isCanonicalSchemaRef(io.Schema) {
			out[io.Schema] = io.Resolved.Raw
		}
	}
	for _, name := range sortedKeys(g.Agents) {
		a := g.Agents[name]
		if a == nil {
			continue
		}
		addAgentIO(a.Spec.Input)
		addAgentIO(a.Spec.Output)
	}
	for _, name := range sortedKeys(g.Workflows) {
		w := g.Workflows[name]
		if w == nil || w.Spec.Input == nil {
			continue
		}
		in := w.Spec.Input
		if in.Resolved != nil && in.Resolved.Raw != nil && isCanonicalSchemaRef(in.Schema) {
			out[in.Schema] = in.Resolved.Raw
		}
	}
	return out
}

// isCanonicalSchemaRef reports whether ref is a schemas/<Type>.json path with no directory traversal,
// so writing it stays inside the export's schemas/ directory.
func isCanonicalSchemaRef(ref string) bool {
	return strings.HasPrefix(ref, exportSchemasDir+"/") &&
		strings.HasSuffix(ref, ".json") &&
		!strings.Contains(ref, "..")
}

// formatUnsupported renders raise findings as a stable, sorted, semicolon-joined message.
func formatUnsupported(us []raise.Unsupported) string {
	parts := make([]string, 0, len(us))
	for _, u := range us {
		parts = append(parts, u.Error())
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
