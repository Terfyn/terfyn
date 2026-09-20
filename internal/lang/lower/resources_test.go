package lower

import (
	"encoding/json"
	"testing"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

func lowerToolsOrFatal(t *testing.T, src string) *Result {
	t.Helper()
	f, diags := lang.Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse errors: %v", diags)
	}
	res, ld := LowerFile(f, Options{})
	if ld.HasErrors() {
		t.Fatalf("lower diagnostics: %v", ld)
	}
	return res
}

// TestInlineTool_OperationsDeclaredPresence is the soundness invariant (ADR 005 §2): an explicit
// `operations {}` is a closed, deny-all manifest (OperationsDeclared=true); an omitted block is open.
func TestInlineTool_OperationsDeclaredPresence(t *testing.T) {
	closed := lowerToolsOrFatal(t, "tool a {\n type native\n operations {}\n}")
	if len(closed.Tools) != 1 || !closed.Tools[0].Spec.OperationsDeclared {
		t.Fatalf("empty operations {} must set OperationsDeclared=true: %+v", closed.Tools)
	}
	if len(closed.Tools[0].Spec.Operations) != 0 {
		t.Fatalf("empty operations {} must have no operations, got %v", closed.Tools[0].Spec.Operations)
	}

	open := lowerToolsOrFatal(t, "tool b {\n type native\n}")
	if len(open.Tools) != 1 || open.Tools[0].Spec.OperationsDeclared {
		t.Fatalf("omitted operations block must leave OperationsDeclared=false: %+v", open.Tools)
	}
}

// TestInlineTool_YAMLEquivalence is the required equivalence golden (ADR 005 §2): an inline tool and
// its YAML twin normalize to byte-identical spec JSON, including the OperationsDeclared bit — so the
// two front ends can never diverge on the closed-world security boundary.
func TestInlineTool_YAMLEquivalence(t *testing.T) {
	agentSrc := `tool workspace {
    type native
    safety { trusted true }
    operations {
        read_file  { effects { workspace.read } }
        write_file { effects { workspace.write } }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Tool
metadata: {name: workspace}
spec:
  type: native
  safety: {trusted: true}
  operations:
    read_file: {effects: [workspace.read]}
    write_file: {effects: [workspace.write]}
`
	inline := lowerToolsOrFatal(t, agentSrc).Tools[0]

	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "workspace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ToolResource)

	if inline.Spec.OperationsDeclared != fromYAML.Spec.OperationsDeclared || !inline.Spec.OperationsDeclared {
		t.Fatalf("OperationsDeclared mismatch: inline=%v yaml=%v", inline.Spec.OperationsDeclared, fromYAML.Spec.OperationsDeclared)
	}
	if normSpecJSON(t, inline) != normSpecJSON(t, fromYAML) {
		t.Fatalf("inline tool spec differs from YAML twin:\n inline: %s\n yaml:   %s", normSpecJSON(t, inline), normSpecJSON(t, fromYAML))
	}
}

// normSpecJSON normalizes a single-tool graph and returns the tool's marshaled spec.
func normSpecJSON(t *testing.T, tr *spec.ToolResource) string {
	t.Helper()
	g := &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{tr.Metadata.Name: tr}}
	spec.NormalizeProjectGraph(g)
	b, err := json.Marshal(g.Tools[tr.Metadata.Name].Spec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInlinePolicy_YAMLEquivalence mirrors the tool golden on the security-relevant policy
// projection (permit / permitWithApproval / requiredFor / execution): an inline policy and its YAML
// twin normalize to byte-identical spec JSON, so a future change to policy lowering or normalization
// cannot silently diverge on the approval/effect surface.
func TestInlinePolicy_YAMLEquivalence(t *testing.T) {
	agentSrc := `policy coding {
    execution { maxTotalCostUsd 5  maxWallClockSeconds 300 }
    approvals { requiredFor { tool.workspace.run_tests } }
    effects {
        permit             { workspace.read }
        permitWithApproval { workspace.write }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Policy
metadata: {name: coding}
spec:
  execution: {maxTotalCostUsd: 5, maxWallClockSeconds: 300}
  approvals: {requiredFor: [tool.workspace.run_tests]}
  effects:
    permit: [workspace.read]
    permitWithApproval: [workspace.write]
`
	inline := lowerToolsOrFatal(t, agentSrc).Policies[0]

	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "coding.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.PolicyResource)

	if normPolicySpecJSON(t, inline) != normPolicySpecJSON(t, fromYAML) {
		t.Fatalf("inline policy spec differs from YAML twin:\n inline: %s\n yaml:   %s", normPolicySpecJSON(t, inline), normPolicySpecJSON(t, fromYAML))
	}
}

// TestInlinePolicy_HitlYAMLEquivalence is the ADR 005 §2 golden for the .agent hitl block (issues
// #106, #440): an inline policy with a rich hitl config and its YAML twin normalize to byte-identical
// spec JSON, so the two front ends can never diverge on human-in-the-loop review config.
func TestInlinePolicy_HitlYAMLEquivalence(t *testing.T) {
	agentSrc := `policy gated-publish {
    approvals { requiredFor { tool.publish.default } }
    hitl {
        descriptionPrefix "Publishing requires operator approval"
        redactKeys { "token" "password" }
        toolSwitchMap {
            deploy_to_production { missing_operation staging }
        }
        interruptOn {
            deploy
            publish {
                allowedDecisions { approve reject edit }
                description "Review publish (${uses})"
                allowedEditArgs { "topic" }
                deniedEditArgs { "secret" }
                switchMap {
                    a.b { c.d }
                }
                redactKeys { "apiKey" }
            }
        }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Policy
metadata: {name: gated-publish}
spec:
  approvals: {requiredFor: [tool.publish.default]}
  hitl:
    descriptionPrefix: Publishing requires operator approval
    redactKeys: [token, password]
    toolSwitchMap:
      deploy_to_production: [missing_operation, staging]
    interruptOn:
      deploy: true
      publish:
        allowedDecisions: [approve, reject, edit]
        description: "Review publish (${uses})"
        allowedEditArgs: [topic]
        deniedEditArgs: [secret]
        switchMap:
          a.b: [c.d]
        redactKeys: [apiKey]
`
	inline := lowerToolsOrFatal(t, agentSrc).Policies[0]

	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "gated-publish.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.PolicyResource)

	if normPolicySpecJSON(t, inline) != normPolicySpecJSON(t, fromYAML) {
		t.Fatalf("inline hitl policy spec differs from YAML twin:\n inline: %s\n yaml:   %s", normPolicySpecJSON(t, inline), normPolicySpecJSON(t, fromYAML))
	}
}

func normPolicySpecJSON(t *testing.T, pr *spec.PolicyResource) string {
	t.Helper()
	g := &spec.ProjectGraph{Policies: map[string]*spec.PolicyResource{pr.Metadata.Name: pr}}
	spec.NormalizeProjectGraph(g)
	b, err := json.Marshal(g.Policies[pr.Metadata.Name].Spec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInlinePolicy_Preset covers the .agent `preset` field (issue #430): a policy may select a
// built-in preset, which lowers to spec.PolicySpec.Preset and resolves through normalization exactly
// like the YAML spec.preset — so a .agent-only project can declare a shell_safe default policy.
func TestInlinePolicy_Preset(t *testing.T) {
	res := lowerToolsOrFatal(t, `policy default {
    preset shell_safe
}`)
	if len(res.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(res.Policies))
	}
	if got := res.Policies[0].Spec.Preset; got != spec.PresetShellSafe {
		t.Fatalf("lowered preset = %q, want %q", got, spec.PresetShellSafe)
	}

	// End to end: after normalization the preset resolves to the built-in shell_safe policy.
	g := &spec.ProjectGraph{
		Meta:     spec.Metadata{Name: "p"},
		Policies: map[string]*spec.PolicyResource{"default": res.Policies[0]},
	}
	spec.NormalizeProjectGraph(g)
	if rp := g.Policies["default"].Spec.ResolvedPreset; rp != spec.PresetShellSafe {
		t.Fatalf("resolved preset = %q, want %q", rp, spec.PresetShellSafe)
	}
}

func TestInlinePolicy_Lowering(t *testing.T) {
	res := lowerToolsOrFatal(t, `policy p {
    execution { maxTotalCostUsd 5 maxWallClockSeconds 300 }
    approvals { requiredFor { tool.git.push_branch } }
    effects { permit { workspace.read } permitWithApproval { workspace.write } }
}`)
	if len(res.Policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(res.Policies))
	}
	p := res.Policies[0].Spec
	if p.Execution == nil || p.Execution.MaxTotalCostUsd != 5 || p.Execution.MaxWallClockSeconds != 300 {
		t.Fatalf("execution: %+v", p.Execution)
	}
	if p.Approvals == nil || len(p.Approvals.RequiredFor) != 1 || p.Approvals.RequiredFor[0] != "tool.git.push_branch" {
		t.Fatalf("approvals: %+v", p.Approvals)
	}
	if p.Effects == nil || len(p.Effects.Permit) != 1 || len(p.Effects.PermitWithApproval) != 1 ||
		p.Effects.Permit[0] != "workspace.read" || p.Effects.PermitWithApproval[0] != "workspace.write" {
		t.Fatalf("effects: %+v", p.Effects)
	}
}

// TestMergeLowered_ToolPolicyCollision is the cross-ingress collision rule (ADR 005 §3): a lowered
// inline Tool/Policy whose name already exists (e.g. an imported YAML resource) is a load error.
func TestMergeLowered_ToolPolicyCollision(t *testing.T) {
	g := &spec.ProjectGraph{
		Tools:    map[string]*spec.ToolResource{"github": {Metadata: spec.Metadata{Name: "github"}}},
		Policies: map[string]*spec.PolicyResource{"default": {Metadata: spec.Metadata{Name: "default"}}},
	}
	res := &Result{
		Tools:    []*spec.ToolResource{{Metadata: spec.Metadata{Name: "github"}}},
		Policies: []*spec.PolicyResource{{Metadata: spec.Metadata{Name: "default"}}},
	}
	err := MergeLowered(g, res)
	if err == nil {
		t.Fatal("a duplicate Tool/Policy across ingress must be an error")
	}
	for _, want := range []string{"Tool", "github", "Policy", "default"} {
		if !containsStr(err.Error(), want) {
			t.Fatalf("collision error should name %q: %v", want, err)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestInlineTool_MCPYAMLEquivalence is the ADR 005 §2 golden for the .agent mcp transport block
// (issue #440): an inline mcp tool and its YAML twin normalize to byte-identical spec JSON, so the
// two front ends can never diverge on transport config.
func TestInlineTool_MCPYAMLEquivalence(t *testing.T) {
	agentSrc := `tool github {
    type mcp
    mcp {
        transport "stdio"
        command "npx"
        args { "-y" "@modelcontextprotocol/server-github" }
        headers { "Authorization" "env:GITHUB_TOKEN" }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Tool
metadata: {name: github}
spec:
  type: mcp
  mcp:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-github"]
    headers: {Authorization: "env:GITHUB_TOKEN"}
`
	inline := lowerToolsOrFatal(t, agentSrc).Tools[0]
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ToolResource)
	if normSpecJSON(t, inline) != normSpecJSON(t, fromYAML) {
		t.Fatalf("inline mcp tool differs from YAML twin:\n inline: %s\n yaml:   %s", normSpecJSON(t, inline), normSpecJSON(t, fromYAML))
	}
}

// TestInlineTool_HTTPYAMLEquivalence is the ADR 005 §2 golden for the .agent http transport block.
func TestInlineTool_HTTPYAMLEquivalence(t *testing.T) {
	agentSrc := `tool webhook {
    type http
    http {
        baseUrl "https://api.example.com"
        headers { "Authorization" "env:API_TOKEN" }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Tool
metadata: {name: webhook}
spec:
  type: http
  http:
    baseUrl: "https://api.example.com"
    headers: {Authorization: "env:API_TOKEN"}
`
	inline := lowerToolsOrFatal(t, agentSrc).Tools[0]
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "webhook.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ToolResource)
	if normSpecJSON(t, inline) != normSpecJSON(t, fromYAML) {
		t.Fatalf("inline http tool differs from YAML twin:\n inline: %s\n yaml:   %s", normSpecJSON(t, inline), normSpecJSON(t, fromYAML))
	}
}

// TestInlineEnvironment_YAMLEquivalence is the ADR 005 §2 golden for the .agent environment overlay
// block (issue #440): an inline environment and its YAML twin lower to byte-identical spec JSON.
func TestInlineEnvironment_YAMLEquivalence(t *testing.T) {
	agentSrc := `environment prod {
    overrides {
        agents {
            reviewer {
                model anthropic/claude-sonnet-5
                constraints { timeoutSeconds 300 }
            }
        }
        policies {
            guarded-writes {
                execution { maxTotalCostUsd 10 }
                approvals { requiredFor { tool.workspace.run_tests } }
            }
        }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Environment
metadata: {name: prod}
spec:
  overrides:
    agents:
      reviewer:
        model: "anthropic/claude-sonnet-5"
        constraints: {timeoutSeconds: 300}
    policies:
      guarded-writes:
        execution: {maxTotalCostUsd: 10}
        approvals: {requiredFor: [tool.workspace.run_tests]}
`
	res := lowerToolsOrFatal(t, agentSrc)
	if len(res.Environments) != 1 {
		t.Fatalf("expected 1 environment, got %d", len(res.Environments))
	}
	inline := res.Environments[0]

	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "prod.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.EnvironmentResource)

	inJSON, err := json.Marshal(inline.Spec)
	if err != nil {
		t.Fatal(err)
	}
	yJSON, err := json.Marshal(fromYAML.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(inJSON) != string(yJSON) {
		t.Fatalf("inline environment differs from YAML twin:\n inline: %s\n yaml:   %s", inJSON, yJSON)
	}
}

// TestInlineProvider_YAMLEquivalence is the ADR 005 §2 golden for the .agent `provider` decl (issue
// #440): an inline provider alias lowers into ProjectSpec.Providers.Models byte-identically to its
// YAML twin, so the two front ends can never diverge on custom-provider config.
func TestInlineProvider_YAMLEquivalence(t *testing.T) {
	agentSrc := `provider corporate-claude {
    type anthropic
    baseUrl "https://api.anthropic.com"
    apiKeyFrom "env:CORP_ANTHROPIC_KEY"
    workspaceIdFrom "env:CORP_WORKSPACE"
}`
	res := lowerToolsOrFatal(t, agentSrc)
	if len(res.Providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(res.Providers))
	}
	inline := res.ToGraph().Spec.Providers

	yamlSrc := `apiVersion: agentic.dev/v0
kind: Project
metadata: {name: demo}
spec:
  providers:
    models:
      corporate-claude:
        type: anthropic
        apiKeyFrom: "env:CORP_ANTHROPIC_KEY"
        workspaceIdFrom: "env:CORP_WORKSPACE"
        baseUrl: "https://api.anthropic.com"
`
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ProjectResource).Spec.Providers

	inJSON, err := json.Marshal(inline)
	if err != nil {
		t.Fatal(err)
	}
	yJSON, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(inJSON) != string(yJSON) {
		t.Fatalf("inline provider differs from YAML twin:\n inline: %s\n yaml:   %s", inJSON, yJSON)
	}
}

// TestInlineProvider_MissingTypeDiag: a provider without a type is a lowering diagnostic (the type
// selects the client and is required in the YAML config too).
func TestInlineProvider_MissingTypeDiag(t *testing.T) {
	f, diags := lang.Parse("t.agent", "provider p {\n    apiKeyFrom \"env:K\"\n}")
	if diags.HasErrors() {
		t.Fatalf("unexpected parse errors: %v", diags)
	}
	_, ld := LowerFile(f, Options{})
	if !ld.HasErrors() {
		t.Fatal("a provider with no type must be a lowering error")
	}
}

// TestInlineProvider_InvalidBaseURL: a custom endpoint must be an absolute http(s) URL (issue #546).
func TestInlineProvider_InvalidBaseURL(t *testing.T) {
	cases := []string{
		`provider p { type openai baseUrl "not-a-url" }`,
		`provider p { type openai baseUrl "ftp://example.com" }`,
		`provider p { type openai baseUrl "https://" }`,
	}
	for _, src := range cases {
		f, diags := lang.Parse("t.agent", src)
		if diags.HasErrors() {
			t.Fatalf("parse %q: %v", src, diags)
		}
		_, ld := LowerFile(f, Options{})
		if !ld.HasErrors() {
			t.Fatalf("invalid baseUrl must be a lowering error: %s", src)
		}
		if !containsStr(ld.Error(), "baseUrl") {
			t.Fatalf("diag should name baseUrl: %v", ld)
		}
	}
}

// TestMergeLowered_ProviderCollision: a lowered provider alias that already exists (a YAML
// providers.models entry, or another .agent provider) is a load error (ADR 005 §3), never a silent
// endpoint/credential swap.
func TestMergeLowered_ProviderCollision(t *testing.T) {
	g := &spec.ProjectGraph{
		Spec: spec.ProjectSpec{Providers: &spec.ProjectProviders{
			Models: map[string]spec.ModelProviderConfig{"corp": {Type: "anthropic"}},
		}},
	}
	res := &Result{Providers: []LoweredProvider{{Name: "corp", Config: spec.ModelProviderConfig{Type: "openai"}}}}
	err := MergeLowered(g, res)
	if err == nil {
		t.Fatal("a duplicate provider alias across ingress must be an error")
	}
	for _, want := range []string{"provider", "corp"} {
		if !containsStr(err.Error(), want) {
			t.Fatalf("collision error should name %q: %v", want, err)
		}
	}
	// The pre-existing YAML config must be untouched by the failed merge.
	if g.Spec.Providers.Models["corp"].Type != "anthropic" {
		t.Fatalf("failed merge mutated the existing provider: %+v", g.Spec.Providers.Models["corp"])
	}
}

// TestInlineDefaults_YAMLEquivalence is the ADR 005 §2 golden for the .agent `defaults` block (issue
// #440, ADR 007): an inline defaults block and its YAML twin normalize to byte-identical spec JSON, so
// the two front ends never diverge on project-wide fallbacks.
func TestInlineDefaults_YAMLEquivalence(t *testing.T) {
	agentSrc := `defaults {
    policy default
    model anthropic/claude-sonnet-5
    runtime container
}`
	f, diags := lang.Parse("t.agent", agentSrc)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	res, ld := LowerFile(f, Options{})
	if ld.HasErrors() {
		t.Fatalf("lower: %v", ld)
	}
	if res.Defaults == nil {
		t.Fatal("expected a lowered defaults block")
	}
	inline := res.ToGraph().Spec.Defaults

	yamlSrc := `apiVersion: agentic.dev/v0
kind: Project
metadata: {name: demo}
spec:
  defaults:
    policy: default
    model: anthropic/claude-sonnet-5
    runtime: container
`
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ProjectResource).Spec.Defaults

	inJSON, err := json.Marshal(inline)
	if err != nil {
		t.Fatal(err)
	}
	yJSON, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(inJSON) != string(yJSON) {
		t.Fatalf("inline defaults differs from YAML twin:\n inline: %s\n yaml:   %s", inJSON, yJSON)
	}
}

// TestLowerDefaults_DuplicateBlockDiag: a project may declare `defaults` at most once; a second block
// in the same file is a lowering error, never a silent last-wins override of project-wide fallbacks.
func TestLowerDefaults_DuplicateBlockDiag(t *testing.T) {
	f, diags := lang.Parse("t.agent", "defaults {\n    policy a\n}\ndefaults {\n    policy b\n}")
	if diags.HasErrors() {
		t.Fatalf("unexpected parse errors: %v", diags)
	}
	_, ld := LowerFile(f, Options{})
	if !ld.HasErrors() {
		t.Fatal("two `defaults` blocks must be a lowering error")
	}
}

// TestMergeLowered_DefaultsCollision: a lowered `defaults` block colliding with an existing
// spec.defaults (from YAML or another .agent block) is a load error (ADR 005 §3), never a silent
// swap of which policy/model applies project-wide.
func TestMergeLowered_DefaultsCollision(t *testing.T) {
	g := &spec.ProjectGraph{
		Spec: spec.ProjectSpec{Defaults: &spec.ProjectDefaults{Policy: "yaml-policy"}},
	}
	res := &Result{Defaults: &spec.ProjectDefaults{Policy: "agent-policy"}}
	err := MergeLowered(g, res)
	if err == nil {
		t.Fatal("a duplicate defaults block across ingress must be an error")
	}
	if !containsStr(err.Error(), "defaults") {
		t.Fatalf("collision error should name `defaults`: %v", err)
	}
	// The pre-existing YAML defaults must be untouched by the failed merge.
	if g.Spec.Defaults.Policy != "yaml-policy" {
		t.Fatalf("failed merge mutated the existing defaults: %+v", g.Spec.Defaults)
	}
}

// TestInlineProjectLimits_YAMLEquivalence is the ADR 005 §2 golden for the top-level `limits` block
// (issue #440, ADR 007): an inline project limits block and its YAML twin normalize to byte-identical
// spec JSON, so the two front ends never diverge on the project-wide execution-limit baseline.
func TestInlineProjectLimits_YAMLEquivalence(t *testing.T) {
	agentSrc := `limits {
    maxToolInputBytes 4096
    maxToolOutputBytes 8192
    maxWorkflowNesting 4
    maxLoopIterations 100
    toolInputExceedPolicy fail
    checkpointExceedPolicy fail
}`
	f, diags := lang.Parse("t.agent", agentSrc)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	res, ld := LowerFile(f, Options{})
	if ld.HasErrors() {
		t.Fatalf("lower: %v", ld)
	}
	if res.Limits == nil {
		t.Fatal("expected a lowered project limits block")
	}
	inline := res.ToGraph().Spec.Limits

	yamlSrc := `apiVersion: agentic.dev/v0
kind: Project
metadata: {name: demo}
spec:
  limits:
    maxToolInputBytes: 4096
    maxToolOutputBytes: 8192
    maxWorkflowNesting: 4
    maxLoopIterations: 100
    toolInputExceedPolicy: fail
    checkpointExceedPolicy: fail
`
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ProjectResource).Spec.Limits

	inJSON, err := json.Marshal(inline)
	if err != nil {
		t.Fatal(err)
	}
	yJSON, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(inJSON) != string(yJSON) {
		t.Fatalf("inline project limits differs from YAML twin:\n inline: %s\n yaml:   %s", inJSON, yJSON)
	}
}

// TestLowerLimits_CheckpointTruncateRejectedByValidation locks the grammar's authorable surface to the
// validated envelope: a `checkpointExceedPolicy truncate` limits block lowers cleanly (the grammar
// accepts the enum), but graph validation rejects it as unsafe (`spec.ValidateExecutionLimits`,
// checkpoint truncation loses durable state) — for BOTH the top-level project baseline and a per-tool
// override. This is the end-to-end guard the equivalence goldens can't give, since they compare lowered
// JSON without ever validating a graph (review of #473 / recurrence of #468).
func TestLowerLimits_CheckpointTruncateRejectedByValidation(t *testing.T) {
	cases := map[string]string{
		"project baseline": `limits {
    checkpointExceedPolicy truncate
}`,
		"per-tool override": `tool bulk {
    type native
    limits {
        checkpointExceedPolicy truncate
    }
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f, diags := lang.Parse("t.agent", src)
			if diags.HasErrors() {
				t.Fatalf("parse: %v", diags)
			}
			res, ld := LowerFile(f, Options{})
			if ld.HasErrors() {
				t.Fatalf("the grammar must accept the enum at lower time: %v", ld)
			}
			g := res.ToGraph()
			g.Meta.Name = "demo"
			spec.NormalizeProjectGraph(g)
			err := spec.ValidateProjectGraph(g, t.TempDir())
			if err == nil {
				t.Fatal("checkpointExceedPolicy truncate must be rejected by graph validation")
			}
			if !containsStr(err.Error(), "checkpointExceedPolicy") {
				t.Fatalf("validation error should name checkpointExceedPolicy: %v", err)
			}
		})
	}
}

// TestLowerProjectLimits_DuplicateBlockDiag: a project may declare a top-level `limits` block at most
// once; a second block in the same file is a lowering error, never a silent last-wins baseline swap.
func TestLowerProjectLimits_DuplicateBlockDiag(t *testing.T) {
	f, diags := lang.Parse("t.agent", "limits {\n    maxToolInputBytes 1\n}\nlimits {\n    maxToolInputBytes 2\n}")
	if diags.HasErrors() {
		t.Fatalf("unexpected parse errors: %v", diags)
	}
	_, ld := LowerFile(f, Options{})
	if !ld.HasErrors() {
		t.Fatal("two top-level `limits` blocks must be a lowering error")
	}
}

// TestMergeLowered_ProjectLimitsCollision: a lowered top-level `limits` block colliding with an
// existing spec.limits (from YAML or another .agent block) is a load error (ADR 005 §3), never a
// silent swap of the enforced execution-limit ceiling.
func TestMergeLowered_ProjectLimitsCollision(t *testing.T) {
	g := &spec.ProjectGraph{
		Spec: spec.ProjectSpec{Limits: &spec.ExecutionLimits{MaxToolInputBytes: 1}},
	}
	res := &Result{Limits: &spec.ExecutionLimits{MaxToolInputBytes: 2}}
	err := MergeLowered(g, res)
	if err == nil {
		t.Fatal("a duplicate project limits block across ingress must be an error")
	}
	if !containsStr(err.Error(), "limits") {
		t.Fatalf("collision error should name `limits`: %v", err)
	}
	if g.Spec.Limits.MaxToolInputBytes != 1 {
		t.Fatalf("failed merge mutated the existing limits: %+v", g.Spec.Limits)
	}
}

// TestInlineTool_WorkspaceYAMLEquivalence is the ADR 005 §2 golden for the .agent workspace tool
// sub-block (issue #440): an inline workspace tool and its YAML twin normalize to byte-identical spec
// JSON, so the two front ends never diverge on native workspace config.
func TestInlineTool_WorkspaceYAMLEquivalence(t *testing.T) {
	agentSrc := `tool workspace {
    type native
    workspace {
        root "sandbox"
        testCommand "go test ./..."
    }
    safety {
        trusted true
        sideEffects true
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Tool
metadata: {name: workspace}
spec:
  type: native
  workspace:
    root: sandbox
    testCommand: "go test ./..."
  safety: {trusted: true, sideEffects: true}
`
	inline := lowerToolsOrFatal(t, agentSrc).Tools[0]
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "workspace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ToolResource)
	if normSpecJSON(t, inline) != normSpecJSON(t, fromYAML) {
		t.Fatalf("inline workspace tool differs from YAML twin:\n inline: %s\n yaml:   %s", normSpecJSON(t, inline), normSpecJSON(t, fromYAML))
	}
}

// TestObjectReturn_YAMLOutputEquivalence: an object-literal return lowers to the same flat
// WorkflowOutput.Value a YAML `output.value: {…}` produces (issue #440), so the .agent form is a true
// substitute for the multi-field YAML output that previously had no .agent representation.
func TestObjectReturn_YAMLOutputEquivalence(t *testing.T) {
	agentSrc := `workflow snippet(input: any) {
    c = helper.echo(product: input.product)
    return { product: c.echo.product, subject: c.echo.subject }
}`
	f, d := lang.Parse("t.agent", agentSrc)
	if d.HasErrors() {
		t.Fatalf("parse: %v", d)
	}
	res, ld := LowerFile(f, Options{})
	if ld.HasErrors() {
		t.Fatalf("lower: %v", ld)
	}
	inline := res.Workflows[0]

	yamlSrc := `apiVersion: agentic.dev/v0
kind: Workflow
metadata: {name: snippet}
spec:
  steps:
    - id: c
      uses: tool.helper.echo
      with: {product: "${input.product}"}
  output:
    value:
      product: ${steps.c.output.echo.product}
      subject: ${steps.c.output.echo.subject}
`
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "snippet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.WorkflowResource)

	ij, _ := json.Marshal(inline.Spec.Output)
	yj, _ := json.Marshal(fromYAML.Spec.Output)
	if string(ij) != string(yj) {
		t.Fatalf("object-return output differs from YAML twin:\n inline: %s\n yaml:   %s", ij, yj)
	}
}

// TestScalarReturn_StillWrapsValue: a non-object return keeps the single-`value` envelope (#440).
func TestScalarReturn_StillWrapsValue(t *testing.T) {
	f, d := lang.Parse("t.agent", "workflow w(input: any) {\n    c = a.b(x: input.x)\n    return c\n}")
	if d.HasErrors() {
		t.Fatalf("parse: %v", d)
	}
	res, ld := LowerFile(f, Options{})
	if ld.HasErrors() {
		t.Fatalf("lower: %v", ld)
	}
	v := res.Workflows[0].Spec.Output.Value
	if len(v) != 1 {
		t.Fatalf("scalar return must have one output field, got %v", v)
	}
	if _, ok := v["value"]; !ok {
		t.Fatalf("scalar return must use the {value: …} envelope, got %v", v)
	}
}

// TestInlineTool_RetryAndOpSchemaYAMLEquivalence is the ADR 005 §2 golden for the .agent tool `retry`
// block and per-operation `schema` (issue #440): they lower byte-identically to the YAML twin.
func TestInlineTool_RetryAndOpSchemaYAMLEquivalence(t *testing.T) {
	agentSrc := `tool github {
    type mcp
    retry {
        maxAttempts 3
        backoff "exponential"
    }
    operations {
        create_issue { schema "schemas/CreateIssue.json"  effects { github.write } }
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Tool
metadata: {name: github}
spec:
  type: mcp
  retry: {maxAttempts: 3, backoff: exponential}
  operations:
    create_issue: {schema: schemas/CreateIssue.json, effects: [github.write]}
`
	inline := lowerToolsOrFatal(t, agentSrc).Tools[0]
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "github.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ToolResource)
	if normSpecJSON(t, inline) != normSpecJSON(t, fromYAML) {
		t.Fatalf("inline retry/op-schema tool differs from YAML twin:\n inline: %s\n yaml:   %s", normSpecJSON(t, inline), normSpecJSON(t, fromYAML))
	}
}

// TestInlinePolicy_ToolsYAMLEquivalence is the golden for policy `tools { forbidUnknownTools }` (#440).
func TestInlinePolicy_ToolsYAMLEquivalence(t *testing.T) {
	agentSrc := `policy strict {
    tools { forbidUnknownTools true }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Policy
metadata: {name: strict}
spec:
  tools: {forbidUnknownTools: true}
`
	inline := lowerToolsOrFatal(t, agentSrc).Policies[0]
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "strict.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.PolicyResource)
	if normPolicySpecJSON(t, inline) != normPolicySpecJSON(t, fromYAML) {
		t.Fatalf("inline policy tools differs from YAML twin:\n inline: %s\n yaml:   %s", normPolicySpecJSON(t, inline), normPolicySpecJSON(t, fromYAML))
	}
}

// TestInlineTool_LimitsYAMLEquivalence is the ADR 005 §2 golden for the .agent tool `limits` block
// (issue #440): the full 9-field per-tool ExecutionLimits override lowers byte-identically to YAML.
func TestInlineTool_LimitsYAMLEquivalence(t *testing.T) {
	agentSrc := `tool bulk {
    type native
    limits {
        maxToolInputBytes 1024
        maxToolOutputBytes 2048
        maxCheckpointBytes 4096
        maxStateBytes 8192
        maxWorkflowNesting 3
        maxLoopIterations 10
        toolInputExceedPolicy truncate
        toolOutputExceedPolicy fail
        checkpointExceedPolicy fail
    }
}`
	yamlSrc := `apiVersion: agentic.dev/v0
kind: Tool
metadata: {name: bulk}
spec:
  type: native
  limits:
    maxToolInputBytes: 1024
    maxToolOutputBytes: 2048
    maxCheckpointBytes: 4096
    maxStateBytes: 8192
    maxWorkflowNesting: 3
    maxLoopIterations: 10
    toolInputExceedPolicy: truncate
    toolOutputExceedPolicy: fail
    checkpointExceedPolicy: fail
`
	inline := lowerToolsOrFatal(t, agentSrc).Tools[0]
	dec, err := spec.ParseResourceFromBytes([]byte(yamlSrc), "bulk.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromYAML := dec.Resource.(*spec.ToolResource)
	if normSpecJSON(t, inline) != normSpecJSON(t, fromYAML) {
		t.Fatalf("inline limits tool differs from YAML twin:\n inline: %s\n yaml:   %s", normSpecJSON(t, inline), normSpecJSON(t, fromYAML))
	}
}

// TestInlineTool_LimitsPrecedence proves a .agent per-tool limits override wins over project/workflow
// limits at runtime resolution — the enforced-precedence behavior that justifies the grammar (issue
// #440; per-tool merge in spec.ResolveExecutionLimits, engine/limits.go).
func TestInlineTool_LimitsPrecedence(t *testing.T) {
	tr := lowerToolsOrFatal(t, `tool bulk {
    type native
    limits { maxToolInputBytes 500 }
}`).Tools[0]
	project := &spec.ProjectSpec{Limits: &spec.ExecutionLimits{MaxToolInputBytes: 999999}}
	resolved := spec.ResolveExecutionLimits(project, nil, &tr.Spec)
	if resolved.MaxToolInputBytes != 500 {
		t.Fatalf("per-tool limit must override project: got MaxToolInputBytes=%d, want 500", resolved.MaxToolInputBytes)
	}
}
