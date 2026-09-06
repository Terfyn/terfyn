package plan

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/tools"
)

type policySpecRisk struct {
	Execution *struct {
		MaxTotalCostUsd     float64 `json:"maxTotalCostUsd"`
		MaxWallClockSeconds int     `json:"maxWallClockSeconds"`
		MaxIterations       int     `json:"maxIterations"`
	} `json:"execution"`
	Approvals *struct {
		RequiredFor []string `json:"requiredFor"`
	} `json:"approvals"`
	Effects *struct {
		Permit             []string `json:"permit"`
		PermitWithApproval []string `json:"permitWithApproval"`
	} `json:"effects"`
}

type agentSpecRisk struct {
	Model string   `json:"model"`
	Tools []string `json:"tools"`
}

type toolSpecRisk struct {
	Safety *struct {
		Trusted          *bool `json:"trusted"`
		SideEffects      *bool `json:"sideEffects"`
		RequiresApproval *bool `json:"requiresApproval"`
	} `json:"safety"`
}

type jsonEnvelope struct {
	Spec json.RawMessage `json:"spec"`
}

type riskSink struct {
	items []RiskItem
	seen  map[string]struct{}
}

func newRiskSink() *riskSink {
	return &riskSink{seen: map[string]struct{}{}}
}

func (s *riskSink) add(it RiskItem) {
	it.Reason = strings.TrimSpace(it.Reason)
	if it.Reason == "" {
		return
	}
	key := string(it.Category) + "\x00" + string(it.Target.Kind) + "/" + it.Target.Name + "\x00" + it.Reason
	if _, ok := s.seen[key]; ok {
		return
	}
	s.seen[key] = struct{}{}
	s.items = append(s.items, it)
}

func summarizeRisks(
	g *spec.ProjectGraph,
	appliedByID map[string]state.AppliedResource,
	desiredByID map[string]desiredRow,
	ops []Operation,
) RiskSummary {
	sink := newRiskSink()

	// The project's default runtime target (spec.defaults.runtime) applies to every workflow with
	// no explicit spec.runtime, so a default flip is a runtime-target change that no per-workflow
	// diff can see (the unset workflows' own spec is byte-unchanged). Resolve the prior and current
	// defaults once so the workflow detector uses the right side of each, and so a default flip is
	// surfaced once at project scope rather than N times.
	oldDef, newDef, projName, hadProject := projectRuntimeDefaults(appliedByID, desiredByID)

	for _, op := range ops {
		key := resourceMapKey(op.Target.Kind, op.Target.Name)
		des := desiredByID[key]
		prev, hadPrev := appliedByID[key]

		var oldJSON string
		if hadPrev {
			oldJSON = prev.NormalizedSpecJSON
		}

		switch op.Target.Kind {
		case spec.KindPolicy:
			summarizePolicyRisk(sink, op, oldJSON, des.json, hadPrev)
		case spec.KindAgent:
			summarizeAgentRisk(sink, g, op, oldJSON, des.json, hadPrev)
		case spec.KindTool:
			summarizeToolRisk(sink, g, op, oldJSON, des.json, hadPrev)
		case spec.KindWorkflow:
			summarizeWorkflowRisk(sink, op, oldJSON, des.json, hadPrev, oldDef, newDef)
		}
	}

	if hadProject {
		summarizeProjectRuntimeRisk(sink, projName, oldDef, newDef)
	}

	return finalizeRiskItems(sink.items)
}

// projectRuntimeDefaults resolves the prior and current spec.defaults.runtime from the Project
// resource rows. hadProject is true when a prior Project row exists (so a default *change* can be
// diffed); on a first apply there is no prior and the current default is shown only by the plan's
// Runtime targets section.
func projectRuntimeDefaults(appliedByID map[string]state.AppliedResource, desiredByID map[string]desiredRow) (oldDef, newDef, projName string, hadProject bool) {
	for key, d := range desiredByID {
		if d.id.Kind != spec.KindProject {
			continue
		}
		projName = d.id.Name
		newDef = parseDefaultRuntime(d.json)
		if prev, ok := appliedByID[key]; ok {
			hadProject = true
			oldDef = parseDefaultRuntime(prev.NormalizedSpecJSON)
		}
		return
	}
	return
}

func mergePolicyLintRisk(g *spec.ProjectGraph, risk RiskSummary) RiskSummary {
	findings := policy.Lint(g)
	if len(findings) == 0 {
		return risk
	}
	sink := newRiskSink()
	for _, it := range risk.Items {
		sink.add(it)
	}
	for _, f := range findings {
		reason := strings.TrimSpace(f.Message)
		if reason == "" {
			reason = string(f.Rule)
		}
		if loc := f.Pos.String(); loc != "" {
			reason = loc + ": " + reason
		}
		name := strings.TrimSpace(f.Policy)
		kind := RiskTargetPolicy
		wkind := WitnessKindPolicy
		if name == "" {
			name = strings.TrimSpace(f.Tool)
			kind = RiskTargetTool
			wkind = WitnessKindTool
		}
		sink.add(RiskItem{
			Category: RiskCategoryLint,
			Severity: RiskSeverity(f.Severity),
			Reason:   reason,
			Target:   RiskTarget{Kind: kind, Name: name},
			Witness:  staticResourceWitness(wkind, name),
		})
	}
	out := finalizeRiskItems(sink.items)
	out.Lint = findings
	return out
}

func finalizeRiskItems(items []RiskItem) RiskSummary {
	sort.SliceStable(items, func(i, j int) bool {
		if riskSevRank(items[i].Severity) != riskSevRank(items[j].Severity) {
			return riskSevRank(items[i].Severity) < riskSevRank(items[j].Severity)
		}
		if items[i].Category != items[j].Category {
			return items[i].Category < items[j].Category
		}
		if items[i].Target.Kind != items[j].Target.Kind {
			return items[i].Target.Kind < items[j].Target.Kind
		}
		if items[i].Target.Name != items[j].Target.Name {
			return items[i].Target.Name < items[j].Target.Name
		}
		return items[i].Reason < items[j].Reason
	})
	msgs := make([]string, 0, len(items))
	for _, it := range items {
		msgs = append(msgs, it.Reason)
	}
	return RiskSummary{Messages: msgs, Items: items}
}

func riskSevRank(s RiskSeverity) int {
	switch s {
	case RiskSeverityHigh:
		return 0
	case RiskSeverityMedium:
		return 1
	case RiskSeverityLow:
		return 2
	default:
		return 3
	}
}

func staticResourceWitness(kind WitnessHopKind, name string) []WitnessHop {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return []WitnessHop{{
		Kind:         kind,
		Name:         name,
		Reachability: WitnessStatic,
	}}
}

func summarizePolicyRisk(sink *riskSink, op Operation, oldJSON, newJSON string, hadPrev bool) {
	newPol, ok := parsePolicySpec(newJSON)
	if !ok {
		return
	}
	name := op.Target.Name
	target := RiskTarget{Kind: RiskTargetPolicy, Name: name}
	wit := staticResourceWitness(WitnessKindPolicy, name)
	newCost := policyMaxCost(newPol)
	newWall := policyMaxWall(newPol)
	newApprovals := policyApprovals(newPol)

	if op.Action == ActionCreate || !hadPrev {
		if newCost > 0 {
			sink.add(RiskItem{
				Category: RiskCategorySafety,
				Severity: RiskSeverityLow,
				Reason:   fmt.Sprintf("New policy defines a cost ceiling (Policy/%s).", name),
				Target:   target,
				Witness:  wit,
			})
		}
		if newWall > 0 {
			sink.add(RiskItem{
				Category: RiskCategorySafety,
				Severity: RiskSeverityLow,
				Reason:   fmt.Sprintf("New policy defines a wall-clock ceiling (Policy/%s).", name),
				Target:   target,
				Witness:  wit,
			})
		}
		if len(newApprovals) > 0 {
			sink.add(RiskItem{
				Category: RiskCategorySafety,
				Severity: RiskSeverityLow,
				Reason:   fmt.Sprintf("New policy defines approval requirements (Policy/%s).", name),
				Target:   target,
				Witness:  wit,
			})
		}
		// A new policy that raises the iteration ceiling above the default 32 is a budget relaxation
		// versus the no-policy baseline; an explicit ceiling at or below 32 is a tightening note (#522).
		if newIter := policyIterCeiling(newPol); newIter > spec.HardAgentMaxIterations {
			sink.add(RiskItem{
				Category: RiskCategoryBudgetRelaxation,
				Severity: RiskSeverityHigh,
				Reason:   fmt.Sprintf("New policy raises the iteration ceiling to %d, above the default %d (Policy/%s).", newIter, spec.HardAgentMaxIterations, name),
				Target:   target,
				Witness:  wit,
			})
		} else if policyMaxIter(newPol) > 0 {
			sink.add(RiskItem{
				Category: RiskCategorySafety,
				Severity: RiskSeverityLow,
				Reason:   fmt.Sprintf("New policy defines an iteration ceiling (Policy/%s).", name),
				Target:   target,
				Witness:  wit,
			})
		}
		return
	}

	oldPol, ok := parsePolicySpec(oldJSON)
	if !ok {
		return
	}
	oldCost := policyMaxCost(oldPol)
	oldWall := policyMaxWall(oldPol)
	oldApprovals := policyApprovals(oldPol)

	// In PolicyExecution, 0/absent means "unbounded" — checkExecutionBudgets only
	// enforces a positive ceiling — so 0 is the MOST permissive value, not a ceiling
	// of zero. A budget relaxation is a move to a more permissive (higher, or newly
	// unbounded) ceiling; adding a finite ceiling where none existed is a tightening
	// and must NOT flag, while removing one (finite → unbounded) is a real relaxation
	// that must (#382). Compare effective ceilings with 0 mapped to +Inf.
	oldCostEff, newCostEff := effectiveCeiling(oldCost), effectiveCeiling(newCost)
	if newCostEff > oldCostEff+1e-9 {
		reason := fmt.Sprintf("Cost ceiling increased (Policy/%s).", name)
		if math.IsInf(newCostEff, 1) {
			reason = fmt.Sprintf("Cost ceiling removed — now unbounded (Policy/%s).", name)
		}
		sink.add(RiskItem{
			Category: RiskCategoryBudgetRelaxation,
			Severity: RiskSeverityHigh,
			Reason:   reason,
			Target:   target,
			Witness:  wit,
		})
	}
	oldWallEff, newWallEff := effectiveCeiling(float64(oldWall)), effectiveCeiling(float64(newWall))
	if newWallEff > oldWallEff+1e-9 {
		reason := fmt.Sprintf("Wall-clock ceiling increased (Policy/%s).", name)
		if math.IsInf(newWallEff, 1) {
			reason = fmt.Sprintf("Wall-clock ceiling removed — now unbounded (Policy/%s).", name)
		}
		sink.add(RiskItem{
			Category: RiskCategoryBudgetRelaxation,
			Severity: RiskSeverityHigh,
			Reason:   reason,
			Target:   target,
			Witness:  wit,
		})
	}
	// Iteration ceiling: 0/absent is the default 32 (not unbounded), so raising the EFFECTIVE ceiling —
	// 8→64, or removing an explicit 8 so it rises to 32 — is a budget relaxation the review gate sees;
	// 64→0 (falls to 32) is a tightening and must not flag (#522).
	if oldIter, newIter := policyIterCeiling(oldPol), policyIterCeiling(newPol); newIter > oldIter {
		sink.add(RiskItem{
			Category: RiskCategoryBudgetRelaxation,
			Severity: RiskSeverityHigh,
			Reason:   fmt.Sprintf("Iteration ceiling increased from %d to %d (Policy/%s).", oldIter, newIter, name),
			Target:   target,
			Witness:  wit,
		})
	}
	for _, a := range oldApprovals {
		if containsString(newApprovals, a) {
			continue
		}
		sink.add(RiskItem{
			Category: RiskCategoryApprovalRemoval,
			Severity: RiskSeverityHigh,
			Reason:   fmt.Sprintf("Approval requirements removed for %q (Policy/%s).", a, name),
			Target:   target,
			Witness:  wit,
		})
	}
	addEffectPermitWidening(sink, name, target, wit, oldPol, newPol)
}

func summarizeAgentRisk(sink *riskSink, g *spec.ProjectGraph, op Operation, oldJSON, newJSON string, hadPrev bool) {
	newAg, ok := parseAgentSpec(newJSON)
	if !ok {
		return
	}
	name := op.Target.Name
	target := RiskTarget{Kind: RiskTargetAgent, Name: name}
	wit := staticResourceWitness(WitnessKindAgent, name)
	newModel := strings.TrimSpace(newAg.Model)
	newTools := agentTools(newAg)

	if op.Action == ActionCreate || !hadPrev {
		if newModel != "" {
			sink.add(RiskItem{
				Category: RiskCategorySafety,
				Severity: RiskSeverityLow,
				Reason:   fmt.Sprintf("New agent binds a model (Agent/%s).", name),
				Target:   target,
				Witness:  wit,
			})
		}
		for _, toolName := range newTools {
			sink.add(toolSurfaceItem(g, name, toolName, target, wit))
		}
		return
	}

	oldAg, ok := parseAgentSpec(oldJSON)
	if !ok {
		return
	}
	oldModel := strings.TrimSpace(oldAg.Model)
	if newModel != oldModel && (newModel != "" || oldModel != "") {
		sink.add(RiskItem{
			Category: RiskCategoryModelChange,
			Severity: RiskSeverityMedium,
			Reason:   fmt.Sprintf("Agent model changed (Agent/%s).", name),
			Target:   target,
			Witness:  wit,
		})
	}
	oldSet := make(map[string]struct{}, len(oldAg.Tools))
	for _, t := range agentTools(oldAg) {
		oldSet[t] = struct{}{}
	}
	for _, toolName := range newTools {
		if _, ok := oldSet[toolName]; ok {
			continue
		}
		sink.add(toolSurfaceItem(g, name, toolName, target, wit))
	}
}

// summarizeWorkflowRisk surfaces a runtime-target *change* driven by the workflow's own
// spec.runtime (issue #342). The runtime is replaceable but the authority is not: the effect bound
// is computed from the graph and is identical whichever runtime runs it, so a runtime change is an
// execution-substrate change, not an authority widening. It is surfaced rather than hidden, matching
// the honesty boundary (ADR 004 §5).
//
// It fires only when the workflow's own spec.runtime changed, resolving each side against its own
// project default (oldDef/newDef). A move caused solely by a project-default flip (own field
// unchanged) is left to summarizeProjectRuntimeRisk, so it is surfaced once at project scope rather
// than once per unset workflow, and a workflow whose effective target is unchanged emits nothing.
// The current selection on a fresh create is shown by the plan's Runtime targets section, not a
// risk item — so plan output for the same program under two runtimes differs only by that line.
func summarizeWorkflowRisk(sink *riskSink, op Operation, oldJSON, newJSON string, hadPrev bool, oldDef, newDef string) {
	if op.Action == ActionCreate || !hadPrev {
		return
	}
	newRT, ok := parseWorkflowRuntime(newJSON)
	if !ok {
		return
	}
	oldRT, ok := parseWorkflowRuntime(oldJSON)
	if !ok {
		return
	}
	if oldRT == newRT {
		return // own field unchanged; a default-driven move is reported at project scope
	}
	oldEff, newEff := effectiveRuntime(oldRT, oldDef), effectiveRuntime(newRT, newDef)
	if oldEff == newEff {
		return
	}
	name := op.Target.Name
	sink.add(RiskItem{
		Category: RiskCategoryRuntimeTargetChange,
		Severity: RiskSeverityMedium,
		Reason:   fmt.Sprintf("Workflow runtime target changed %q → %q (Workflow/%s); same authority bound, different execution substrate.", oldEff, newEff, name),
		Target:   RiskTarget{Kind: RiskTargetWorkflow, Name: name},
		Witness:  staticResourceWitness(WitnessKindWorkflow, name),
	})
}

// summarizeProjectRuntimeRisk surfaces a project-default runtime flip (issue #342): a change to
// spec.defaults.runtime moves every workflow with no explicit spec.runtime, which no per-workflow
// diff can see. It is reported once at project scope. Same authority bound, different substrate.
func summarizeProjectRuntimeRisk(sink *riskSink, projName, oldDef, newDef string) {
	oldEff, newEff := effectiveRuntime("", oldDef), effectiveRuntime("", newDef)
	if oldEff == newEff {
		return
	}
	sink.add(RiskItem{
		Category: RiskCategoryRuntimeTargetChange,
		Severity: RiskSeverityMedium,
		Reason:   fmt.Sprintf("Project default runtime target changed %q → %q (Project/%s); every workflow without an explicit spec.runtime moves accordingly.", oldEff, newEff, projName),
		Target:   RiskTarget{Kind: RiskTargetProject, Name: projName},
	})
}

func parseWorkflowRuntime(resourceJSON string) (string, bool) {
	var env jsonEnvelope
	if err := json.Unmarshal([]byte(resourceJSON), &env); err != nil {
		return "", false
	}
	var w struct {
		Runtime string `json:"runtime"`
	}
	if err := json.Unmarshal(env.Spec, &w); err != nil {
		return "", false
	}
	return strings.TrimSpace(w.Runtime), true
}

// parseDefaultRuntime extracts spec.defaults.runtime from a Project resource's normalized JSON
// ("" when absent or unparseable).
func parseDefaultRuntime(resourceJSON string) string {
	var env jsonEnvelope
	if err := json.Unmarshal([]byte(resourceJSON), &env); err != nil {
		return ""
	}
	var p struct {
		Defaults *struct {
			Runtime string `json:"runtime"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(env.Spec, &p); err != nil || p.Defaults == nil {
		return ""
	}
	return strings.TrimSpace(p.Defaults.Runtime)
}

func toolSurfaceItem(g *spec.ProjectGraph, agentName, toolName string, target RiskTarget, wit []WitnessHop) RiskItem {
	sev := RiskSeverityMedium
	reason := fmt.Sprintf("Agent tools list gained %q (Agent/%s).", toolName, agentName)
	// A tool that declares side effects (safety.sideEffects) is write-capable. This is the declared
	// capability signal (ADR 007 step 1) that replaced the removed spec.permissions allow-name heuristic:
	// an agent gaining a side-effecting tool widens its authority and is high severity.
	if toolHasSideEffects(g, toolName) {
		sev = RiskSeverityHigh
		reason = fmt.Sprintf("Agent tools list gained write-capable tool %q (Agent/%s; declares side effects).", toolName, agentName)
	}
	return RiskItem{
		Category: RiskCategoryToolSurfaceChange,
		Severity: sev,
		Reason:   reason,
		Target:   target,
		Witness:  wit,
	}
}

func summarizeToolRisk(sink *riskSink, g *spec.ProjectGraph, op Operation, oldJSON, newJSON string, hadPrev bool) {
	if _, ok := parseToolSpec(newJSON); !ok {
		return
	}
	name := op.Target.Name
	target := RiskTarget{Kind: RiskTargetTool, Name: name}
	wit := staticResourceWitness(WitnessKindTool, name)
	newDecision := toolPlanDecisionFromGraph(g, name)

	if op.Action == ActionCreate || !hadPrev {
		addToolSafetyRisk(sink, name, newDecision, nil, target, wit)
		return
	}

	oldTool, ok := parseToolSpec(oldJSON)
	if !ok {
		return
	}
	oldDecision := toolDecisionFromParsed(g, name, oldTool)
	addToolSafetyRisk(sink, name, newDecision, &oldDecision, target, wit)
}

func toolPlanDecisionFromGraph(g *spec.ProjectGraph, toolName string) policy.ToolDecision {
	if g != nil {
		for _, pr := range g.Policies {
			if pr == nil {
				continue
			}
			td := policy.EffectiveToolDecision(g, &pr.Spec, toolName)
			if td.Decision == policy.DecisionRequireApproval {
				return td
			}
		}
	}
	return policy.EffectiveToolDecision(g, nil, toolName)
}

func toolDecisionFromParsed(g *spec.ProjectGraph, toolName string, parsed *toolSpecRisk) policy.ToolDecision {
	if g != nil {
		for _, pr := range g.Policies {
			if pr == nil {
				continue
			}
			td := policy.EffectiveToolDecision(g, &pr.Spec, toolName)
			if td.Decision == policy.DecisionRequireApproval {
				return td
			}
		}
	}
	var safety *spec.ToolSafety
	src := policy.SourceFailClosedDefault
	if parsed != nil && parsed.Safety != nil {
		safety = &spec.ToolSafety{
			Trusted:          parsed.Safety.Trusted,
			SideEffects:      parsed.Safety.SideEffects,
			RequiresApproval: parsed.Safety.RequiresApproval,
		}
		src = policy.SourceSafetyMetadata
	}
	resolved := spec.ResolveToolSafety(safety)
	return policy.ToolDecision{
		Decision: policy.Derive(resolved),
		Source:   src,
		Safety:   resolved,
	}
}

func addToolSafetyRisk(sink *riskSink, toolName string, cur policy.ToolDecision, prev *policy.ToolDecision, target RiskTarget, wit []WitnessHop) {
	if cur.Decision != policy.DecisionRequireApproval {
		return
	}
	if prev != nil && prev.Decision == policy.DecisionRequireApproval {
		return
	}
	// Plan uses prefix match on tool.<name>. for explicit requiredFor (conservative); runtime matches exact uses.
	sink.add(RiskItem{
		Category: RiskCategorySafety,
		Severity: RiskSeverityMedium,
		Reason: fmt.Sprintf(
			"Tool/%s will require approval at run (decision=%s, source=%s).",
			toolName, cur.Decision, cur.Source,
		),
		Target:  target,
		Witness: wit,
	})
}

func parsePolicySpec(resourceJSON string) (*policySpecRisk, bool) {
	var env jsonEnvelope
	if err := json.Unmarshal([]byte(resourceJSON), &env); err != nil {
		return nil, false
	}
	var p policySpecRisk
	if err := json.Unmarshal(env.Spec, &p); err != nil {
		return nil, false
	}
	return &p, true
}

func parseAgentSpec(resourceJSON string) (*agentSpecRisk, bool) {
	var env jsonEnvelope
	if err := json.Unmarshal([]byte(resourceJSON), &env); err != nil {
		return nil, false
	}
	var a agentSpecRisk
	if err := json.Unmarshal(env.Spec, &a); err != nil {
		return nil, false
	}
	return &a, true
}

func parseToolSpec(resourceJSON string) (*toolSpecRisk, bool) {
	var env jsonEnvelope
	if err := json.Unmarshal([]byte(resourceJSON), &env); err != nil {
		return nil, false
	}
	var t toolSpecRisk
	if err := json.Unmarshal(env.Spec, &t); err != nil {
		return nil, false
	}
	return &t, true
}

// effectiveCeiling maps a PolicyExecution budget to the bound it actually enforces:
// 0/absent means "unbounded" (checkExecutionBudgets enforces only a positive value),
// so it is the most permissive ceiling — returned as +Inf so a relaxation comparison
// (a newer, more permissive ceiling) orders correctly instead of reading 0 as a
// ceiling of zero (#382).
func effectiveCeiling(v float64) float64 {
	if v <= 0 {
		return math.Inf(1)
	}
	return v
}

func policyMaxCost(p *policySpecRisk) float64 {
	if p == nil || p.Execution == nil {
		return 0
	}
	return p.Execution.MaxTotalCostUsd
}

func policyMaxWall(p *policySpecRisk) int {
	if p == nil || p.Execution == nil {
		return 0
	}
	return p.Execution.MaxWallClockSeconds
}

// policyMaxIter is a policy's raw execution.maxIterations (0 when unset).
func policyMaxIter(p *policySpecRisk) int {
	if p == nil || p.Execution == nil {
		return 0
	}
	return p.Execution.MaxIterations
}

// policyIterCeiling is a policy's EFFECTIVE iteration ceiling for risk comparison. Unlike cost/wall,
// 0/absent is not "unbounded" — it is the default HardAgentMaxIterations (32). Raising the effective
// ceiling (e.g. 8→64, or removing an explicit 8 so it rises to 32) is a budget relaxation (#522).
func policyIterCeiling(p *policySpecRisk) int {
	if p == nil || p.Execution == nil {
		return spec.HardAgentMaxIterations
	}
	return spec.EffectiveMaxIterationsCeiling(p.Execution.MaxIterations)
}

func policyApprovals(p *policySpecRisk) []string {
	if p == nil || p.Approvals == nil {
		return nil
	}
	return p.Approvals.RequiredFor
}

func addEffectPermitWidening(sink *riskSink, name string, target RiskTarget, wit []WitnessHop, oldPol, newPol *policySpecRisk) {
	oldUnattended := policyUnattendedEffectPermits(oldPol)
	oldAllowed := policyAnyEffectPermits(oldPol)
	flagged := map[string]struct{}{}
	flag := func(ident string) {
		ident = strings.TrimSpace(ident)
		if ident == "" {
			return
		}
		if _, ok := flagged[ident]; ok {
			return
		}
		flagged[ident] = struct{}{}
		sink.add(RiskItem{
			Category: RiskCategoryEffectPermitWidening,
			Severity: RiskSeverityHigh,
			Reason:   fmt.Sprintf("Effect permit widened with %q (Policy/%s).", ident, name),
			Target:   target,
			Witness:  wit,
		})
	}
	// Unattended permit not already covered by an old unattended permit
	// (includes promoting an ident from permitWithApproval to permit).
	for _, ident := range policyUnattendedEffectPermits(newPol) {
		if effectIdentCovered(oldUnattended, ident) {
			continue
		}
		flag(ident)
	}
	// Newly allowed at all (either list) vs the old union.
	for _, ident := range policyAnyEffectPermits(newPol) {
		if effectIdentCovered(oldAllowed, ident) {
			continue
		}
		flag(ident)
	}
}

func policyAnyEffectPermits(p *policySpecRisk) []string {
	if p == nil || p.Effects == nil {
		return nil
	}
	var out []string
	out = append(out, p.Effects.Permit...)
	out = append(out, p.Effects.PermitWithApproval...)
	return out
}

func policyUnattendedEffectPermits(p *policySpecRisk) []string {
	if p == nil || p.Effects == nil {
		return nil
	}
	var out []string
	for _, ident := range p.Effects.Permit {
		if effectIdentCovered(p.Effects.PermitWithApproval, ident) {
			continue
		}
		out = append(out, ident)
	}
	return out
}

func effectIdentCovered(list []string, ident string) bool {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return false
	}
	for _, p := range list {
		if spec.EffectCovers(p, ident) {
			return true
		}
	}
	return false
}

func agentTools(a *agentSpecRisk) []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.Tools))
	for _, t := range a.Tools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// toolHasSideEffects reports whether the named tool declares side effects (safety.sideEffects: true) —
// the declared write-capability signal (ADR 007 step 1) that replaced the removed spec.permissions
// allow-name heuristic. Side-effect metadata is materialized during normalization, so a native tool
// with no explicit safety carries its derived default here.
func toolHasSideEffects(g *spec.ProjectGraph, toolRef string) bool {
	if g == nil {
		return false
	}
	// A grant may be operation-pinned (tool.<name>.<op>, the `.agent` convention) or a bare tool name
	// (legacy YAML `tools: [<name>]`). Resolve the pinned form to its base tool before the g.Tools
	// lookup — exactly as the effect analysis does via tools.ParseUses — so a write-capable tool granted
	// by its pinned reference is not silently missed (which would fail open, under-scoring a genuine
	// tool-surface widening HIGH→MEDIUM).
	name := strings.TrimSpace(toolRef)
	if base, _, err := tools.ParseUses(toolRef); err == nil {
		name = base
	}
	tr := g.Tools[name]
	if tr == nil || tr.Spec.Safety == nil || tr.Spec.Safety.SideEffects == nil {
		return false
	}
	return *tr.Spec.Safety.SideEffects
}

func containsString(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}
