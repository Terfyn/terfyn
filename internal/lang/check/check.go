package check

import (
	"github.com/Terfyn/terfyn/internal/effects"
	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/lang/lower"
	"github.com/Terfyn/terfyn/internal/spec"
)

// Options supplies context Check cannot derive from f alone.
type Options struct {
	// Project supplies already-loaded sibling YAML resources (agents, tools,
	// policies, other workflows) so a .agent file can reference and interoperate
	// with them. Nil is treated as an empty graph. Check never mutates Project —
	// it computes against a shallow clone.
	Project *spec.ProjectGraph

	// Files supplies other .agent ASTs in the same compilation unit. Every
	// file listed here is lowered, merged into Program.Graph, type-checked,
	// rebound, and effects-clause-checked exactly like f — this is one
	// compilation unit, not f plus context for f. This is the project-wide
	// symbol table internal/lang/lower/lower.go's Options.Workflows doc
	// comment names as this package's replacement.
	Files []*lang.File

	// SchemaDir overrides where TypeRef names resolve to schema files
	// (<SchemaDir>/schemas/<Name>.json). Defaults to the directory of f's own
	// position (f.Pos.File) when empty.
	SchemaDir string
}

// Program is the checked program: f plus the resolved graph and effect bound
// it type/effect-checks against. A non-nil Program is always returned, even
// when Check reports errors, so a caller can inspect partial results — the
// returned Diagnostics is the authority on pass/fail via HasErrors (or
// AsError to get a plain error, nil for a warning-only result). Do not treat
// a non-empty Diagnostics or a bare error-interface conversion of it as
// failure on its own — see Diagnostics.AsError.
type Program struct {
	File   *lang.File
	Graph  *spec.ProjectGraph
	Bounds effects.GraphBounds
	// Executables is the execution-IR projection of every workflow in the
	// compilation unit, keyed by workflow name (ADR 002 §5, #199). This is the
	// form control flow (Branch/Loop/Fork) lives in, executable via execir.Interp.
	// Positional workflow: arguments are rebound to real parameter names here
	// (applyExecRebinds), the same rewrite Graph receives.
	//
	// Not yet on a production path: project.LoadProject (#200) runs Check and uses
	// its Graph (the resource projection), but no planner or runner constructs an
	// execir.Program — the loader instead REFUSES control-flow workflows, and the
	// plan-hash fold of execir.Program.Digest (plan.WorkflowSpecHashWithExec) is
	// unused. Wiring these programs onto the engine is the remaining work (with the
	// persistence half of #199, #207). Populated even when diagnostics are present
	// (best-effort, like Graph).
	Executables map[string]*execir.Program
}

// Check resolves, type-checks, and effect-checks the WHOLE compilation unit
// (f plus every file in Options.Files) — not just f. Program.Graph is one
// merged graph holding every file's lowered workflows, and it is documented
// (and relied on by callers) as an executable projection; type-checking only
// f while Files' workflows still sit on that same graph would leave a
// positional workflow: call in a Files-only file with its lowered
// arg0/arg1 keys never rebound, silently contradicting that contract for
// exactly the files a caller is least likely to have already checked
// directly.
//
// The effect bound is computed by lowering EVERY file in the compilation unit
// (f plus Options.Files) through lower.LowerFile into one merged
// *spec.ProjectGraph (with Options.Project) and calling effects.Compute on
// the result — see doc.go for why this package never walks the AST for
// effects on its own. Lowering only f and merely classifying Options.Files'
// declarations for callee-kind purposes would leave a cross-file callee's own
// body out of the graph: effects.Compute would then walk a workflow callee
// that resolves to nothing (silently contributing no effects — fail-open) or
// an agent callee that resolves to nothing (contributing Unknown — fail-closed
// but still not the real grant set). Lowering the whole unit is what makes
// "the frontend and YAML paths agree on a bound" true when a program spans
// more than one .agent file.
//
// Check also REWRITES the resource projection it returns: lower.LowerFile
// keys a positional call argument by raw index (arg0, arg1, ...) because
// lowering has no symbol table of its own to resolve a callee's declared
// parameters against. Type-checking a workflow: call does have that
// information (checkWorkflowArgs), so Check applies the resolved parameter
// names back onto the already-lowered graph as a second pass (applyRebinds)
// — otherwise a positional workflow call would type-check clean while
// Program.Graph still carried a with: map the callee cannot read. This only
// touches steps this call itself just lowered, never a resource reachable
// through Options.Project.
func Check(f *lang.File, opts Options) (*Program, lang.Diagnostics) {
	prog := &Program{File: f}
	if f == nil {
		return prog, nil
	}

	var diags lang.Diagnostics
	var lowered []*spec.WorkflowResource

	unit := compilationUnit(f, opts.Files)
	workflowNames := collectWorkflowNames(unit, opts.Project)

	graph := cloneGraph(opts.Project)
	executables := map[string]*execir.Program{}
	for _, file := range unit {
		result, lowerDiags := lower.LowerFile(file, lower.Options{Workflows: workflowNames})
		diags = append(diags, lowerDiags...)
		lowered = append(lowered, result.Workflows...)
		if err := lower.MergeLowered(graph, result); err != nil {
			diags = append(diags, lang.Diagnostic{Pos: file.Pos, Msg: err.Error()})
		}
		// Execution lowering is the sibling projection (ADR 002 §5): lowered
		// directly from the AST, in parallel with the resource projection above,
		// never from it. Its diagnostics (e.g. a call inside a condition) are
		// part of the compilation unit's result.
		for _, d := range file.Decls {
			wd, ok := d.(*lang.WorkflowDecl)
			if !ok {
				continue
			}
			execProg, execDiags := lower.LowerExec(wd, workflowNames)
			diags = append(diags, execDiags...)
			if execProg != nil && execProg.Workflow != "" {
				executables[execProg.Workflow] = execProg
			}
		}
	}
	prog.Graph = graph
	prog.Executables = executables
	prog.Bounds = effects.Compute(graph)

	tu, typeDiags := resolveTypes(f, opts)
	diags = append(diags, typeDiags...)
	wireAgentSchemas(unit, tu, graph)
	wireWorkflowSchemas(unit, tu, graph)
	checkDiags, rebinds := checkTypes(unit, tu)
	diags = append(diags, checkDiags...)
	applyRebinds(lowered, rebinds)
	// The same positional-argument rebind must reach the EXECUTION IR, not only
	// the resource projection: LowerExec keys positional workflow: arguments as
	// arg0/arg1 exactly as LowerFile does, and Program.Executables is the form the
	// engine will run — an InvokeWorkflow left with arg0 keys would hand the
	// Invoker a map under the wrong keys (paramScope binds by parameter name).
	applyExecRebinds(executables, rebinds)

	diags = append(diags, checkEffectsClauses(unit, prog.Bounds)...)

	return prog, dedupDiags(diags.Sorted())
}

// wireAgentSchemas records each .agent agent's resolved input/output schema onto
// the resource projection (#294). The checker is the single component that resolves
// a type ref (schemas/<Name>.json) and knows whether the file exists, so it — not
// the pure resource lowering — populates AgentSpec.Input/Output: an UNRESOLVED type
// stays absent (matching the checker's leniency, so an agent with a typed I/O and no
// schema file is not forced to fail schema-file validation), and a RESOLVED one
// carries both the project-root-relative Schema ref (lower.SchemaRef, the same
// convention resolveTypes uses) and the compiled document, so validate and the
// runtime enforce structured agent I/O for .agent-authored agents exactly as for
// YAML-authored ones.
func wireAgentSchemas(unit []*lang.File, tu *typeUniverse, graph *spec.ProjectGraph) {
	if graph == nil || tu == nil {
		return
	}
	for _, file := range unit {
		if file == nil {
			continue
		}
		for _, d := range file.Decls {
			ad, ok := d.(*lang.AgentDecl)
			if !ok {
				continue
			}
			ar := graph.Agents[identName(ad.Name)]
			if ar == nil {
				continue
			}
			info := tu.agents[identName(ad.Name)]
			if ad.Input != nil && info.Input != nil {
				ar.Spec.Input = &spec.AgentIO{Schema: lower.SchemaRef(ad.Input.Name), Resolved: info.Input}
			}
			if ad.Output != nil && info.Output != nil {
				ar.Spec.Output = &spec.AgentIO{Schema: lower.SchemaRef(ad.Output.Name), Resolved: info.Output}
			}
		}
	}
}

// wireWorkflowSchemas records each .agent workflow's resolved input schema onto the resource
// projection, so a .agent workflow gets the same runtime input validation a YAML `spec.input.schema`
// gave (ADR 007 parity). The runtime input's identity is arity-structural, exactly as lower.newEnv
// binds it: a workflow with a SINGLE parameter (whatever its name) takes that parameter's type as the
// whole input, so its type is the runtime input schema; a workflow with MULTIPLE parameters takes a
// synthesized composite `{param: …}` object for which no single author-provided schema exists, so
// nothing is wired (lenient — never validate the composite against one field's schema). As with
// agents, an UNRESOLVED type also stays absent, and a RESOLVED one carries both the
// project-root-relative ref and the compiled document.
func wireWorkflowSchemas(unit []*lang.File, tu *typeUniverse, graph *spec.ProjectGraph) {
	if graph == nil || tu == nil {
		return
	}
	for _, file := range unit {
		if file == nil {
			continue
		}
		for _, d := range file.Decls {
			wd, ok := d.(*lang.WorkflowDecl)
			if !ok {
				continue
			}
			wr := graph.Workflows[identName(wd.Name)]
			if wr == nil {
				continue
			}
			// Mirror lower.newEnv: only a single parameter is the whole runtime input.
			if len(wd.Params) != 1 || wd.Params[0].Type == nil {
				continue
			}
			info := tu.workflows[identName(wd.Name)]
			if len(info.ParamOrder) == 0 {
				continue
			}
			if doc := info.Params[info.ParamOrder[0]]; doc != nil {
				wr.Spec.Input = &spec.WorkflowInput{Schema: lower.SchemaRef(wd.Params[0].Type.Name), Resolved: doc}
			}
		}
	}
}

// dedupDiags drops every exact-duplicate diagnostic (same position, message,
// and severity), keeping the first occurrence and the input order. The checker
// is the authority for unresolved references and emits the same message
// lowering's prefixOf does for a straight-line reference, so a genuine typo
// would otherwise be reported twice; a control-flow scope violation is reported
// by the checker alone. Duplicates need not be adjacent: Sorted orders by
// position only, and a call-argument string with several bad ${…} tokens puts
// the checker's [t1, t2] and lowering's [t1, t2] at the same position, so the
// two copies of t1 are interleaved with t2. A seen-set catches them anyway.
func dedupDiags(diags lang.Diagnostics) lang.Diagnostics {
	if len(diags) < 2 {
		return diags
	}
	seen := make(map[lang.Diagnostic]struct{}, len(diags))
	out := make(lang.Diagnostics, 0, len(diags))
	for _, d := range diags {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// applyRebinds rewrites a lowered workflow: step's placeholder with: keys
// (argN) to the callee's real parameter names, per rb.renames (see rebind's
// doc comment for the position-correlation approach). lowered is exactly the
// set of *spec.WorkflowResource values this Check call itself just produced
// via lower.LowerFile — never a resource reachable through Options.Project,
// which Check must not mutate; cloneGraph only clones the graph's maps, not
// the resource values a caller's Project graph points to.
//
// Each matching step's with: map is rebuilt FRESH from its original entries
// rather than mutated key-by-key in place. Lowering's placeholder namespace
// (arg0, arg1, ...) and the callee's real parameter namespace share the same
// map, so a parameter can legally be named e.g. "arg1" — a sequence of
// delete-then-insert on one live map can then alias one rename's target onto
// another rename's source (arg0->arg1 followed by arg1->a silently drops the
// first value once the second rename fires). Building a new map by reading
// every ORIGINAL key exactly once and writing to a separate map has no such
// hazard: every old key is looked up in renames independent of what any
// other key rewrites to.
func applyRebinds(lowered []*spec.WorkflowResource, rebinds []rebind) {
	if len(rebinds) == 0 {
		return
	}
	byPos := make(map[lang.Pos]map[string]string, len(rebinds))
	for _, rb := range rebinds {
		byPos[rb.pos] = rb.renames
	}
	for _, wr := range lowered {
		for i := range wr.Spec.Steps {
			s := &wr.Spec.Steps[i]
			if s.WorkflowPos.IsZero() || s.With == nil {
				continue
			}
			renames, ok := byPos[s.WorkflowPos]
			if !ok {
				continue
			}
			next := make(map[string]any, len(s.With))
			for k, v := range s.With {
				if nk, renamed := renames[k]; renamed {
					next[nk] = v
				} else {
					next[k] = v
				}
			}
			s.With = next
		}
	}
}

// applyExecRebinds rewrites positional workflow: argument keys (arg0, arg1, ...)
// on every InvokeWorkflow in the execution IR to the callee's real parameter
// names, using the same rebinds Check applies to the resource projection
// (applyRebinds). Correlation is by position: LowerExec stamps InvokeWorkflow.Pos
// with the call's position, which equals the callee position rebind.pos carries
// (CallExpr.Pos is its callee RefExpr.Pos), so no re-derivation of a step-id
// scheme is needed. It recurses into every control-flow body so a workflow call
// inside an if/loop/parallel — including a hoisted nested call — is rebound too.
func applyExecRebinds(executables map[string]*execir.Program, rebinds []rebind) {
	if len(rebinds) == 0 {
		return
	}
	byPos := make(map[lang.Pos]map[string]string, len(rebinds))
	for _, rb := range rebinds {
		byPos[rb.pos] = rb.renames
	}
	for _, prog := range executables {
		if prog != nil {
			rebindNodes(prog.Body, byPos)
		}
	}
}

func rebindNodes(nodes []execir.Node, byPos map[lang.Pos]map[string]string) {
	for _, n := range nodes {
		switch v := n.(type) {
		case *execir.InvokeWorkflow:
			if renames, ok := byPos[v.Pos]; ok {
				v.Args = renameArgs(v.Args, renames)
			}
		case *execir.Branch:
			rebindNodes(v.Then, byPos)
			rebindNodes(v.Else, byPos)
		case *execir.Loop:
			rebindNodes(v.Body, byPos)
		case *execir.While:
			rebindNodes(v.Body, byPos)
		case *execir.Retry:
			rebindNodes(v.Body, byPos)
		case *execir.Fork:
			for i := range v.Branches {
				rebindNodes(v.Branches[i].Nodes, byPos)
			}
		case *execir.Graph:
			for i := range v.Nodes {
				rebindNodes([]execir.Node{v.Nodes[i].Run}, byPos)
			}
		}
	}
}

// renameArgs rebuilds an argument map applying renames, reading every original
// key once so a parameter legally named like a placeholder (arg1) cannot alias
// one rename's target onto another's source — the same hazard applyRebinds
// avoids for the resource projection.
func renameArgs(args map[string]execir.Value, renames map[string]string) map[string]execir.Value {
	if args == nil {
		return nil
	}
	out := make(map[string]execir.Value, len(args))
	for k, val := range args {
		if nk, ok := renames[k]; ok {
			out[nk] = val
		} else {
			out[k] = val
		}
	}
	return out
}

// compilationUnit returns f followed by every distinct, non-nil file in
// files, so every file in the unit is lowered exactly once (f first, so its
// own diagnostics are unaffected by Options.Files ordering).
func compilationUnit(f *lang.File, files []*lang.File) []*lang.File {
	unit := make([]*lang.File, 0, len(files)+1)
	unit = append(unit, f)
	seen := map[*lang.File]bool{f: true}
	for _, other := range files {
		if other == nil || seen[other] {
			continue
		}
		seen[other] = true
		unit = append(unit, other)
	}
	return unit
}

// collectWorkflowNames gathers every workflow name declared anywhere in the
// compilation unit or the already-loaded YAML project, so a single-identifier
// callee in ANY file of the unit classifies as a workflow: step rather than
// defaulting to agent:. Passing this full set to every file's LowerFile call
// is safe even for names a file declares itself: lower.go's classifyDecls
// gives a file's own declarations priority over Options.Workflows.
func collectWorkflowNames(unit []*lang.File, project *spec.ProjectGraph) map[string]bool {
	out := map[string]bool{}
	for _, file := range unit {
		for _, d := range file.Decls {
			if wd, ok := d.(*lang.WorkflowDecl); ok {
				if name := identName(wd.Name); name != "" {
					out[name] = true
				}
			}
		}
	}
	if project != nil {
		for name := range project.Workflows {
			out[name] = true
		}
	}
	return out
}

// cloneGraph returns a shallow copy of g (or an empty graph for nil) so
// project.MergeLowered's in-place mutation never surprises a caller holding g.
// Resource pointers are shared; only the top-level maps are copied.
func cloneGraph(g *spec.ProjectGraph) *spec.ProjectGraph {
	out := &spec.ProjectGraph{
		Agents:       map[string]*spec.AgentResource{},
		Tools:        map[string]*spec.ToolResource{},
		Workflows:    map[string]*spec.WorkflowResource{},
		Policies:     map[string]*spec.PolicyResource{},
		Environments: map[string]*spec.EnvironmentResource{},
	}
	if g == nil {
		return out
	}
	out.Meta = g.Meta
	out.Pos = g.Pos
	out.Spec = g.Spec
	// Deep-copy the provider-alias map: MergeLowered may add .agent `provider` aliases to the clone's
	// ProjectSpec (issue #440), and the shared *ProjectProviders pointer would otherwise leak those
	// writes back into the caller's graph — exactly what this clone exists to prevent.
	if g.Spec.Providers != nil {
		cp := *g.Spec.Providers
		if g.Spec.Providers.Models != nil {
			cp.Models = make(map[string]spec.ModelProviderConfig, len(g.Spec.Providers.Models))
			for k, v := range g.Spec.Providers.Models {
				cp.Models[k] = v
			}
		}
		out.Spec.Providers = &cp
	}
	for k, v := range g.Agents {
		out.Agents[k] = v
	}
	for k, v := range g.Tools {
		out.Tools[k] = v
	}
	for k, v := range g.Workflows {
		out.Workflows[k] = v
	}
	for k, v := range g.Policies {
		out.Policies[k] = v
	}
	for k, v := range g.Environments {
		out.Environments[k] = v
	}
	return out
}

func identName(id *lang.Ident) string {
	if id == nil {
		return ""
	}
	return id.Name
}
