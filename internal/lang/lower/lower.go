package lower

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// Options carries classification the current file cannot supply on its own.
type Options struct {
	// Workflows names EXTRA callees — project-level workflows declared in other
	// files — that a single-identifier call should lower to a workflow: step.
	// Workflows declared in the file being lowered are detected automatically and
	// need not be listed here; a name declared in the file always wins over this
	// set. Dotted callees (github.get_pr) are always tool calls. Once #198 lands
	// its project-wide symbol table replaces this field.
	Workflows map[string]bool
}

// Result is the resource projection of a lowered .agent file (ADR 002 §5): the
// Agent/Tool/Policy/Workflow resources plan/apply/policy analysis run against. It
// is deliberately NOT an input to the execution lowering (#199) — see doc.go.
//
// A Result is a valid resource projection only when LowerFile returned NO
// diagnostics. When diagnostics are present the Result is best-effort: it may
// carry duplicate resource names and invalid interpolation tokens (e.g. a
// whole-input ${input} that engine.resolvePath rejects), because lowering does
// not drop a written construct without a diagnostic. Callers must check the
// diagnostics before using a Result. In particular LowerFile is the authority for
// resource identity in a file — it reports a duplicate agent/workflow name, or a
// name declared as both — so ToGraph and project.MergeLowered may assume unique
// names on a diagnostic-free Result.
type Result struct {
	Agents       []*spec.AgentResource
	Workflows    []*spec.WorkflowResource
	Tools        []*spec.ToolResource
	Policies     []*spec.PolicyResource
	Environments []*spec.EnvironmentResource
	// Providers are not a resource kind — they lower into spec.ProjectSpec.Providers.Models (project
	// config), not a graph map. Carried here in author order so ToGraph/MergeLowered can fold them into
	// the project spec with duplicate-alias detection (issue #440).
	Providers []LoweredProvider
	// Defaults is the singleton `defaults { … }` block lowered into spec.ProjectSpec.Defaults (project
	// config, not a resource). Nil when the file declares no `defaults` block (issue #440, ADR 007).
	Defaults *spec.ProjectDefaults
	// Limits is the singleton top-level `limits { … }` block lowered into spec.ProjectSpec.Limits, the
	// project-wide execution-limit baseline. Nil when the file declares none (issue #440, ADR 007).
	Limits    *spec.ExecutionLimits
	SourceMap *SourceMap
}

// LoweredProvider is one `provider <alias> { … }` declaration lowered to its ProjectSpec config.
type LoweredProvider struct {
	Name   string
	Config spec.ModelProviderConfig
	Pos    spec.Pos
}

// ToGraph folds the lowered resources into a fresh spec.ProjectGraph keyed by
// name. It does not merge with any existing graph; use project.MergeLowered for
// that. It assumes a diagnostic-free Result (unique names, per the Result doc);
// duplicate names would collapse under the map, which is why LowerFile diagnoses
// them.
func (r *Result) ToGraph() *spec.ProjectGraph {
	g := &spec.ProjectGraph{
		Agents:       map[string]*spec.AgentResource{},
		Tools:        map[string]*spec.ToolResource{},
		Workflows:    map[string]*spec.WorkflowResource{},
		Policies:     map[string]*spec.PolicyResource{},
		Environments: map[string]*spec.EnvironmentResource{},
	}
	if r == nil {
		return g
	}
	for _, a := range r.Agents {
		g.Agents[a.Metadata.Name] = a
	}
	for _, w := range r.Workflows {
		g.Workflows[w.Metadata.Name] = w
	}
	for _, t := range r.Tools {
		g.Tools[t.Metadata.Name] = t
	}
	for _, pol := range r.Policies {
		g.Policies[pol.Metadata.Name] = pol
	}
	for _, e := range r.Environments {
		g.Environments[e.Metadata.Name] = e
	}
	for _, pv := range r.Providers {
		setProviderModel(g, pv.Name, pv.Config)
	}
	if r.Defaults != nil {
		g.Spec.Defaults = r.Defaults
	}
	if r.Limits != nil {
		g.Spec.Limits = r.Limits
	}
	return g
}

// setProviderModel writes one lowered provider alias into g.Spec.Providers.Models, allocating the
// nested config as needed. Providers are project config, not a resource keyed in a graph map.
func setProviderModel(g *spec.ProjectGraph, name string, cfg spec.ModelProviderConfig) {
	if g.Spec.Providers == nil {
		g.Spec.Providers = &spec.ProjectProviders{}
	}
	if g.Spec.Providers.Models == nil {
		g.Spec.Providers.Models = map[string]spec.ModelProviderConfig{}
	}
	g.Spec.Providers.Models[name] = cfg
}

// LowerFile lowers a parsed .agent file into its resource projection. Diagnostics
// are lowering-time problems (an unresolved reference, a malformed call); an
// empty result is returned for a nil file.
func LowerFile(f *lang.File, opts Options) (*Result, lang.Diagnostics) {
	l := &lowerer{opts: opts, sm: newSourceMap()}
	res := &Result{SourceMap: l.sm}
	if f == nil {
		return res, nil
	}
	// A pre-pass over the whole file establishes resource identity before any
	// body is lowered: which single-identifier callees are workflows vs agents
	// (a call may name a workflow declared later in the file), and which names
	// collide. The classifier this builds is the answer the File already holds —
	// callee kind is not deferred to #198's project-wide resolution.
	l.classifyDecls(f)
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *lang.AgentDecl:
			if a := l.agent(decl); a != nil {
				res.Agents = append(res.Agents, a)
			}
		case *lang.WorkflowDecl:
			if w := l.workflow(decl); w != nil {
				res.Workflows = append(res.Workflows, w)
			}
		case *lang.ToolDecl:
			if t := l.tool(decl); t != nil {
				res.Tools = append(res.Tools, t)
			}
		case *lang.PolicyDecl:
			if pol := l.policy(decl); pol != nil {
				res.Policies = append(res.Policies, pol)
			}
		case *lang.EnvironmentDecl:
			if e := l.environment(decl); e != nil {
				res.Environments = append(res.Environments, e)
			}
		case *lang.ProviderDecl:
			if pv, ok := l.provider(decl); ok {
				res.Providers = append(res.Providers, pv)
			}
		case *lang.DefaultsDecl:
			if res.Defaults != nil {
				l.diag(decl.Pos, "duplicate `defaults` block: a project may declare defaults at most once")
				continue
			}
			res.Defaults = l.defaults(decl)
		case *lang.LimitsDecl:
			if res.Limits != nil {
				l.diag(decl.Pos, "duplicate `limits` block: a project may declare limits at most once")
				continue
			}
			res.Limits = l.projectLimits(decl)
		}
	}
	return res, l.diags
}

type lowerer struct {
	opts  Options
	sm    *SourceMap
	diags lang.Diagnostics
	// workflowCallee[name] is true when a single-identifier call to name lowers
	// to a workflow: step. Built by classifyDecls from the file's WorkflowDecls
	// (and Options.Workflows); an agent-declared name is never marked.
	workflowCallee map[string]bool
}

// classifyDecls records, for every top-level declaration, whether its name is an
// agent or a workflow, so a single-identifier callee lowers to the right step
// kind. A name declared twice (same kind) or as both an agent and a workflow is a
// diagnostic — the file is the first place resource identity exists, so the
// ambiguity is reported here, never resolved by silently choosing agent:.
func (l *lowerer) classifyDecls(f *lang.File) {
	l.workflowCallee = map[string]bool{}
	kind := map[string]string{}
	at := map[string]spec.Pos{}
	for _, d := range f.Decls {
		var name, k string
		var pos spec.Pos
		switch decl := d.(type) {
		case *lang.AgentDecl:
			name, k, pos = identName(decl.Name), "agent", declNamePos(decl.Name, decl.Pos)
		case *lang.WorkflowDecl:
			name, k, pos = identName(decl.Name), "workflow", declNamePos(decl.Name, decl.Pos)
		default:
			continue
		}
		if name == "" {
			continue
		}
		if prev, ok := kind[name]; ok {
			if prev == k {
				l.diag(pos, "duplicate %s %q (already declared at %s)", k, name, at[name].String())
			} else {
				l.diag(pos, "%q is declared as both an agent and a workflow", name)
			}
			continue // first declaration wins the classification
		}
		kind[name] = k
		at[name] = pos
		if k == "workflow" {
			l.workflowCallee[name] = true
		}
	}
	// Extra project-level workflow names from other files, but never overriding a
	// name this file declares as an agent.
	for name := range l.opts.Workflows {
		if _, declared := kind[name]; !declared {
			l.workflowCallee[name] = true
		}
	}
}

// instructionsFilePath returns the referenced path for diagnostics ("" if malformed).
func instructionsFilePath(f *lang.InstructionsFile) string {
	if f == nil || f.Path == nil {
		return ""
	}
	return f.Path.Value
}

func declNamePos(id *lang.Ident, fallback spec.Pos) spec.Pos {
	if id != nil && !id.Pos.IsZero() {
		return id.Pos
	}
	return fallback
}

func (l *lowerer) diag(p spec.Pos, format string, args ...any) {
	l.diags = append(l.diags, lang.Diagnostic{Pos: p, Msg: fmt.Sprintf(format, args...)})
}

// --- Agents -----------------------------------------------------------------

func (l *lowerer) agent(d *lang.AgentDecl) *spec.AgentResource {
	name := identName(d.Name)
	if name == "" {
		l.diag(d.Pos, "agent declaration has no name")
		return nil
	}
	ar := &spec.AgentResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindAgent,
		Metadata:   spec.Metadata{Name: name},
		Pos:        d.Pos,
	}
	l.sm.set(KeyAgent(name), d.Pos)
	if d.Model != nil {
		ar.Spec.Model = d.Model.Raw
	}
	if d.Policy != nil {
		ar.Spec.Policy = d.Policy.Name
	}
	if d.Description != nil {
		ar.Spec.Description = d.Description.Value
	}
	if d.Constraints != nil {
		ar.Spec.Constraints = lowerConstraints(d.Constraints)
	}
	// instructions -> AgentSpec.Instructions: the agent prompt, copied verbatim
	// (the lexer already normalized a multiline body). No new prompt abstraction
	// and no new runtime semantics — the existing agent runtime consumes it.
	if d.Instructions != nil {
		ar.Spec.Instructions = d.Instructions.Value
		l.sm.set(KeyAgentInstructions(name), d.Instructions.Pos)
	} else if d.InstructionsFile != nil {
		// instructions file("path") (#360): the file's contents, resolved by the project loader,
		// lower verbatim exactly as an inline string would. Resolved is a pointer: a nil here means
		// the reference reached lowering WITHOUT loader resolution — which would silently pin an
		// empty prompt into the spec hash/snapshot — so it is a hard diagnostic, not a benign empty.
		if d.InstructionsFile.Resolved == nil {
			l.diag(d.InstructionsFile.Pos, "instructions file(%q) reached lowering without loader resolution — file references must be resolved by the project loader", instructionsFilePath(d.InstructionsFile))
		} else {
			ar.Spec.Instructions = *d.InstructionsFile.Resolved
			l.sm.set(KeyAgentInstructions(name), d.InstructionsFile.Pos)
		}
	}
	// grants -> AgentSpec.Tools: an autonomous capability bound, not a call list
	// (ADR 002). Each grant reconstructs the tool.<name>.<operation> uses string.
	// A grant may bind several operations on ONE tool (tool.github.read_pr +
	// tool.github.read_comments, or the implement-review workspace read_file +
	// write_file + run_tests); ResolveAgentAdvertisedTools advertises each operation
	// as its own tool-def, gated independently (#291).
	for _, g := range d.Grants {
		uses := grantUses(g)
		if uses == "" {
			// A malformed grant is already a parser diagnostic; skip silently so
			// lowering does not double-report.
			continue
		}
		ar.Spec.Tools = append(ar.Spec.Tools, uses)
		ar.Spec.ToolsPos = append(ar.Spec.ToolsPos, g.Pos)
		l.sm.set(KeyAgentGrant(name, uses), g.Pos)
	}
	// Type references are recorded in the source map here; the CHECKER populates
	// AgentSpec.Input/Output.Schema from them when the schemas/<Name>.json file
	// resolves (#294, check.wireAgentSchemas) — it is the single place that resolves
	// a type ref and knows whether the file exists, so an unresolved type stays
	// untyped (the checker's leniency) rather than failing schema-file validation.
	if d.Input != nil {
		l.sm.set(KeyAgentType(name, "input"), d.Input.Pos)
	}
	if d.Output != nil {
		l.sm.set(KeyAgentType(name, "output"), d.Output.Pos)
	}
	return ar
}

// SchemaRef returns the project-root-relative schema path for a .agent type name,
// following the schemas/<Name>.json convention shared by the checker (which resolves
// it against the schema dir) and the resource projection (which stores it as an
// AgentIO.Schema ref, resolved against the project root at validate). The loader
// passes the project root as the checker's schema dir, so both use the same base and
// the two never diverge (#294).
func SchemaRef(typeName string) string {
	return "schemas/" + typeName + ".json"
}

// lowerConstraints converts a parsed constraints block into spec.AgentConstraints,
// copying only the fields the author set (#310).
func lowerConstraints(c *lang.Constraints) *spec.AgentConstraints {
	out := &spec.AgentConstraints{}
	if c.MaxIterations != nil {
		out.MaxIterations = *c.MaxIterations
	}
	if c.MaxTokens != nil {
		out.MaxTokens = *c.MaxTokens
	}
	if c.TimeoutSeconds != nil {
		out.TimeoutSeconds = *c.TimeoutSeconds
	}
	if c.Temperature != nil {
		t := *c.Temperature
		out.Temperature = &t
	}
	if c.RequireStructuredOutput != nil {
		out.RequireStructuredOutput = *c.RequireStructuredOutput
	}
	return out
}

func grantUses(g *lang.Grant) string {
	tn := g.ToolName()
	op := g.OperationName()
	if tn == "" || op == "" {
		return ""
	}
	return "tool." + tn + "." + op
}

// --- Workflows --------------------------------------------------------------

func (l *lowerer) workflow(d *lang.WorkflowDecl) *spec.WorkflowResource {
	name := identName(d.Name)
	if name == "" {
		l.diag(d.Pos, "workflow declaration has no name")
		return nil
	}
	wr := &spec.WorkflowResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindWorkflow,
		Metadata:   spec.Metadata{Name: name},
		Pos:        d.Pos,
	}
	if d.Description != nil {
		wr.Spec.Description = d.Description.Value
	}
	if d.Policy != nil {
		wr.Spec.Policy = d.Policy.Name
	}
	l.sm.set(KeyWorkflow(name), d.Pos)

	// Param/result types and the effects { } clause are diagnostic-only for the
	// resource projection (checking is #198/#190); record positions, do not
	// synthesize resource fields.
	for _, p := range d.Params {
		if p.Name != nil {
			l.sm.set(KeyWorkflowType(name, p.Name.Name), p.Pos)
		}
	}
	if d.Result != nil {
		l.sm.set(KeyWorkflowType(name, "result"), d.Result.Pos)
	}
	for _, e := range d.Effects {
		l.sm.set(KeyWorkflowEffect(name, e.Name), e.Pos)
	}

	wl := &workflowLowerer{
		l:    l,
		wf:   name,
		env:  newEnv(d),
		used: map[string]int{},
	}
	wl.reserveBindings(d.Body)
	wl.lowerBody(d.Body)
	wr.Spec.Steps = wl.steps
	wr.Spec.Output = wl.output
	return wr
}

// refEnv maps a leading source identifier to its interpolation prefix (without
// the surrounding ${}). Params resolve to the input root; bindings resolve to
// their step output.
type refEnv struct {
	roots map[string]string
}

func newEnv(d *lang.WorkflowDecl) refEnv {
	roots := map[string]string{}
	switch {
	case len(d.Params) == 1 && d.Params[0].Name != nil:
		// A single parameter names the whole workflow input, so a field access
		// (input.repo) lowers to ${input.repo}. A BARE reference to that
		// parameter would lower to ${input}, which the interpolation language
		// cannot represent — token() diagnoses that case.
		roots[d.Params[0].Name.Name] = "input"
	default:
		for _, p := range d.Params {
			if p.Name != nil {
				roots[p.Name.Name] = "input." + p.Name.Name
			}
		}
	}
	return refEnv{roots: roots}
}

type workflowLowerer struct {
	l      *lowerer
	wf     string
	env    refEnv
	used   map[string]int // reserved + generated step ids
	steps  []spec.WorkflowStep
	output *spec.WorkflowOutput
	// synthetic is true while flattening a control-flow body (if/for/while) into
	// steps; every step lowered under it is marked spec.WorkflowStep.Synthetic (#305)
	// so the validator skips executable-graph checks on the effect-analysis-only
	// projection of control flow.
	synthetic bool
}

// reserveBindings claims every explicit binding name so generated (unbound / SSA
// temporary) step ids never collide with an author's binding.
func (wl *workflowLowerer) reserveBindings(body []lang.Stmt) {
	for _, st := range body {
		switch s := st.(type) {
		case *lang.AssignStmt:
			wl.reserveName(s.Target)
		case *lang.ParallelStmt:
			for _, b := range s.Body {
				wl.reserveName(b.Target)
			}
		}
	}
}

func (wl *workflowLowerer) reserveName(id *lang.Ident) {
	if id == nil || id.Name == "" {
		return
	}
	if _, ok := wl.used[id.Name]; ok {
		wl.l.diag(id.Pos, "duplicate binding %q", id.Name)
		return
	}
	wl.used[id.Name] = 0
}

// lowerBody walks top-level statements, threading a dependency frontier: the
// step ids the next statement must wait for. Sequential statements chain to the
// previous frontier (order-preserving); a parallel block's branches all share
// the pre-block frontier and their union becomes the next frontier (fan-in).
func (wl *workflowLowerer) lowerBody(body []lang.Stmt) {
	var frontier []string
	for _, st := range body {
		switch s := st.(type) {
		case *lang.AssignStmt:
			if id, ok := wl.lowerAssign(s, frontier); ok {
				frontier = []string{id}
			}
		case *lang.ExprStmt:
			call, ok := s.X.(*lang.CallExpr)
			if !ok {
				wl.l.diag(s.Pos, "expression statement is not a call and has no effect")
				continue
			}
			id := wl.freshID(calleeLeaf(call.Callee))
			wl.lowerCall(id, call, frontier, s.Pos)
			frontier = []string{id}
		case *lang.ParallelStmt:
			var next []string
			for _, b := range s.Body {
				if id, ok := wl.lowerAssign(b, frontier); ok {
					next = append(next, id)
				}
			}
			if len(next) > 0 {
				frontier = next
			}
		case *lang.ApprovalStmt:
			wl.lowerApproval(s, frontier)
			frontier = []string{identName(s.Bind)}
		case *lang.IfStmt, *lang.ForStmt, *lang.WhileStmt, *lang.RetryStmt:
			// Control flow does not become a WorkflowStep field (ADR 002 §4); it
			// lowers to the execution IR (LowerExec). The resource projection
			// instead FLATTENS every reachable arm/body into steps so the effect
			// bound computed over it (internal/effects walks these steps) is the
			// union over all branches — a conditional cannot smuggle an
			// unpermitted effect past the effects clause (ADR 002 §5, #199). The
			// flattened steps are a sound over-approximation for effect analysis
			// and plan diffing, not an independently executable graph; execution
			// runs from the execution IR. They are marked Synthetic (#305) so the
			// validator does not hold them to executable-graph invariants (needs
			// wiring, per-field input-schema mapping).
			wl.synthetic = true
			wl.lowerControlStmts([]lang.Stmt{st}, frontier)
			wl.synthetic = false
		case *lang.ReturnStmt:
			wl.output = &spec.WorkflowOutput{Value: wl.outputValueFor(s.Value, frontier)}
		}
	}
}

// lowerControlStmts flattens the statements inside a conditional or loop into
// resource steps (the union over branches; see the IfStmt/ForStmt case in
// lowerBody). Every call becomes a step with a FRESH id — a name bound in both
// arms of an `if` must not collide — so these steps carry structural ids, not
// author binding names, and the projection is not required to be independently
// executable. Effect analysis only reads each step's uses:/agent/workflow, which
// this preserves faithfully.
func (wl *workflowLowerer) lowerControlStmts(body []lang.Stmt, predNeeds []string) {
	for _, st := range body {
		switch s := st.(type) {
		case *lang.AssignStmt:
			wl.lowerControlAssign(s, predNeeds)
		case *lang.ExprStmt:
			if call, ok := s.X.(*lang.CallExpr); ok {
				id := wl.freshID(calleeLeaf(call.Callee))
				wl.lowerCall(id, call, predNeeds, s.Pos)
			} else {
				wl.l.diag(s.Pos, "expression statement is not a call and has no effect")
			}
		case *lang.ParallelStmt:
			for _, b := range s.Body {
				wl.lowerControlAssign(b, predNeeds)
			}
		case *lang.IfStmt:
			wl.lowerControlStmts(s.Then, predNeeds)
			wl.lowerControlStmts(s.Else, predNeeds)
		case *lang.ForStmt:
			if v := identName(s.Var); v != "" {
				// Register the loop variable so references to it inside the body
				// resolve (best-effort) instead of being flagged unresolved.
				wl.env.roots[v] = "loop." + v
			}
			wl.lowerControlStmts(s.Body, predNeeds)
		case *lang.WhileStmt:
			// A bounded while flattens its body the same way (the union over the
			// reachable steps); the iteration bound is orthogonal to the effect
			// bound (ADR 002 §6) and is not represented in the resource projection.
			wl.lowerControlStmts(s.Body, predNeeds)
		case *lang.RetryStmt:
			// A bounded retry-until flattens its body the same way as while: the effect
			// bound is the union over the reachable steps, orthogonal to the iteration
			// bound and the on-exhaustion failure (#361).
			wl.lowerControlStmts(s.Body, predNeeds)
		case *lang.ReturnStmt:
			wl.output = &spec.WorkflowOutput{Value: wl.outputValueFor(s.Value, predNeeds)}
		}
	}
}

// lowerControlAssign lowers an assignment inside a control-flow body, allocating
// a fresh step id (so branch-local bindings never collide) and registering the
// binding's interpolation root best-effort.
func (wl *workflowLowerer) lowerControlAssign(s *lang.AssignStmt, predNeeds []string) {
	name := identName(s.Target)
	switch v := s.Value.(type) {
	case *lang.CallExpr:
		id := wl.freshID(name)
		wl.lowerCall(id, v, predNeeds, s.Pos)
		wl.env.roots[name] = "steps." + id + ".output"
	case *lang.RefExpr:
		wl.env.roots[name] = wl.prefixOf(v)
	case *lang.LitExpr:
		// A literal binding contributes no step and no interpolation root.
	default:
		wl.l.diag(s.Pos, "unsupported binding value")
	}
}

// lowerAssign lowers `target = <call>` (or `target = <ref>` alias) and returns
// the produced step id, or ok=false when no step is produced (an alias).
func (wl *workflowLowerer) lowerAssign(s *lang.AssignStmt, predNeeds []string) (string, bool) {
	name := identName(s.Target)
	switch v := s.Value.(type) {
	case *lang.CallExpr:
		wl.lowerCall(name, v, predNeeds, s.Pos)
		wl.env.roots[name] = "steps." + name + ".output"
		return name, true
	case *lang.RefExpr:
		// An alias binds a name to an existing value; it produces no step.
		wl.env.roots[name] = wl.prefixOf(v)
		return "", false
	default:
		wl.l.diag(s.Pos, "unsupported binding value")
		return "", false
	}
}

// lowerApproval projects an `approval <bind> { … }` statement into a spec.WorkflowStep carrying an
// Approval value (the resource form effect analysis walks), and binds the decision name into the
// workflow env so a later step may reference it. The exec IR gets the matching execir.Approval via
// LowerExec (exec.go). Its needs are the current frontier — an approval pauses after its predecessors.
func (wl *workflowLowerer) lowerApproval(s *lang.ApprovalStmt, predNeeds []string) {
	id := identName(s.Bind)
	cfg := &spec.WorkflowApprovalConfig{}
	if s.Description != nil {
		cfg.Description = s.Description.Value
	}
	for _, k := range s.RedactKeys {
		if k != nil {
			cfg.RedactKeys = append(cfg.RedactKeys, k.Value)
		}
	}
	step := spec.WorkflowStep{
		ID:            id,
		Pos:           s.Pos,
		Approval:      &spec.WorkflowApprovalValue{Enabled: true, Config: cfg},
		NeedsDeclared: true,
		Synthetic:     wl.synthetic,
	}
	// The review payload lowers exactly like a call's arguments (nested calls hoisted into their own
	// SSA-temporary steps first), so a YAML approval's `with:` round-trips through the same path.
	var tempNeeds []string
	if len(s.With) > 0 {
		with := make(map[string]any, len(s.With))
		for i, arg := range s.With {
			with[callArgKey(i, arg, false)] = wl.lowerArg(arg.Value, id, i, predNeeds, &tempNeeds)
		}
		step.With = with
	}
	step.Needs = mergeNeeds(predNeeds, tempNeeds)
	wl.steps = append(wl.steps, step)
	wl.env.roots[id] = "steps." + id + ".output"
	wl.l.sm.set(KeyStep(wl.wf, id), s.Pos)
}

// lowerCall lowers one call into a step, hoisting nested-call arguments into
// their own SSA-temporary steps first, and records the step's needs.
func (wl *workflowLowerer) lowerCall(id string, call *lang.CallExpr, predNeeds []string, pos spec.Pos) {
	step := spec.WorkflowStep{ID: id, Pos: pos, NeedsDeclared: true, Synthetic: wl.synthetic}
	wl.applyCallee(&step, call.Callee)
	step.WholeDocument = step.Agent != "" && isWholeDocumentCall(call.Args)

	var tempNeeds []string
	if len(call.Args) > 0 {
		with := make(map[string]any, len(call.Args))
		for i, arg := range call.Args {
			with[callArgKey(i, arg, step.WholeDocument)] = wl.lowerArg(arg.Value, id, i, predNeeds, &tempNeeds)
		}
		step.With = with
	}
	step.Needs = mergeNeeds(predNeeds, tempNeeds)
	wl.steps = append(wl.steps, step)
	// Anchor the step's source-map entry at the callee (invocation site) — the
	// position downstream ref/policy diagnostics underline (AgentPos/UsesPos),
	// falling back to the statement position.
	anchor := pos
	if call.Callee != nil && !call.Callee.Pos.IsZero() {
		anchor = call.Callee.Pos
	}
	wl.l.sm.set(KeyStep(wl.wf, id), anchor)
}

// applyCallee sets exactly one target field. A dotted callee (github.get_pr) is a
// tool call; a single identifier is a workflow: step when classifyDecls marked it
// a workflow (declared in this file or listed in Options.Workflows), otherwise an
// agent: step.
func (wl *workflowLowerer) applyCallee(step *spec.WorkflowStep, callee *lang.RefExpr) {
	if callee == nil || len(callee.Parts) == 0 {
		wl.l.diag(step.Pos, "call has no callee")
		return
	}
	if len(callee.Parts) >= 2 {
		tool := callee.Parts[0].Name
		ops := make([]string, 0, len(callee.Parts)-1)
		for _, p := range callee.Parts[1:] {
			ops = append(ops, p.Name)
		}
		step.Uses = "tool." + tool + "." + strings.Join(ops, ".")
		step.UsesPos = callee.Pos
		return
	}
	name := callee.Parts[0].Name
	if wl.l.workflowCallee[name] {
		step.Workflow = name
		step.WorkflowPos = callee.Pos
		return
	}
	step.Agent = name
	step.AgentPos = callee.Pos
}

// lowerArg lowers one argument value. A nested call is hoisted into an SSA
// temporary step whose id is the parent id plus the argument path (structural,
// never source-derived), referenced by ${steps.<temp>.output}.
func (wl *workflowLowerer) lowerArg(e lang.Expr, parentID string, argIdx int, predNeeds []string, tempNeeds *[]string) any {
	switch v := e.(type) {
	case *lang.RefExpr:
		return wl.token(v)
	case *lang.LitExpr:
		// A string argument that embeds `${<binding>}` tokens is a template (#316):
		// each token resolves to the resource `${steps.<id>.output.…}` form and the
		// referenced step becomes a predecessor. A plain literal lowers to its Go
		// value directly.
		if s, ok := v.Value.(string); ok && interpTokenRE.MatchString(s) {
			return wl.interpolateArg(s, v.Pos, tempNeeds)
		}
		return v.Value
	case *lang.CallExpr:
		id := wl.freshID(parentID + "_arg" + strconv.Itoa(argIdx))
		wl.lowerCall(id, v, predNeeds, v.Pos)
		*tempNeeds = append(*tempNeeds, id)
		return stepToken(id)
	case *lang.ObjectExpr:
		// An object literal as an argument (issue #440) lowers field-by-field, mirroring lowerArg so a
		// nested call/ref inside it hoists and tracks predecessors like any other argument value.
		out := make(map[string]any, len(v.Fields))
		for i, f := range v.Fields {
			if f == nil || f.Key == nil {
				continue
			}
			out[f.Key.Name] = wl.lowerArg(f.Value, parentID+"_"+f.Key.Name, i, predNeeds, tempNeeds)
		}
		return out
	default:
		wl.l.diag(e.Position(), "unsupported argument expression")
		return ""
	}
}

// lowerValue lowers a return/top-level expression, hoisting a call the same way
// lowerArg does.
func (wl *workflowLowerer) lowerValue(e lang.Expr, idBase string, predNeeds []string) any {
	switch v := e.(type) {
	case *lang.RefExpr:
		return wl.token(v)
	case *lang.LitExpr:
		return v.Value
	case *lang.CallExpr:
		id := wl.freshID(idBase + "_" + calleeLeaf(v.Callee))
		wl.lowerCall(id, v, predNeeds, v.Pos)
		return stepToken(id)
	case *lang.ObjectExpr:
		return wl.objectFieldsMap(v, idBase, predNeeds)
	default:
		wl.l.diag(e.Position(), "unsupported expression")
		return ""
	}
}

// objectFieldsMap lowers an object literal's fields into a map[string]any (each value lowered like any
// other resource value: refs → interpolation tokens, nested calls hoisted to steps). Issue #440.
func (wl *workflowLowerer) objectFieldsMap(v *lang.ObjectExpr, idBase string, predNeeds []string) map[string]any {
	out := make(map[string]any, len(v.Fields))
	for _, f := range v.Fields {
		if f == nil || f.Key == nil {
			continue
		}
		out[f.Key.Name] = wl.lowerValue(f.Value, idBase+"_"+f.Key.Name, predNeeds)
	}
	return out
}

// outputValueFor builds a workflow output's Value map for a `return <expr>`. An object literal becomes
// the value map directly (`return {a: x}` → `{a: …}`), matching a YAML `output.value: {a}`; any other
// expression uses the single-`value` envelope (`return x` → `{value: …}`) the scalar convention needs
// (issue #440).
func (wl *workflowLowerer) outputValueFor(e lang.Expr, predNeeds []string) map[string]any {
	if obj, ok := e.(*lang.ObjectExpr); ok {
		return wl.objectFieldsMap(obj, "return", predNeeds)
	}
	return map[string]any{"value": wl.lowerValue(e, "return", predNeeds)}
}

// token renders a reference as an interpolation string, e.g. result.summary ->
// ${steps.result.output.summary}, input.repo -> ${input.repo}.
//
// A reference resolving to the whole workflow input (the path "input" alone, from
// the single-parameter `input` or an alias of it) is legitimate (#303): the execir
// path binds the whole input document to that parameter (paramScope) and resolves
// Ref{["input"]} to it, and the execir path is what runs. The resource projection
// is a sound over-approximation for effect analysis and is no longer executed — the
// WorkflowStep DAG runtime was retired (#278) — so a ${input} token in a with-map
// is inert (no run-time resolvePath to fail-close against). It is therefore emitted
// as ${input} rather than reported as a diagnostic, so the flagship's
// `state = input; Implementer(state)` compiles and runs.
func (wl *workflowLowerer) token(r *lang.RefExpr) any {
	return "${" + wl.prefixOf(r) + "}"
}

// interpolateArg rewrites a template string argument (#316) into the resource
// projection's interpolation form: each `${<binding>.<field>…}` token has its head
// binding resolved through env.roots (the same map prefixOf uses) to a
// `${steps.<id>.output.…}` (or `${input.…}`) token, and the referenced step is added
// to the consumer step's needs so the reference is a valid predecessor.
func (wl *workflowLowerer) interpolateArg(s string, pos spec.Pos, tempNeeds *[]string) string {
	return interpTokenRE.ReplaceAllStringFunc(s, func(tok string) string {
		m := interpTokenRE.FindStringSubmatch(tok)
		if m == nil {
			return tok
		}
		return "${" + wl.resolveTemplatePath(strings.TrimSpace(m[1]), pos, tempNeeds) + "}"
	})
}

func (wl *workflowLowerer) resolveTemplatePath(inner string, pos spec.Pos, tempNeeds *[]string) string {
	parts := strings.Split(inner, ".")
	head := parts[0]
	prefix, ok := wl.env.roots[head]
	if !ok {
		wl.l.diag(pos, "unresolved reference %q in interpolation", head)
		prefix = head
	}
	if id := stepIDFromPrefix(prefix); id != "" {
		*tempNeeds = append(*tempNeeds, id)
	}
	if len(parts) == 1 {
		return prefix
	}
	return prefix + "." + strings.Join(parts[1:], ".")
}

// stepIDFromPrefix extracts the step id from a `steps.<id>.output` interpolation
// prefix, or "" when the prefix is not a step reference (e.g. `input`).
func stepIDFromPrefix(prefix string) string {
	rest, ok := strings.CutPrefix(prefix, "steps.")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// prefixOf resolves a reference to its interpolation path (no ${}). An unresolved
// leading identifier yields a diagnostic and a best-effort literal path.
func (wl *workflowLowerer) prefixOf(r *lang.RefExpr) string {
	if r == nil || len(r.Parts) == 0 {
		return ""
	}
	head := r.Parts[0].Name
	prefix, ok := wl.env.roots[head]
	if !ok {
		wl.l.diag(r.Pos, "unresolved reference %q", head)
		prefix = head
	}
	rest := r.Parts[1:]
	if len(rest) == 0 {
		return prefix
	}
	parts := make([]string, 0, len(rest)+1)
	parts = append(parts, prefix)
	for _, p := range rest {
		parts = append(parts, p.Name)
	}
	return strings.Join(parts, ".")
}

// freshID returns base if unused, else base_1, base_2, … — deterministic in
// source order and independent of source location.
func (wl *workflowLowerer) freshID(base string) string {
	if base == "" {
		base = "step"
	}
	if _, ok := wl.used[base]; !ok {
		wl.used[base] = 0
		return base
	}
	for {
		wl.used[base]++
		cand := base + "_" + strconv.Itoa(wl.used[base])
		if _, ok := wl.used[cand]; !ok {
			wl.used[cand] = 0
			return cand
		}
	}
}

// --- helpers ----------------------------------------------------------------

func identName(id *lang.Ident) string {
	if id == nil {
		return ""
	}
	return id.Name
}

func calleeLeaf(callee *lang.RefExpr) string {
	if callee == nil || len(callee.Parts) == 0 {
		return "call"
	}
	return callee.Parts[len(callee.Parts)-1].Name
}

func stepToken(id string) string { return "${steps." + id + ".output}" }

// mergeNeeds concatenates predecessor and temp needs, de-duplicating while
// preserving first-seen order.
func mergeNeeds(pred, temp []string) []string {
	if len(pred) == 0 && len(temp) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(pred)+len(temp))
	out := make([]string, 0, len(pred)+len(temp))
	for _, group := range [][]string{pred, temp} {
		for _, id := range group {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
