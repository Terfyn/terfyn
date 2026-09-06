package lower

import (
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// Inline tool/policy lowering (ADR 005, issue #333): a `.agent` tool/policy declaration lowers to
// the same spec.ToolResource / spec.PolicyResource the YAML loader produces, so there is one IR and
// one validator. Presence-sensitive semantics are preserved — most importantly ToolSpec
// .OperationsDeclared, which the closed-world capability manifest (#204) derives from the presence
// of an `operations` block (including an empty one).

// stringLitValue returns a string literal's value, or "" when absent.
func stringLitValue(s *lang.StringLit) string {
	if s == nil {
		return ""
	}
	return s.Value
}

// stringLitList returns the values of a string-literal list, or nil when empty (matching YAML omitempty).
func stringLitList(items []*lang.StringLit) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, stringLitValue(it))
	}
	return out
}

// headerPairsMap collapses header key/value pairs into a map, or nil when empty (matching YAML
// omitempty). A duplicate key keeps the last value, mirroring YAML map semantics.
func headerPairsMap(pairs []*lang.HeaderPair) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if p == nil || p.Key == nil {
			continue
		}
		out[p.Key.Value] = stringLitValue(p.Value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (l *lowerer) tool(d *lang.ToolDecl) *spec.ToolResource {
	name := identName(d.Name)
	if name == "" {
		l.diag(d.Pos, "tool declaration has no name")
		return nil
	}
	tr := &spec.ToolResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindTool,
		Metadata:   spec.Metadata{Name: name},
		Pos:        d.Pos,
	}
	if d.Type != nil {
		tr.Spec.Type = d.Type.Name
	}
	if m := d.MCP; m != nil {
		tr.Spec.MCP = &spec.ToolMCP{
			Transport: stringLitValue(m.Transport),
			Command:   stringLitValue(m.Command),
			URL:       stringLitValue(m.URL),
			Args:      stringLitList(m.Args),
			Headers:   headerPairsMap(m.Headers),
		}
	}
	if h := d.HTTP; h != nil {
		tr.Spec.HTTP = &spec.ToolHTTP{
			BaseURL: stringLitValue(h.BaseURL),
			Headers: headerPairsMap(h.Headers),
		}
	}
	if w := d.Workspace; w != nil {
		tr.Spec.Workspace = &spec.ToolWorkspace{
			Root:        stringLitValue(w.Root),
			TestCommand: stringLitValue(w.TestCommand),
		}
	}
	if r := d.Retry; r != nil {
		tr.Spec.Retry = &spec.ToolRetry{Backoff: stringLitValue(r.Backoff)}
		if r.MaxAttempts != nil {
			tr.Spec.Retry.MaxAttempts = *r.MaxAttempts
		}
	}
	if lim := d.Limits; lim != nil {
		tr.Spec.Limits = lowerToolLimits(lim)
	}
	if d.Safety != nil {
		tr.Spec.Safety = &spec.ToolSafety{
			Trusted:          d.Safety.Trusted,
			SideEffects:      d.Safety.SideEffects,
			RequiresApproval: d.Safety.RequiresApproval,
		}
	}
	if d.Operations != nil {
		// Presence of the block — even `operations {}` — is a closed callable world. This mirrors the
		// YAML `operations:` key stamping and is the security boundary the two front ends must agree on.
		tr.Spec.OperationsDeclared = true
		tr.Spec.Operations = map[string]spec.ToolOperation{}
		for _, op := range d.Operations.Ops {
			opName := identName(op.Name)
			if opName == "" {
				continue
			}
			var effs []string
			for _, e := range op.Effects {
				if e != nil {
					effs = append(effs, e.Name)
				}
			}
			tr.Spec.Operations[opName] = spec.ToolOperation{Effects: effs, Schema: stringLitValue(op.Schema)}
		}
	}
	return tr
}

// lowerToolLimits lowers a `limits { … }` block to spec.ExecutionLimits (issue #440), copying only set
// fields so an unset override stays zero (and does not win the ResolveExecutionLimits merge).
func lowerToolLimits(b *lang.ToolLimitsBlock) *spec.ExecutionLimits {
	out := &spec.ExecutionLimits{}
	setInt := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	setInt(&out.MaxToolInputBytes, b.MaxToolInputBytes)
	setInt(&out.MaxToolOutputBytes, b.MaxToolOutputBytes)
	setInt(&out.MaxCheckpointBytes, b.MaxCheckpointBytes)
	setInt(&out.MaxStateBytes, b.MaxStateBytes)
	setInt(&out.MaxWorkflowNesting, b.MaxWorkflowNesting)
	setInt(&out.MaxLoopIterations, b.MaxLoopIterations)
	if b.ToolInputExceedPolicy != nil {
		out.ToolInputExceedPolicy = spec.LimitExceedPolicy(b.ToolInputExceedPolicy.Name)
	}
	if b.ToolOutputExceedPolicy != nil {
		out.ToolOutputExceedPolicy = spec.LimitExceedPolicy(b.ToolOutputExceedPolicy.Name)
	}
	if b.CheckpointExceedPolicy != nil {
		out.CheckpointExceedPolicy = spec.LimitExceedPolicy(b.CheckpointExceedPolicy.Name)
	}
	return out
}

func (l *lowerer) policy(d *lang.PolicyDecl) *spec.PolicyResource {
	name := identName(d.Name)
	if name == "" {
		l.diag(d.Pos, "policy declaration has no name")
		return nil
	}
	pr := &spec.PolicyResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindPolicy,
		Metadata:   spec.Metadata{Name: name},
		Pos:        d.Pos,
	}
	if preset := identName(d.Preset); preset != "" {
		pr.Spec.Preset = preset
	}
	pr.Spec.Execution = lowerPolicyExecution(d.Execution)
	pr.Spec.Approvals = lowerPolicyApprovals(d.Approvals)
	pr.Spec.Hitl = lowerHitl(d.Hitl)
	if t := d.Tools; t != nil && t.ForbidUnknownTools != nil {
		pr.Spec.Tools = &spec.PolicyTools{ForbidUnknownTools: *t.ForbidUnknownTools}
	}
	if e := d.Effects; e != nil {
		ef := &spec.PolicyEffects{}
		for _, r := range e.Permit {
			if r != nil {
				ef.Permit = append(ef.Permit, r.Name)
			}
		}
		for _, r := range e.PermitWithApproval {
			if r != nil {
				ef.PermitWithApproval = append(ef.PermitWithApproval, r.Name)
			}
		}
		pr.Spec.Effects = ef
	}
	return pr
}

// lowerPolicyExecution lowers a policy execution block, or nil when absent.
func lowerPolicyExecution(e *lang.PolicyExecutionBlock) *spec.PolicyExecution {
	if e == nil {
		return nil
	}
	ex := &spec.PolicyExecution{}
	if e.MaxTotalCostUsd != nil {
		ex.MaxTotalCostUsd = *e.MaxTotalCostUsd
	}
	if e.MaxWallClockSeconds != nil {
		ex.MaxWallClockSeconds = *e.MaxWallClockSeconds
	}
	if e.MaxIterations != nil {
		ex.MaxIterations = *e.MaxIterations
	}
	if e.RequireStructuredOutput != nil {
		ex.RequireStructuredOutput = *e.RequireStructuredOutput
	}
	return ex
}

// lowerPolicyApprovals lowers a policy approvals block, or nil when absent.
func lowerPolicyApprovals(a *lang.PolicyApprovalsBlock) *spec.PolicyApprovals {
	if a == nil {
		return nil
	}
	ap := &spec.PolicyApprovals{RequireAllTools: a.RequireAllTools, Permissive: a.Permissive}
	for _, g := range a.RequiredFor {
		if uses := grantUses(g); uses != "" {
			ap.RequiredFor = append(ap.RequiredFor, uses)
		}
	}
	return ap
}

// switchMapToSpec collapses switch-map entries into map[source][]targets, or nil when empty
// (matching YAML omitempty). A duplicate source keeps the last, mirroring YAML map semantics.
func switchMapToSpec(entries []*lang.SwitchMapEntry) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for _, e := range entries {
		if e == nil || e.Source == nil {
			continue
		}
		var targets []string
		for _, t := range e.Targets {
			if t != nil {
				targets = append(targets, t.Name)
			}
		}
		out[e.Source.Name] = targets
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lowerHitl lowers a `hitl { … }` policy block to spec.HitlPolicy, or nil when absent (issues #106, #440).
func lowerHitl(h *lang.HitlBlock) *spec.HitlPolicy {
	if h == nil {
		return nil
	}
	out := &spec.HitlPolicy{
		DescriptionPrefix: stringLitValue(h.DescriptionPrefix),
		RedactKeys:        stringLitList(h.RedactKeys),
		ToolSwitchMap:     switchMapToSpec(h.ToolSwitchMap),
	}
	if len(h.InterruptOn) > 0 {
		out.InterruptOn = make(map[string]spec.HitlInterruptValue, len(h.InterruptOn))
		for _, e := range h.InterruptOn {
			if e == nil || e.Name == nil {
				continue
			}
			out.InterruptOn[e.Name.Name] = spec.HitlInterruptValue{
				Enabled: true,
				Config:  lowerInterruptConfig(e.Config),
			}
		}
	}
	return out
}

// lowerInterruptConfig lowers a per-tool interruptOn config block, or nil when the entry is a bare
// tool name (enabled with defaults).
func lowerInterruptConfig(c *lang.InterruptConfig) *spec.HitlInterruptConfig {
	if c == nil {
		return nil
	}
	cfg := &spec.HitlInterruptConfig{
		Description:      stringLitValue(c.Description),
		AllowedEditArgs:  stringLitList(c.AllowedEditArgs),
		DeniedEditArgs:   stringLitList(c.DeniedEditArgs),
		AllowedEditPaths: stringLitList(c.AllowedEditPaths),
		DeniedEditPaths:  stringLitList(c.DeniedEditPaths),
		AllowedEditTools: stringLitList(c.AllowedEditTools),
		SwitchMap:        switchMapToSpec(c.SwitchMap),
		RedactKeys:       stringLitList(c.RedactKeys),
	}
	for _, d := range c.AllowedDecisions {
		if d != nil {
			cfg.AllowedDecisions = append(cfg.AllowedDecisions, spec.HitlDecisionKind(d.Name))
		}
	}
	return cfg
}

// provider lowers a `provider <alias> { type … apiKeyFrom … workspaceIdFrom … }` declaration to a
// spec.ModelProviderConfig destined for spec.ProjectSpec.Providers.Models[alias] (issue #440). The
// second return is false (with a diagnostic) when the alias or the required type is missing.
func (l *lowerer) provider(d *lang.ProviderDecl) (LoweredProvider, bool) {
	name := identName(d.Name)
	if name == "" {
		l.diag(d.Pos, "provider declaration has no name")
		return LoweredProvider{}, false
	}
	typ := identName(d.Type)
	if typ == "" {
		l.diag(d.Pos, "provider %q must declare a type (e.g. type anthropic)", name)
		return LoweredProvider{}, false
	}
	return LoweredProvider{
		Name: name,
		Pos:  d.Pos,
		Config: spec.ModelProviderConfig{
			Type:            typ,
			APIKeyFrom:      stringLitValue(d.APIKeyFrom),
			WorkspaceIDFrom: stringLitValue(d.WorkspaceIDFrom),
		},
	}, true
}

// defaults lowers the singleton `defaults { policy … model … runtime … }` block to a
// spec.ProjectDefaults destined for spec.ProjectSpec.Defaults (issue #440, ADR 007). Returns nil
// only for a nil decl; an all-empty block lowers to a zero-value ProjectDefaults (harmless, and the
// printer/raiser round-trips it to nothing).
func (l *lowerer) defaults(d *lang.DefaultsDecl) *spec.ProjectDefaults {
	if d == nil {
		return nil
	}
	out := &spec.ProjectDefaults{
		Policy:  identName(d.Policy),
		Runtime: identName(d.Runtime),
	}
	if d.Model != nil {
		out.Model = d.Model.Raw
	}
	return out
}

// projectLimits lowers the singleton top-level `limits { … }` block to spec.ProjectSpec.Limits (issue
// #440, ADR 007), reusing lowerToolLimits for the shared nine-field body. Returns nil only for a nil
// decl or nil body.
func (l *lowerer) projectLimits(d *lang.LimitsDecl) *spec.ExecutionLimits {
	if d == nil || d.Block == nil {
		return nil
	}
	return lowerToolLimits(d.Block)
}

// environment lowers an `environment <Name> { overrides { … } }` declaration to the same
// spec.EnvironmentResource the YAML loader produces (issue #440), applied by spec.ApplyEnvironment.
func (l *lowerer) environment(d *lang.EnvironmentDecl) *spec.EnvironmentResource {
	name := identName(d.Name)
	if name == "" {
		l.diag(d.Pos, "environment declaration has no name")
		return nil
	}
	er := &spec.EnvironmentResource{
		APIVersion: spec.APIVersionV0,
		Kind:       spec.KindEnvironment,
		Metadata:   spec.Metadata{Name: name},
		Pos:        d.Pos,
	}
	if ov := d.Overrides; ov != nil {
		out := &spec.EnvironmentOverrides{}
		if len(ov.Agents) > 0 {
			out.Agents = map[string]spec.AgentOverride{}
			for _, a := range ov.Agents {
				an := identName(a.Name)
				if an == "" {
					continue
				}
				o := spec.AgentOverride{}
				if a.Model != nil {
					o.Model = a.Model.Raw
				}
				if a.Constraints != nil {
					o.Constraints = lowerConstraints(a.Constraints)
				}
				out.Agents[an] = o
			}
		}
		if len(ov.Policies) > 0 {
			out.Policies = map[string]spec.PolicyOverride{}
			for _, pol := range ov.Policies {
				pn := identName(pol.Name)
				if pn == "" {
					continue
				}
				out.Policies[pn] = spec.PolicyOverride{
					Execution: lowerPolicyExecution(pol.Execution),
					Approvals: lowerPolicyApprovals(pol.Approvals),
				}
			}
		}
		er.Spec.Overrides = out
	}
	return er
}
