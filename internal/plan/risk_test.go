package plan

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

func graphWithPolicy(cost float64) *spec.ProjectGraph {
	return graphWithPolicyBudget(cost, 0, nil)
}

func graphWithPolicyBudget(cost float64, wall int, requiredFor []string) *spec.ProjectGraph {
	g := minimalGraph()
	ps := spec.PolicySpec{}
	if cost > 0 || wall > 0 {
		ps.Execution = &spec.PolicyExecution{MaxTotalCostUsd: cost, MaxWallClockSeconds: wall}
	}
	if len(requiredFor) > 0 {
		ps.Approvals = &spec.PolicyApprovals{RequiredFor: requiredFor}
	}
	g.Policies["default"] = &spec.PolicyResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindPolicy,
		Metadata:   spec.Metadata{Name: "default"},
		Spec:       ps,
	}
	return g
}

func TestRiskSummary_costCeilingIncreased(t *testing.T) {
	oldG := graphWithPolicy(3.0)
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicy(10.0)

	p := NewPlanner(&fakeDeploy{list: applied})
	pl, err := p.ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(pl.Risk.Messages, " ")
	if !strings.Contains(strings.ToLower(joined), "cost ceiling increased") {
		t.Fatalf("expected cost ceiling risk, got %#v", pl.Risk.Messages)
	}
}

// TestRiskSummary_addingCeilingIsNotRelaxation proves that adding a cost/wall-clock
// ceiling where none existed (0/unbounded → finite) is a tightening, not a
// budget_relaxation — 0 means "unbounded", the most permissive value, so a CI gate
// keyed on budget_relaxation must not block the safe change (#382).
func TestRiskSummary_addingCeilingIsNotRelaxation(t *testing.T) {
	oldG := graphWithPolicyBudget(0, 0, nil) // no execution ceilings at all
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicyBudget(5, 60, nil)

	p := NewPlanner(&fakeDeploy{list: applied})
	pl, err := p.ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range pl.Risk.Items {
		if it.Category == RiskCategoryBudgetRelaxation {
			t.Fatalf("adding a ceiling flagged as budget_relaxation [%s]: %s", it.Severity, it.Reason)
		}
	}
}

// TestRiskSummary_removingCeilingIsRelaxation proves the inverse: removing a ceiling
// (finite → 0/unbounded) is a real relaxation and must flag as budget_relaxation,
// which the old newCost>oldCost comparison missed because it read 0 as a low ceiling
// (#382).
func TestRiskSummary_removingCeilingIsRelaxation(t *testing.T) {
	oldG := graphWithPolicyBudget(5, 60, nil)
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicyBudget(0, 0, nil) // ceilings removed → unbounded

	p := NewPlanner(&fakeDeploy{list: applied})
	pl, err := p.ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRiskCategory(pl, RiskCategoryBudgetRelaxation) {
		t.Fatalf("removing ceilings should flag budget_relaxation, got %#v", pl.Risk.Items)
	}
	var sawCost, sawWall bool
	for _, it := range pl.Risk.Items {
		if it.Category != RiskCategoryBudgetRelaxation {
			continue
		}
		if it.Severity != RiskSeverityHigh {
			t.Fatalf("relaxation severity %s, want high: %#v", it.Severity, it)
		}
		low := strings.ToLower(it.Reason)
		if strings.Contains(low, "cost ceiling") {
			sawCost = true
		}
		if strings.Contains(low, "wall-clock ceiling") {
			sawWall = true
		}
	}
	if !sawCost || !sawWall {
		t.Fatalf("expected both cost and wall-clock removal flagged, cost=%v wall=%v: %#v", sawCost, sawWall, pl.Risk.Items)
	}
}

// Raising a policy's iteration ceiling is a budget relaxation the review gate must see; unlike
// cost/wall, 0/absent is the default 32 (not unbounded), so 8→64 relaxes, 8→unset relaxes (rises to
// 32), and 64→unset is a tightening (falls to 32) that must not flag (issue #522).
func TestRiskSummary_maxIterationsRelaxation(t *testing.T) {
	graphWithIters := func(n int) *spec.ProjectGraph {
		g := minimalGraph()
		g.Policies["default"] = &spec.PolicyResource{
			APIVersion: spec.APIVersionV0, Kind: spec.KindPolicy, Metadata: spec.Metadata{Name: "default"},
			Spec: spec.PolicySpec{Execution: &spec.PolicyExecution{MaxIterations: n}},
		}
		return g
	}
	iterRelaxationFlagged := func(t *testing.T, oldN, newN int) bool {
		t.Helper()
		applied := appliedFromDesired(t, "dev", graphWithIters(oldN))
		p := NewPlanner(&fakeDeploy{list: applied})
		pl, err := p.ComputePlan(context.Background(), "dev", graphWithIters(newN), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range pl.Risk.Items {
			if it.Category == RiskCategoryBudgetRelaxation && strings.Contains(strings.ToLower(it.Reason), "iteration ceiling") {
				if it.Severity != RiskSeverityHigh {
					t.Fatalf("iteration relaxation severity %s, want high", it.Severity)
				}
				return true
			}
		}
		return false
	}
	if !iterRelaxationFlagged(t, 8, 64) {
		t.Fatal("raising the iteration ceiling 8→64 must flag budget_relaxation")
	}
	if !iterRelaxationFlagged(t, 8, 0) {
		t.Fatal("removing an explicit 8 (rises to the default 32) must flag budget_relaxation")
	}
	if iterRelaxationFlagged(t, 64, 0) {
		t.Fatal("64→unset falls to the default 32 — a tightening — and must NOT flag")
	}
	if iterRelaxationFlagged(t, 64, 8) {
		t.Fatal("lowering the ceiling 64→8 must NOT flag budget_relaxation")
	}
}

func TestRiskSummary_effectPermitWidening(t *testing.T) {
	oldG := graphWithPolicyBudget(3, 0, nil)
	oldG.Policies["default"].Spec.Effects = &spec.PolicyEffects{Permit: []string{"github.read"}}
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicyBudget(3, 0, nil)
	newG.Policies["default"].Spec.Effects = &spec.PolicyEffects{Permit: []string{"github.read", "github.write"}}

	p := NewPlanner(&fakeDeploy{list: applied})
	pl, err := p.ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRiskCategory(pl, RiskCategoryEffectPermitWidening) {
		t.Fatalf("expected effect_permit_widening, got %#v", pl.Risk.Items)
	}
	var n int
	for _, it := range pl.Risk.Items {
		if it.Category != RiskCategoryEffectPermitWidening {
			continue
		}
		n++
		if it.Severity != RiskSeverityHigh {
			t.Fatalf("severity %s", it.Severity)
		}
		if !strings.Contains(it.Reason, "github.write") {
			t.Fatalf("reason should name new ident: %#v", it)
		}
		if strings.Contains(strings.ToLower(it.Reason), "tight") || it.Category == RiskCategoryBudgetRelaxation {
			t.Fatalf("permit widening must not be labeled tightening/budget: %#v", it)
		}
		if len(it.Witness) == 0 || it.Witness[0].Kind != WitnessKindPolicy {
			t.Fatalf("witness: %#v", it.Witness)
		}
	}
	if n != 1 {
		t.Fatalf("want 1 widening item, got %d: %#v", n, pl.Risk.Items)
	}

	covered := graphWithPolicyBudget(3, 0, nil)
	covered.Policies["default"].Spec.Effects = &spec.PolicyEffects{Permit: []string{"github"}}
	applied2 := appliedFromDesired(t, "dev", covered)
	child := graphWithPolicyBudget(3, 0, nil)
	child.Policies["default"].Spec.Effects = &spec.PolicyEffects{Permit: []string{"github", "github.read"}}
	pl2, err := NewPlanner(&fakeDeploy{list: applied2}).ComputePlan(context.Background(), "dev", child, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasRiskCategory(pl2, RiskCategoryEffectPermitWidening) {
		t.Fatalf("github already covers github.read: %#v", pl2.Risk.Items)
	}
}

func TestRiskSummary_effectPermitWidening_unattendedPromotion(t *testing.T) {
	oldG := graphWithPolicyBudget(3, 0, nil)
	oldG.Policies["default"].Spec.Effects = &spec.PolicyEffects{PermitWithApproval: []string{"github.write"}}
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicyBudget(3, 0, nil)
	newG.Policies["default"].Spec.Effects = &spec.PolicyEffects{Permit: []string{"github.write"}}

	p := NewPlanner(&fakeDeploy{list: applied})
	pl, err := p.ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRiskCategory(pl, RiskCategoryEffectPermitWidening) {
		t.Fatalf("promoting permitWithApproval to unattended permit must be effect_permit_widening, got %#v", pl.Risk.Items)
	}
	var n int
	for _, it := range pl.Risk.Items {
		if it.Category != RiskCategoryEffectPermitWidening {
			continue
		}
		n++
		if !strings.Contains(it.Reason, "github.write") {
			t.Fatalf("reason should name promoted ident: %#v", it)
		}
	}
	if n != 1 {
		t.Fatalf("want 1 widening item, got %d: %#v", n, pl.Risk.Items)
	}

	dual := graphWithPolicyBudget(3, 0, nil)
	dual.Policies["default"].Spec.Effects = &spec.PolicyEffects{
		Permit:             []string{"github.write"},
		PermitWithApproval: []string{"github.write"},
	}
	plDual, err := NewPlanner(&fakeDeploy{list: applied}).ComputePlan(context.Background(), "dev", dual, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasRiskCategory(plDual, RiskCategoryEffectPermitWidening) {
		t.Fatalf("dual-list stays approval-gated, not unattended widening: %#v", plDual.Risk.Items)
	}
}

func TestRiskSummary_approvalRemovalAndCostIncrease_distinctItems(t *testing.T) {
	oldG := graphWithPolicyBudget(3, 60, []string{"tool.helper.echo", "tool.github.issues.write"})
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicyBudget(10, 60, []string{"tool.github.issues.write"})

	p := NewPlanner(&fakeDeploy{list: applied})
	pl, err := p.ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRiskCategory(pl, RiskCategoryApprovalRemoval) || !hasRiskCategory(pl, RiskCategoryBudgetRelaxation) {
		t.Fatalf("expected approval_removal and budget_relaxation, got %#v", pl.Risk.Items)
	}
	var approval, budget int
	for _, it := range pl.Risk.Items {
		switch it.Category {
		case RiskCategoryApprovalRemoval:
			approval++
			if it.Severity != RiskSeverityHigh {
				t.Fatalf("approval_removal severity %s", it.Severity)
			}
			if !strings.Contains(it.Reason, "tool.helper.echo") {
				t.Fatalf("approval item should name removed action: %#v", it)
			}
			if got := FormatRiskItem(it); !strings.Contains(got, "approval_removal") || !strings.Contains(got, "[high]") {
				t.Fatalf("unlabeled item: %s", got)
			}
		case RiskCategoryBudgetRelaxation:
			budget++
			if it.Severity != RiskSeverityHigh {
				t.Fatalf("budget_relaxation severity %s", it.Severity)
			}
			if !strings.Contains(strings.ToLower(it.Reason), "cost ceiling increased") {
				t.Fatalf("budget item: %#v", it)
			}
		}
	}
	if approval != 1 || budget != 1 {
		t.Fatalf("want 1 approval + 1 cost budget item, got approval=%d budget=%d items=%#v", approval, budget, pl.Risk.Items)
	}
	labeled := FormatPlan(pl)
	if !strings.Contains(labeled, "high:\n") {
		t.Fatalf("plan output missing high severity group:\n%s", labeled)
	}
	if !strings.Contains(labeled, "[high] approval_removal:") || !strings.Contains(labeled, "[high] budget_relaxation:") {
		t.Fatalf("plan output missing labeled items:\n%s", labeled)
	}
}

func TestRiskSummary_wallClockCeilingIncreased(t *testing.T) {
	oldG := graphWithPolicyBudget(3, 60, nil)
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithPolicyBudget(3, 120, nil)

	pl, err := NewPlanner(&fakeDeploy{list: applied}).ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range pl.Risk.Items {
		if it.Category == RiskCategoryBudgetRelaxation && strings.Contains(strings.ToLower(it.Reason), "wall-clock") {
			found = true
			if it.Severity != RiskSeverityHigh {
				t.Fatalf("wall-clock severity %s", it.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected wall-clock budget_relaxation, got %#v", pl.Risk.Items)
	}
}

func TestRiskSummary_agentModelChanged(t *testing.T) {
	oldG := graphWithAgent("mock/gpt-4")
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithAgent("mock/gpt-4o")

	pl, err := NewPlanner(&fakeDeploy{list: applied}).ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range pl.Risk.Items {
		if it.Category == RiskCategoryModelChange {
			found = true
			if it.Severity != RiskSeverityMedium {
				t.Fatalf("model_change severity %s", it.Severity)
			}
			if it.Target.Kind != RiskTargetAgent || it.Target.Name != "rev" {
				t.Fatalf("target %#v", it.Target)
			}
			if len(it.Witness) != 1 || it.Witness[0].Kind != WitnessKindAgent || it.Witness[0].Reachability != WitnessStatic {
				t.Fatalf("witness %#v", it.Witness)
			}
		}
	}
	if !found {
		t.Fatalf("expected model_change, got %#v", pl.Risk.Items)
	}
}

// TestRiskSummary_agentToolsListGained is the ADR 007 step-1 regression: after removing the
// spec.permissions write-name heuristic, an agent that gains a side-effecting (write-capable) tool
// must STILL produce a HIGH tool_surface_change authority finding — now derived from the declared
// safety.sideEffects signal, proving we deleted a redundant heuristic, not the coverage.
func TestRiskSummary_agentToolsListGained(t *testing.T) {
	oldG := graphWithAgentTools(false, false)
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithAgentTools(true, true) // agent gains a side-effecting github tool

	pl, err := NewPlanner(&fakeDeploy{list: applied}).ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range pl.Risk.Items {
		if it.Category == RiskCategoryToolSurfaceChange {
			found = true
			if it.Severity != RiskSeverityHigh {
				t.Fatalf("write-capable tool surface should be high, got %s", it.Severity)
			}
			if !strings.Contains(it.Reason, "github") {
				t.Fatalf("reason %#v", it.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected tool_surface_change, got %#v", pl.Risk.Items)
	}
}

// TestRiskSummary_agentToolsListGained_pinnedGrant is the regression for the pinned-reference
// fail-open (review of #477): under ADR 007 agent grants are operation-pinned (tool.<name>.<op>), so
// the tool-surface risk path must resolve that reference to its base tool before the safety lookup —
// otherwise g.Tools["tool.github.default"] misses (tools are keyed "github"), sideEffects reads false,
// and a genuinely write-capable tool-surface widening is under-scored HIGH→MEDIUM. The existing
// TestRiskSummary_agentToolsListGained used bare names, which resolve trivially and hid this.
func TestRiskSummary_agentToolsListGained_pinnedGrant(t *testing.T) {
	oldG := graphWithAgentTools(false, false)
	oldG.Agents["rev"].Spec.Tools = []string{"tool.helper.echo"}
	applied := appliedFromDesired(t, "dev", oldG)
	newG := graphWithAgentTools(true, true) // github is side-effecting
	newG.Agents["rev"].Spec.Tools = []string{"tool.helper.echo", "tool.github.default"}

	pl, err := NewPlanner(&fakeDeploy{list: applied}).ComputePlan(context.Background(), "dev", newG, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range pl.Risk.Items {
		if it.Category == RiskCategoryToolSurfaceChange {
			found = true
			if it.Severity != RiskSeverityHigh {
				t.Fatalf("a write-capable tool granted by its pinned reference must be high, got %s", it.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected tool_surface_change, got %#v", pl.Risk.Items)
	}
}

func TestRiskItem_witnessPathReadyForEffectDelta(t *testing.T) {
	item := RiskItem{
		Category: RiskCategorySafety,
		Severity: RiskSeverityHigh,
		Reason:   "effect-delta placeholder",
		Target:   RiskTarget{Kind: RiskTargetTool, Name: "github"},
		Witness: []WitnessHop{
			{Kind: WitnessKindWorkflow, Name: "pr-review", Reachability: WitnessStatic},
			{Kind: WitnessKindStep, Name: "review", ID: "review", Reachability: WitnessStatic},
			{Kind: WitnessKindAgent, Name: "reviewer", Reachability: WitnessAutonomous},
			{Kind: WitnessKindToolOperation, Name: "issues.write", Reachability: WitnessAutonomous},
		},
	}
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var got RiskItem
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Witness) != 4 {
		t.Fatalf("witness hops: %#v", got.Witness)
	}
	if got.Witness[0].Kind != WitnessKindWorkflow || got.Witness[2].Reachability != WitnessAutonomous {
		t.Fatalf("round-trip %#v", got.Witness)
	}
	if got.Witness[3].Kind != WitnessKindToolOperation || got.Witness[3].Name != "issues.write" {
		t.Fatalf("tool_operation hop %#v", got.Witness[3])
	}
}

// graphWithAgentTools builds a reviewer agent granted a read-only `helper` and, when includeGithub is
// set, a `github` tool whose declared side effects are githubSideEffects. Write-capability risk is
// derived from safety.sideEffects (ADR 007 step 1), not a permission allow list.
func graphWithAgentTools(includeGithub, githubSideEffects bool) *spec.ProjectGraph {
	g := graphWithAgent("mock/gpt-4")
	no := false
	g.Tools["helper"] = &spec.ToolResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindTool,
		Metadata:   spec.Metadata{Name: "helper"},
		Spec:       spec.ToolSpec{Type: "native", Safety: &spec.ToolSafety{SideEffects: &no}},
	}
	se := githubSideEffects
	g.Tools["github"] = &spec.ToolResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindTool,
		Metadata:   spec.Metadata{Name: "github"},
		Spec:       spec.ToolSpec{Type: "native", Safety: &spec.ToolSafety{SideEffects: &se}},
	}
	if includeGithub {
		g.Agents["rev"].Spec.Tools = []string{"helper", "github"}
	} else {
		g.Agents["rev"].Spec.Tools = []string{"helper"}
	}
	return g
}

func hasRiskCategory(pl *Plan, cat RiskCategory) bool {
	if pl == nil {
		return false
	}
	for _, it := range pl.Risk.Items {
		if it.Category == cat {
			return true
		}
	}
	return false
}
