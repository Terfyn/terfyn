package policy

import (
	"context"
	"strings"

	"github.com/Terfyn/terfyn/internal/spec"
)

// StricterOf returns an evaluator that fails closed if either a or b denies
// (issue #194). Subworkflow steps apply both the caller's and the callee's policy.
func StricterOf(a, b PolicyEvaluator) PolicyEvaluator {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &stricterEvaluator{a: a, b: b}
}

type stricterEvaluator struct {
	a, b PolicyEvaluator
}

func (s *stricterEvaluator) CheckRun(ctx context.Context, run RunContext) error {
	if err := s.a.CheckRun(ctx, run); err != nil {
		return err
	}
	return s.b.CheckRun(ctx, run)
}

func (s *stricterEvaluator) CheckStep(ctx context.Context, step StepContext) error {
	if err := s.a.CheckStep(ctx, step); err != nil {
		return err
	}
	return s.b.CheckStep(ctx, step)
}

func (s *stricterEvaluator) CheckToolCall(ctx context.Context, call ToolCallContext) error {
	if err := s.a.CheckToolCall(ctx, call); err != nil {
		return err
	}
	return s.b.CheckToolCall(ctx, call)
}

func (s *stricterEvaluator) PolicySpec() *spec.PolicySpec {
	type specCarrier interface {
		PolicySpec() *spec.PolicySpec
	}
	var left, right *spec.PolicySpec
	if c, ok := s.a.(specCarrier); ok {
		left = c.PolicySpec()
	}
	if c, ok := s.b.(specCarrier); ok {
		right = c.PolicySpec()
	}
	return mergeStricterPolicySpec(left, right)
}

func mergeStricterPolicySpec(a, b *spec.PolicySpec) *spec.PolicySpec {
	if a == nil && b == nil {
		return nil
	}
	var out *spec.PolicySpec
	switch {
	case a == nil:
		out = clonePolicySpec(b)
	case b == nil:
		out = clonePolicySpec(a)
	default:
		out = clonePolicySpec(a)
		if b.Execution != nil {
			if out.Execution == nil {
				cp := *b.Execution
				out.Execution = &cp
			} else {
				ex := *out.Execution
				if b.Execution.MaxTotalCostUsd > 0 && (ex.MaxTotalCostUsd <= 0 || b.Execution.MaxTotalCostUsd < ex.MaxTotalCostUsd) {
					ex.MaxTotalCostUsd = b.Execution.MaxTotalCostUsd
				}
				if b.Execution.MaxWallClockSeconds > 0 && (ex.MaxWallClockSeconds <= 0 || b.Execution.MaxWallClockSeconds < ex.MaxWallClockSeconds) {
					ex.MaxWallClockSeconds = b.Execution.MaxWallClockSeconds
				}
				if b.Execution.RequireStructuredOutput {
					ex.RequireStructuredOutput = true
				}
				out.Execution = &ex
			}
		}
		out.Hitl = mergeStricterHitl(out.Hitl, b.Hitl)
	}
	// The iteration ceiling is the tighter of BOTH policies' EFFECTIVE ceilings. Unlike cost/wall-clock,
	// 0 means "the default 32", not "unbounded" — so an absent policy (or execution block) still imposes
	// 32, and a subworkflow can never fail open on a callee's lower cap by omitting the field (issue
	// #522). Only a genuine tightening below the default is materialized; at the default it stays unset.
	if merged := min(policyIterationCeiling(a), policyIterationCeiling(b)); merged < spec.HardAgentMaxIterations {
		if out.Execution == nil {
			out.Execution = &spec.PolicyExecution{}
		}
		out.Execution.MaxIterations = merged
	} else if out.Execution != nil {
		// Both sides allow ≥ 32: keep an explicit higher shared ceiling, else clear to unset (= 32).
		if merged == spec.HardAgentMaxIterations {
			out.Execution.MaxIterations = 0
		} else {
			out.Execution.MaxIterations = merged
		}
	}
	return out
}

// policyIterationCeiling is a policy's effective iteration ceiling — the declared execution.maxIterations
// or the default HardAgentMaxIterations when the policy, its execution block, or the field is absent.
func policyIterationCeiling(p *spec.PolicySpec) int {
	if p == nil || p.Execution == nil {
		return spec.HardAgentMaxIterations
	}
	return spec.EffectiveMaxIterationsCeiling(p.Execution.MaxIterations)
}

func clonePolicySpec(p *spec.PolicySpec) *spec.PolicySpec {
	if p == nil {
		return nil
	}
	out := *p
	if p.Execution != nil {
		cp := *p.Execution
		out.Execution = &cp
	}
	out.Hitl = cloneHitlPolicy(p.Hitl)
	if p.Approvals != nil {
		ap := *p.Approvals
		ap.RequiredFor = append([]string(nil), p.Approvals.RequiredFor...)
		out.Approvals = &ap
	}
	if p.Tools != nil {
		tp := *p.Tools
		out.Tools = &tp
	}
	if p.Effects != nil {
		ef := *p.Effects
		ef.Permit = append([]string(nil), p.Effects.Permit...)
		ef.PermitWithApproval = append([]string(nil), p.Effects.PermitWithApproval...)
		out.Effects = &ef
	}
	return &out
}

func cloneHitlPolicy(h *spec.HitlPolicy) *spec.HitlPolicy {
	if h == nil {
		return nil
	}
	out := spec.HitlPolicy{
		DescriptionPrefix: h.DescriptionPrefix,
		RedactKeys:        append([]string(nil), h.RedactKeys...),
	}
	if h.InterruptOn != nil {
		out.InterruptOn = make(map[string]spec.HitlInterruptValue, len(h.InterruptOn))
		for k, v := range h.InterruptOn {
			out.InterruptOn[k] = cloneHitlInterruptValue(v)
		}
	}
	if h.ToolSwitchMap != nil {
		out.ToolSwitchMap = cloneStringListMap(h.ToolSwitchMap)
	}
	return &out
}

func cloneHitlInterruptValue(v spec.HitlInterruptValue) spec.HitlInterruptValue {
	out := spec.HitlInterruptValue{Enabled: v.Enabled}
	if v.Config != nil {
		out.Config = cloneHitlInterruptConfig(v.Config)
	}
	return out
}

func cloneHitlInterruptConfig(c *spec.HitlInterruptConfig) *spec.HitlInterruptConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.AllowedDecisions = append([]spec.HitlDecisionKind(nil), c.AllowedDecisions...)
	out.AllowedEditArgs = append([]string(nil), c.AllowedEditArgs...)
	out.DeniedEditArgs = append([]string(nil), c.DeniedEditArgs...)
	out.AllowedEditPaths = append([]string(nil), c.AllowedEditPaths...)
	out.DeniedEditPaths = append([]string(nil), c.DeniedEditPaths...)
	out.AllowedEditTools = append([]string(nil), c.AllowedEditTools...)
	out.RedactKeys = append([]string(nil), c.RedactKeys...)
	out.SwitchMap = cloneStringListMap(c.SwitchMap)
	return &out
}

func mergeStricterHitl(a, b *spec.HitlPolicy) *spec.HitlPolicy {
	if a == nil {
		return cloneHitlPolicy(b)
	}
	if b == nil {
		return cloneHitlPolicy(a)
	}
	out := cloneHitlPolicy(a)
	out.DescriptionPrefix = joinNonEmptyPrefix(out.DescriptionPrefix, b.DescriptionPrefix)
	out.RedactKeys = unionStrings(out.RedactKeys, b.RedactKeys)
	out.ToolSwitchMap = joinAllowMap(out.ToolSwitchMap, b.ToolSwitchMap)
	if out.InterruptOn == nil && b.InterruptOn != nil {
		out.InterruptOn = map[string]spec.HitlInterruptValue{}
	}
	for k, bv := range b.InterruptOn {
		if av, ok := out.InterruptOn[k]; ok {
			out.InterruptOn[k] = mergeHitlInterruptValue(av, bv)
		} else {
			out.InterruptOn[k] = cloneHitlInterruptValue(bv)
		}
	}
	return out
}

func mergeHitlInterruptValue(a, b spec.HitlInterruptValue) spec.HitlInterruptValue {
	out := spec.HitlInterruptValue{Enabled: a.Enabled || b.Enabled}
	ca, cb := a.Config, b.Config
	if ca == nil && cb == nil {
		return out
	}
	// Union the non-decision fields (an absent side contributes nothing there), then set
	// AllowedDecisions to the intersection of the two sides' *effective* decision sets. Each side's
	// effective set is its explicit allowedDecisions, else its default decision set — never the
	// universe (issue #357): treating an unset side as the universe let the other side's explicit
	// decisions survive the merge even when the unset side's real defaults excluded them (e.g. a
	// side that never permitted `edit` merging with an `[edit]`-only side).
	merged := mergeHitlConfigFields(ca, cb)
	merged.AllowedDecisions = intersectDecisions(effectiveDecisions(ca), effectiveDecisions(cb))
	out.Config = merged
	return out
}

// mergeHitlConfigFields unions the non-decision fields of two HITL configs (either may be nil). The
// caller sets AllowedDecisions.
func mergeHitlConfigFields(ca, cb *spec.HitlInterruptConfig) *spec.HitlInterruptConfig {
	if ca == nil {
		return cloneHitlInterruptConfig(cb)
	}
	if cb == nil {
		return cloneHitlInterruptConfig(ca)
	}
	return &spec.HitlInterruptConfig{
		Description:      joinNonEmptyPrefix(ca.Description, cb.Description),
		AllowedEditArgs:  joinAllowList(ca.AllowedEditArgs, cb.AllowedEditArgs),
		DeniedEditArgs:   unionStrings(ca.DeniedEditArgs, cb.DeniedEditArgs),
		AllowedEditPaths: joinAllowList(ca.AllowedEditPaths, cb.AllowedEditPaths),
		DeniedEditPaths:  unionStrings(ca.DeniedEditPaths, cb.DeniedEditPaths),
		AllowedEditTools: joinAllowList(ca.AllowedEditTools, cb.AllowedEditTools),
		SwitchMap:        joinAllowMap(ca.SwitchMap, cb.SwitchMap),
		RedactKeys:       unionStrings(ca.RedactKeys, cb.RedactKeys),
	}
}

// effectiveDecisions is the decision set a HITL config actually permits: its explicit
// allowedDecisions if set, else the operation-independent default set — {approve, reject}, plus
// `edit` when the config declares edit args/paths and `switch` when it declares switch targets
// locally. A `switch` permitted only via a policy-level toolSwitchMap for a specific operation is
// not known at merge time; omitting it keeps the stricter merge fail-closed (the merge may be
// stricter than the true intersection for `switch`, never more permissive), preserving
// monotonicity (merged ⊆ each side).
func effectiveDecisions(cfg *spec.HitlInterruptConfig) []spec.HitlDecisionKind {
	if cfg != nil && len(cfg.AllowedDecisions) > 0 {
		return append([]spec.HitlDecisionKind(nil), cfg.AllowedDecisions...)
	}
	out := []spec.HitlDecisionKind{spec.HitlDecisionApprove, spec.HitlDecisionReject}
	if cfg != nil {
		if len(cfg.AllowedEditArgs) > 0 || len(cfg.AllowedEditPaths) > 0 || len(cfg.DeniedEditArgs) > 0 || len(cfg.DeniedEditPaths) > 0 {
			out = append(out, spec.HitlDecisionEdit)
		}
		if len(cfg.AllowedEditTools) > 0 || len(cfg.SwitchMap) > 0 {
			out = append(out, spec.HitlDecisionSwitch)
		}
	}
	return out
}

// intersectDecisions returns the decisions present in both a and b, in a's order and de-duplicated.
// An empty intersection is the deny-all sentinel (see hitlDecisionNone) — never an empty slice,
// which downstream would re-read as "unset ⇒ defaults" and fail open.
func intersectDecisions(a, b []spec.HitlDecisionKind) []spec.HitlDecisionKind {
	seen := make(map[spec.HitlDecisionKind]struct{}, len(b))
	for _, k := range b {
		seen[k] = struct{}{}
	}
	var out []spec.HitlDecisionKind
	used := map[spec.HitlDecisionKind]struct{}{}
	for _, k := range a {
		if _, ok := seen[k]; !ok {
			continue
		}
		if _, dup := used[k]; dup {
			continue
		}
		used[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		return []spec.HitlDecisionKind{hitlDecisionNone}
	}
	return out
}

func joinNonEmptyPrefix(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "" || a == b:
		return a
	default:
		return a + " / " + b
	}
}

func joinAllowList(a, b []string) []string {
	if len(a) == 0 {
		return append([]string(nil), b...)
	}
	if len(b) == 0 {
		return append([]string(nil), a...)
	}
	return intersectStrings(a, b)
}

func joinAllowMap(a, b map[string][]string) map[string][]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := map[string][]string{}
	for k, v := range a {
		out[k] = append([]string(nil), v...)
	}
	for k, v := range b {
		if prev, ok := out[k]; ok {
			out[k] = joinAllowList(prev, v)
		} else {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	return uniqueStrings(append(append([]string(nil), a...), b...))
}

func intersectStrings(a, b []string) []string {
	seen := map[string]struct{}{}
	for _, s := range b {
		s = strings.TrimSpace(s)
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	var out []string
	used := map[string]struct{}{}
	for _, s := range a {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; !ok {
			continue
		}
		if _, dup := used[s]; dup {
			continue
		}
		used[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func cloneStringListMap(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = append([]string(nil), v...)
	}
	return out
}
