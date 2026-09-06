package spec

import "github.com/Terfyn/terfyn/internal/schema"

// --- Project (design doc §7.1) ---

type ProjectSpec struct {
	// Imports is a YAML source-loader mechanism, not part of the canonical model: the loader expands it
	// to find additional YAML files (internal/project.expandImports). Under ADR 007 `.agent` is the sole
	// authoring surface and has no import concept, and exported YAML is a one-way serialization of the
	// graph, not a reloadable manifest — so imports has no future architectural role. The struct field is
	// retained only temporarily so `terfyn migrate` can still read a legacy YAML project; it disappears
	// with the YAML loader (ADR 007 steps 6–7). Do not build new behavior on it.
	Imports   []string                `yaml:"imports,omitempty" json:"imports,omitempty"`
	Defaults  *ProjectDefaults        `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Providers *ProjectProviders       `yaml:"providers,omitempty" json:"providers,omitempty"`
	State     *ProjectStateConfig     `yaml:"state,omitempty" json:"state,omitempty"`
	Traces    *ProjectTracesConfig    `yaml:"traces,omitempty" json:"traces,omitempty"`
	Telemetry *ProjectTelemetryConfig `yaml:"telemetry,omitempty" json:"telemetry,omitempty"`
	// Limits bounds tool I/O and checkpoint bytes for all workflows (issue #117).
	Limits *ExecutionLimits `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type ProjectDefaults struct {
	Runtime string `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Model   string `yaml:"model,omitempty" json:"model,omitempty"`
	Policy  string `yaml:"policy,omitempty" json:"policy,omitempty"`
}

type ProjectProviders struct {
	Models map[string]ModelProviderConfig `yaml:"models,omitempty" json:"models,omitempty"`
}

type ModelProviderConfig struct {
	Type       string `yaml:"type" json:"type"`
	APIKeyFrom string `yaml:"apiKeyFrom,omitempty" json:"apiKeyFrom,omitempty"`
	// WorkspaceIDFrom optionally sources a workspace id (same env:VAR form as
	// APIKeyFrom). The anthropic provider sends it as the anthropic-workspace-id
	// header, which Anthropic requires for identity-linked API keys.
	WorkspaceIDFrom string `yaml:"workspaceIdFrom,omitempty" json:"workspaceIdFrom,omitempty"`
}

type ProjectStateConfig struct {
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	DSN     string `yaml:"dsn,omitempty" json:"dsn,omitempty"`
}

type ProjectTracesConfig struct {
	Backend         string                     `yaml:"backend,omitempty" json:"backend,omitempty"`
	RetentionDays   int                        `yaml:"retentionDays,omitempty" json:"retentionDays,omitempty"`
	RedactKeys      []string                   `yaml:"redactKeys,omitempty" json:"redactKeys,omitempty"`
	MaxPayloadBytes int                        `yaml:"maxPayloadBytes,omitempty" json:"maxPayloadBytes,omitempty"`
	Redaction       *ProjectTracesRedactionCfg `yaml:"redaction,omitempty" json:"redaction,omitempty"`
}

// ProjectTracesRedactionCfg tunes sanitize/redact/truncate for trace payloads (issue #110).
type ProjectTracesRedactionCfg struct {
	RedactKeys      []string `yaml:"redactKeys,omitempty" json:"redactKeys,omitempty"`
	MaxDepth        int      `yaml:"maxDepth,omitempty" json:"maxDepth,omitempty"`
	MaxBytes        int      `yaml:"maxBytes,omitempty" json:"maxBytes,omitempty"`
	MaxStringChars  int      `yaml:"maxStringChars,omitempty" json:"maxStringChars,omitempty"`
	MaxPayloadBytes int      `yaml:"maxPayloadBytes,omitempty" json:"maxPayloadBytes,omitempty"`
}

// ProjectTelemetryConfig enables optional OpenTelemetry trace export (issue #108).
// SQLite traces remain the local source of truth; OTLP export is additive.
type ProjectTelemetryConfig struct {
	Enabled       bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ServiceName   string `yaml:"serviceName,omitempty" json:"serviceName,omitempty"`
	Endpoint      string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	ConsoleExport bool   `yaml:"consoleExport,omitempty" json:"consoleExport,omitempty"`
}

// --- Agent (design doc §7.2, MVP fields) ---

type AgentSpec struct {
	Description  string   `yaml:"description,omitempty" json:"description,omitempty"`
	Model        string   `yaml:"model,omitempty" json:"model,omitempty"`
	Instructions string   `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Tools        []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// ToolsPos is diagnostic metadata aligned with Tools (issue #187). Not YAML/JSON identity.
	ToolsPos    []Pos             `yaml:"-" json:"-"`
	Policy      string            `yaml:"policy,omitempty" json:"policy,omitempty"`
	Constraints *AgentConstraints `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	Input       *AgentIO          `yaml:"input,omitempty" json:"input,omitempty"`
	Output      *AgentIO          `yaml:"output,omitempty" json:"output,omitempty"`
	// Note: spec.runtime and spec.memory were removed from the canonical model (ADR 007 step 1); they had
	// no runtime semantics. `terfyn migrate --to-agent` still accepts legacy YAML with them (warn + omit).
}

type AgentConstraints struct {
	MaxIterations int `yaml:"maxIterations,omitempty" json:"maxIterations,omitempty"`
	// MaxTokens caps a single completion's output tokens (issue #514). Unset/zero resolves to
	// DefaultAgentMaxTokens via ResolveMaxTokens; it is sent to the provider as max_tokens by both the
	// Anthropic and OpenAI adapters. A coding agent that writes a whole file needs far more than the
	// old 4096 chat-era cap, so this is author-tunable per agent.
	MaxTokens      int `yaml:"maxTokens,omitempty" json:"maxTokens,omitempty"`
	TimeoutSeconds int `yaml:"timeoutSeconds,omitempty" json:"timeoutSeconds,omitempty"`
	// Temperature is a pointer so an explicit 0 (deterministic sampling) is distinct from unset
	// (nil → provider default). Every set value, including 0, is folded into the spec hash, shown in
	// plan diffs, merged by environment overlays, and sent to the provider (issue #388).
	Temperature             *float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	RequireStructuredOutput bool     `yaml:"requireStructuredOutput,omitempty" json:"requireStructuredOutput,omitempty"`
}

type AgentIO struct {
	Schema string `yaml:"schema,omitempty" json:"schema,omitempty"`
	// Resolved is the compiled JSON Schema loaded from Schema during validate (issue #193).
	// Diagnostic/derived; not identity. Omitted from hashes.
	Resolved *schema.Document `yaml:"-" json:"-"`
}

// --- Tool (design doc §7.3, MVP types: mcp, http, native) ---

type ToolSpec struct {
	Type  string     `yaml:"type,omitempty" json:"type,omitempty"`
	MCP   *ToolMCP   `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	HTTP  *ToolHTTP  `yaml:"http,omitempty" json:"http,omitempty"`
	Retry *ToolRetry `yaml:"retry,omitempty" json:"retry,omitempty"`
	// Note: spec.permissions (allow/deny) was removed from the canonical model (ADR 007 step 1); it was a
	// non-authoritative plan-only write heuristic superseded by operations/effects capability analysis.
	// `terfyn migrate --to-agent` still accepts legacy YAML with it (warn + omit).
	// Safety carries blast-radius metadata for fail-closed policy derivation (issue #103).
	Safety *ToolSafety `yaml:"safety,omitempty" json:"safety,omitempty"`
	// Operations declares per-operation named effects (issue #188, ADR 002). Additive to Safety.
	Operations map[string]ToolOperation `yaml:"operations,omitempty" json:"operations,omitempty"`
	// OperationsDeclared is true when the mapping included an `operations` key (even if empty). It
	// is the presence bit for the closed-world capability manifest (issue #204): an empty
	// `operations: {}` is a *closed* manifest that denies every operation, distinct from an omitted
	// `operations` (an open callable set, backward compatible). Because `Operations` is omitempty an
	// empty map serializes away, so this bit carries closedness — and it is **part of identity**
	// (`json:"operationsDeclared"`), not merely diagnostic: it flows into the normalized spec hash,
	// plan diffs, `NormalizedSpecJSON`, and `graphFromApplied`, so deleting `operations:` from a
	// locked tool is a visible plan change rather than a silent reopen, and the deployed manifest
	// reconstructed from applied spec (and the #207 snapshot) sees the same closed world runtime
	// enforces. Not author-settable (`yaml:"-"`); it is derived from key presence during load.
	// `omitempty` keeps the field absent (JSON unchanged) for the common open tool.
	OperationsDeclared bool `yaml:"-" json:"operationsDeclared,omitempty"`
	// Limits optionally overrides project execution byte limits for this tool (issue #117).
	Limits *ExecutionLimits `yaml:"limits,omitempty" json:"limits,omitempty"`
	// Workspace configures the native workspace adapter (read_file / write_file / run_tests) in the
	// reviewed Tool resource rather than via environment variables (issue #323 follow-up). When set,
	// it takes precedence over TERFYN_WORKSPACE_ROOT / TERFYN_WORKSPACE_TEST_COMMAND; when absent the
	// env fallback applies (backward compatible). `omitempty` keeps the field out of the normalized
	// spec hash for tools that do not declare it.
	Workspace *ToolWorkspace `yaml:"workspace,omitempty" json:"workspace,omitempty"`
}

// ToolOperation is one named operation on a Tool and the effects it may produce.
type ToolOperation struct {
	Effects []string `yaml:"effects,omitempty" json:"effects,omitempty"`
	// Schema is a JSON Schema ref for this operation's input (the manifest's "operation → effects →
	// schema", completing #204). When set, a tool call's input is validated against it before
	// dispatch; absent means gradual (any input). Part of the capability manifest and identity.
	Schema string `yaml:"schema,omitempty" json:"schema,omitempty"`
	// Pos is the YAML map-key location of this operation (issue #187). Not identity.
	Pos Pos `yaml:"-" json:"-"`
	// EffectsPos is diagnostic metadata aligned with Effects (issue #187).
	EffectsPos []Pos `yaml:"-" json:"-"`
	// SchemaPos is diagnostic metadata for Schema (issue #187). Not identity.
	SchemaPos Pos `yaml:"-" json:"-"`
}

// ToolSafety describes trust and side effects for policy fallback when no explicit Policy rule matches.
// Omitted fields resolve to fail-closed defaults via [ResolveToolSafety].
type ToolSafety struct {
	Trusted          *bool `yaml:"trusted,omitempty" json:"trusted,omitempty"`
	SideEffects      *bool `yaml:"sideEffects,omitempty" json:"sideEffects,omitempty"`
	RequiresApproval *bool `yaml:"requiresApproval,omitempty" json:"requiresApproval,omitempty"`
}

// ResolvedToolSafety holds fully resolved safety flags after defaults and derivation.
type ResolvedToolSafety struct {
	Trusted          bool
	SideEffects      bool
	RequiresApproval bool
}

type ToolMCP struct {
	Transport string            `yaml:"transport,omitempty" json:"transport,omitempty"`
	Command   string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty" json:"args,omitempty"`
	URL       string            `yaml:"url,omitempty" json:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

type ToolHTTP struct {
	BaseURL string            `yaml:"baseUrl,omitempty" json:"baseUrl,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// ToolWorkspace is the declarative config for the native workspace adapter (issue #323 follow-up).
type ToolWorkspace struct {
	// Root is the sandbox directory read_file / write_file resolve within. A relative path is
	// resolved against the project root; an absolute path is used as-is. Every access is confined to
	// it via os.Root (symlink/`..` escapes refused). Empty falls back to TERFYN_WORKSPACE_ROOT.
	Root string `yaml:"root,omitempty" json:"root,omitempty"`
	// TestCommand is the command run_tests executes (via sh -c) in the root. It is operator config,
	// never a tool-call argument, so an agent cannot choose an arbitrary command. Empty falls back to
	// TERFYN_WORKSPACE_TEST_COMMAND.
	TestCommand string `yaml:"testCommand,omitempty" json:"testCommand,omitempty"`
}

type ToolRetry struct {
	MaxAttempts int    `yaml:"maxAttempts,omitempty" json:"maxAttempts,omitempty"`
	Backoff     string `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

// --- Workflow (design doc §7.4, MVP) ---

type WorkflowSpec struct {
	Description string           `yaml:"description,omitempty" json:"description,omitempty"`
	Runtime     string           `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Trigger     *WorkflowTrigger `yaml:"trigger,omitempty" json:"trigger,omitempty"`
	Input       *WorkflowInput   `yaml:"input,omitempty" json:"input,omitempty"`
	Policy      string           `yaml:"policy,omitempty" json:"policy,omitempty"`
	Steps       []WorkflowStep   `yaml:"steps,omitempty" json:"steps,omitempty"`
	Output      *WorkflowOutput  `yaml:"output,omitempty" json:"output,omitempty"`
	// Limits optionally overrides project execution byte limits for this workflow (issue #117).
	Limits *ExecutionLimits `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type WorkflowTrigger struct {
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
}

type WorkflowInput struct {
	Schema string `yaml:"schema,omitempty" json:"schema,omitempty"`
	// Resolved is the compiled JSON Schema loaded from Schema during validate (issue #193).
	Resolved *schema.Document `yaml:"-" json:"-"`
}

type WorkflowStep struct {
	ID    string `yaml:"id,omitempty" json:"id,omitempty"`
	Uses  string `yaml:"uses,omitempty" json:"uses,omitempty"`
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	// Workflow names another Workflow resource in the project graph (issue #194, ADR 002).
	// The callee is statically named; with: maps to the callee's input and the callee's
	// output.value becomes this step's output. Exactly one of uses, agent, workflow, or approval.
	Workflow string `yaml:"workflow,omitempty" json:"workflow,omitempty"`
	// Approval marks a graph-node human pause (issue #195, ADR 002). true or a mapping
	// with optional description/redactKeys. Policy still gates tool-call approvals;
	// this field only says where the workflow suspends. XOR with uses, agent, workflow.
	Approval *WorkflowApprovalValue `yaml:"approval,omitempty" json:"approval,omitempty"`
	With     map[string]any         `yaml:"with,omitempty" json:"with,omitempty"`
	// Needs lists step IDs that must complete before this step runs (issue #192, ADR 002).
	// Edges are static and author-declared. Empty/omitted means:
	//   - if no step in the workflow declares needs, YAML order is an implicit chain
	//     (backward compatible sequential execution);
	//   - if any step declares needs, omitted needs means this step is a root
	//     (ready immediately, may run concurrently with other roots).
	Needs []string `yaml:"needs,omitempty" json:"needs,omitempty"`
	// Pos, UsesPos, AgentPos, WorkflowPos, ApprovalPos, and NeedsPos are diagnostic metadata only (issue #187).
	Pos         Pos   `yaml:"-" json:"-"`
	UsesPos     Pos   `yaml:"-" json:"-"`
	AgentPos    Pos   `yaml:"-" json:"-"`
	WorkflowPos Pos   `yaml:"-" json:"-"`
	ApprovalPos Pos   `yaml:"-" json:"-"`
	NeedsPos    []Pos `yaml:"-" json:"-"`
	// Synthetic marks a step SYNTHESIZED by flattening a `.agent` control-flow body
	// (an `if`/`for`/`while` arm) into the resource projection (ADR 002 §5, #305).
	// Such a step is a sound over-approximation for EFFECT ANALYSIS only — the union
	// over reachable branches — and is NOT an independently executable node: execution
	// runs the pinned execir program (control flow lives there), and since #278 the
	// resource projection is never executed. So the executable-graph invariants
	// (interpolation-predecessor `needs` wiring, per-field input-schema mapping of
	// `with`) do not apply to it; argument type safety comes from the checker's type
	// system, and effect analysis reads only `uses`/`agent`/`workflow`. Diagnostic
	// metadata, not identity (`yaml:"-" json:"-"`).
	Synthetic bool `yaml:"-" json:"-"`
	// NeedsDeclared is true when the mapping included a `needs` key (even if empty). Because `Needs`
	// is omitempty, an empty declared list would serialize away, so this bit is **part of identity**
	// (`json:"needsDeclared"`), not merely diagnostic: it is the DAG-mode signal
	// ([WorkflowUsesExplicitNeeds]) — an empty `needs:` opts the whole workflow into graph mode /
	// concurrent roots — and a deployment snapshot (#207) must reproduce graph vs sequential
	// execution on resume. Mirrors [ToolSpec.OperationsDeclared]. Not author-settable (`yaml:"-"`);
	// derived from key presence during load. `omitempty` keeps JSON unchanged for the common
	// implicit-sequential step.
	NeedsDeclared bool `yaml:"-" json:"needsDeclared,omitempty"`
}

type WorkflowOutput struct {
	Value map[string]any `yaml:"value,omitempty" json:"value,omitempty"`
}

// --- Policy (design doc §7.5, MVP) ---

type PolicySpec struct {
	// Preset references a built-in policy preset (strict, permissive, shell_safe) as a base
	// for this Policy resource; local spec fields layer on top (issue #104).
	Preset string `yaml:"preset,omitempty" json:"preset,omitempty"`
	// ResolvedPreset is populated during [NormalizeProjectGraph] when a preset is expanded; not author YAML.
	ResolvedPreset string           `yaml:"-" json:"-"`
	Execution      *PolicyExecution `yaml:"execution,omitempty" json:"execution,omitempty"`
	Tools          *PolicyTools     `yaml:"tools,omitempty" json:"tools,omitempty"`
	Approvals      *PolicyApprovals `yaml:"approvals,omitempty" json:"approvals,omitempty"`
	// Effects is the static permit set for transitive tool effects (issue #190).
	Effects *PolicyEffects `yaml:"effects,omitempty" json:"effects,omitempty"`
	// Hitl configures human-in-the-loop approval gates for gated tool calls (issue #106).
	Hitl *HitlPolicy `yaml:"hitl,omitempty" json:"hitl,omitempty"`
	// Note: spec.security (networkAccess/secretAccess) was removed from the canonical model (ADR 007 step
	// 1); it was never enforced. `terfyn migrate --to-agent` still accepts legacy YAML with it (warn + omit).
}

// PolicyEffects lists effect identifiers a Policy permits (issue #190, ADR 002).
// permit is unattended allow; permitWithApproval is allowed only subject to approval.
// A missing or empty block permits nothing once any Tool declares spec.operations effects.
type PolicyEffects struct {
	Permit                []string `yaml:"permit,omitempty" json:"permit,omitempty"`
	PermitWithApproval    []string `yaml:"permitWithApproval,omitempty" json:"permitWithApproval,omitempty"`
	PermitPos             []Pos    `yaml:"-" json:"-"`
	PermitWithApprovalPos []Pos    `yaml:"-" json:"-"`
}

type PolicyExecution struct {
	MaxWallClockSeconds int     `yaml:"maxWallClockSeconds,omitempty" json:"maxWallClockSeconds,omitempty"`
	MaxTotalCostUsd     float64 `yaml:"maxTotalCostUsd,omitempty" json:"maxTotalCostUsd,omitempty"`
	// MaxIterations is the policy's ceiling on an agent's constraints.maxIterations — the governed,
	// environment-overridable replacement for the frozen global cap (issue #522). Unset/zero keeps the
	// default HardAgentMaxIterations (32); resolved via ResolveMaxIterations.
	MaxIterations           int  `yaml:"maxIterations,omitempty" json:"maxIterations,omitempty"`
	RequireStructuredOutput bool `yaml:"requireStructuredOutput,omitempty" json:"requireStructuredOutput,omitempty"`
}

type PolicyTools struct {
	ForbidUnknownTools bool `yaml:"forbidUnknownTools,omitempty" json:"forbidUnknownTools,omitempty"`
}

type PolicyApprovals struct {
	RequiredFor []string `yaml:"requiredFor,omitempty" json:"requiredFor,omitempty"`
	// RequiredForPos is diagnostic metadata aligned with RequiredFor (issue #187).
	RequiredForPos []Pos `yaml:"-" json:"-"`
	// RequireAllTools gates every tool call when true (strict preset). Pointer preserves tri-state merge.
	RequireAllTools *bool `yaml:"requireAllTools,omitempty" json:"requireAllTools,omitempty"`
	// Permissive skips tool-call approval when true (permissive preset). Pointer preserves tri-state merge.
	Permissive *bool `yaml:"permissive,omitempty" json:"permissive,omitempty"`
}

// --- Environment (design doc §7.6, MVP overrides) ---

type EnvironmentSpec struct {
	Overrides *EnvironmentOverrides `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

type EnvironmentOverrides struct {
	Agents   map[string]AgentOverride  `yaml:"agents,omitempty" json:"agents,omitempty"`
	Policies map[string]PolicyOverride `yaml:"policies,omitempty" json:"policies,omitempty"`
}

type AgentOverride struct {
	Model       string            `yaml:"model,omitempty" json:"model,omitempty"`
	Constraints *AgentConstraints `yaml:"constraints,omitempty" json:"constraints,omitempty"`
}

type PolicyOverride struct {
	Execution *PolicyExecution `yaml:"execution,omitempty" json:"execution,omitempty"`
	// Approvals merges extra requiredFor entries onto the named Policy (issue #171).
	// Overlay entries are unioned with the base list; empty overlay requiredFor is a no-op.
	Approvals *PolicyApprovals `yaml:"approvals,omitempty" json:"approvals,omitempty"`
}
