package lang

// Inline resource declarations (ADR 005, issue #333): top-level `tool { … }` and `policy { … }`
// blocks in .agent that lower to the same spec.ToolResource / spec.PolicyResource the YAML loader
// produces. Field pointers are nil when omitted; presence of a block is significant where the IR is
// presence-sensitive (notably ToolDecl.Operations — see the lowering, which sets OperationsDeclared).

// ToolDecl is a `tool <Name> { … }` declaration.
type ToolDecl struct {
	Pos       Pos
	Name      *Ident
	Type      *Ident              // type <native|mock|mcp|http>
	MCP       *ToolMCPBlock       // mcp { … } transport config (issue #440)
	HTTP      *ToolHTTPBlock      // http { … } transport config (issue #440)
	Workspace *ToolWorkspaceBlock // workspace { … } native workspace config (issue #440)
	Retry     *ToolRetryBlock     // retry { … } mcp/http call retry config (issue #440)
	Limits    *ToolLimitsBlock    // limits { … } per-tool execution-limit overrides (issue #440)
	Safety    *ToolSafetyBlock    // safety { … }
	// Operations is nil when the block is omitted (open callable set) and non-nil when present,
	// including an empty `operations {}` (a closed, deny-all manifest). This distinction is the
	// OperationsDeclared bit (#204) and MUST survive lowering.
	Operations *ToolOperations
}

// ToolMCPBlock is `mcp { transport … command … args { … } url … headers { … } }` (issue #440).
type ToolMCPBlock struct {
	Pos       Pos
	Transport *StringLit
	Command   *StringLit
	Args      []*StringLit
	URL       *StringLit
	Headers   []*HeaderPair
}

// ToolHTTPBlock is `http { baseUrl … headers { … } }` (issue #440).
type ToolHTTPBlock struct {
	Pos     Pos
	BaseURL *StringLit
	Headers []*HeaderPair
}

// ToolWorkspaceBlock is `workspace { root "…" testCommand "…" }` (issue #440): declarative config for
// the native workspace adapter. Each field is nil when omitted (env fallback applies).
type ToolWorkspaceBlock struct {
	Pos         Pos
	Root        *StringLit
	TestCommand *StringLit
}

// ToolRetryBlock is `retry { maxAttempts N backoff "…" }` (issue #440): mcp/http call retry config.
type ToolRetryBlock struct {
	Pos         Pos
	MaxAttempts *int
	Backoff     *StringLit
}

// ToolLimitsBlock is `limits { maxToolInputBytes N … toolInputExceedPolicy <truncate|fail> … }`
// (issue #440): per-tool overrides of the project/workflow execution limits, merged at top precedence
// by spec.ResolveExecutionLimits. Each field is nil when omitted. The three *ExceedPolicy fields are
// the `truncate` / `fail` enum, carried as an Ident.
type ToolLimitsBlock struct {
	Pos                    Pos
	MaxToolInputBytes      *int
	MaxToolOutputBytes     *int
	MaxCheckpointBytes     *int
	MaxStateBytes          *int
	MaxWorkflowNesting     *int
	MaxLoopIterations      *int
	ToolInputExceedPolicy  *Ident
	ToolOutputExceedPolicy *Ident
	CheckpointExceedPolicy *Ident
}

// HeaderPair is one `"<key>" "<value>"` entry in a headers { … } block.
type HeaderPair struct {
	Pos   Pos
	Key   *StringLit
	Value *StringLit
}

func (d *ToolDecl) Position() Pos { return d.Pos }
func (d *ToolDecl) declNode()     {}

// ToolSafetyBlock is `safety { trusted … sideEffects … requiresApproval … }`; each field is nil when omitted.
type ToolSafetyBlock struct {
	Pos              Pos
	Trusted          *bool
	SideEffects      *bool
	RequiresApproval *bool
}

// ToolOperations is the `operations { … }` block. Ops may be empty (an explicit `operations {}`).
type ToolOperations struct {
	Pos Pos
	Ops []*ToolOperationDecl
}

// ToolOperationDecl is one `"<op>" { schema "…" effects { … } }` entry in the operations block.
type ToolOperationDecl struct {
	Pos     Pos
	Name    *Ident
	Schema  *StringLit   // schema "…" per-operation input schema ref (issue #440); nil when omitted
	Effects []*EffectRef // effects { … } (bare dotted idents); nil when omitted
}

// PolicyDecl is a `policy <Name> { … }` declaration.
type PolicyDecl struct {
	Pos       Pos
	Name      *Ident
	Preset    *Ident // preset <name>; a built-in policy preset (e.g. shell_safe). nil if omitted.
	Execution *PolicyExecutionBlock
	Approvals *PolicyApprovalsBlock
	Effects   *PolicyEffectsBlock
	Hitl      *HitlBlock        // hitl { … } human-in-the-loop review config (issue #106, #440)
	Tools     *PolicyToolsBlock // tools { forbidUnknownTools … } (issue #440)
}

// PolicyToolsBlock is `tools { forbidUnknownTools <bool> }` (issue #440): when true, every tool call not
// explicitly permitted is denied (the strict-preset closed world). Field nil when omitted.
type PolicyToolsBlock struct {
	Pos                Pos
	ForbidUnknownTools *bool
}

// HitlBlock is the parsed `hitl { … }` policy sub-block (issues #106, #440). It lowers to
// spec.HitlPolicy. interruptOn keys are Tool metadata.name values (they configure review at a gate,
// they do not gate by themselves — see spec.HitlPolicy).
type HitlBlock struct {
	Pos               Pos
	DescriptionPrefix *StringLit
	RedactKeys        []*StringLit
	ToolSwitchMap     []*SwitchMapEntry // source operation -> allowed switch targets
	InterruptOn       []*InterruptEntry
}

// SwitchMapEntry maps a source operation to its allowed switch-decision targets. Source and each
// target are dotted operation names joined into a single Ident (like tool operation names).
type SwitchMapEntry struct {
	Pos     Pos
	Source  *Ident
	Targets []*Ident
}

// InterruptEntry is one `interruptOn` tool entry: a bare `<tool>` means enabled with defaults
// (YAML `true`); `<tool> { … }` supplies per-tool review config.
type InterruptEntry struct {
	Pos    Pos
	Name   *Ident
	Config *InterruptConfig // nil = enabled with defaults
}

// InterruptConfig is per-tool review configuration inside an interruptOn entry. It lowers to
// spec.HitlInterruptConfig; each field is nil/empty when omitted.
type InterruptConfig struct {
	Pos              Pos
	AllowedDecisions []*Ident // approve | reject | edit | switch
	Description      *StringLit
	AllowedEditArgs  []*StringLit
	DeniedEditArgs   []*StringLit
	AllowedEditPaths []*StringLit
	DeniedEditPaths  []*StringLit
	AllowedEditTools []*StringLit
	SwitchMap        []*SwitchMapEntry
	RedactKeys       []*StringLit
}

func (d *PolicyDecl) Position() Pos { return d.Pos }
func (d *PolicyDecl) declNode()     {}

// PolicyExecutionBlock is `execution { maxTotalCostUsd … maxWallClockSeconds … maxIterations … requireStructuredOutput … }`.
type PolicyExecutionBlock struct {
	Pos                     Pos
	MaxTotalCostUsd         *float64
	MaxWallClockSeconds     *int
	MaxIterations           *int
	RequireStructuredOutput *bool
}

// PolicyApprovalsBlock is `approvals { requiredFor { … } requireAllTools … permissive … }`.
type PolicyApprovalsBlock struct {
	Pos             Pos
	RequiredFor     []*Grant // tool.<name>.<operation> uses strings
	RequireAllTools *bool
	Permissive      *bool
}

// PolicyEffectsBlock is `effects { permit { … } permitWithApproval { … } }`. Both are first-class:
// permitted autonomously versus permitted only behind approval (ADR 005 §4).
type PolicyEffectsBlock struct {
	Pos                Pos
	Permit             []*EffectRef
	PermitWithApproval []*EffectRef
}

// ProviderDecl is a top-level `provider <alias> { type … apiKeyFrom … workspaceIdFrom … }`
// declaration (issue #440): a custom/aliased model provider that lowers into
// spec.ProjectSpec.Providers.Models[alias]. Built-in namespaces (anthropic, openai, …) resolve
// implicitly and need no declaration; `provider` is for aliases, custom endpoints, and credentials.
type ProviderDecl struct {
	Pos             Pos
	Name            *Ident     // the alias, e.g. corporate-claude
	Type            *Ident     // underlying provider type (anthropic, openai, mock, …); required
	APIKeyFrom      *StringLit // env:VAR reference for the API key (optional)
	WorkspaceIDFrom *StringLit // env:VAR reference for a workspace id (optional)
}

func (d *ProviderDecl) Position() Pos { return d.Pos }
func (d *ProviderDecl) declNode()     {}

// DefaultsDecl is the top-level singleton `defaults { policy … model … runtime … }` declaration
// (issue #440, ADR 007): project-wide fallbacks that lower into spec.ProjectSpec.Defaults. It is the
// reviewable `.agent` home for what YAML expressed as `spec.defaults`. A project may declare it at
// most once; a second block, or a collision with a YAML `spec.defaults`, is a lowering error.
type DefaultsDecl struct {
	Pos     Pos
	Policy  *Ident    // default policy name applied to workflows/agents that declare none (optional)
	Model   *ModelRef // default model (<provider>/<name>) for agents that declare none (optional)
	Runtime *Ident    // default runtime name (optional)
}

func (d *DefaultsDecl) Position() Pos { return d.Pos }
func (d *DefaultsDecl) declNode()     {}

// LimitsDecl is the top-level singleton `limits { … }` declaration (issue #440, ADR 007): the
// project-wide execution-limit baseline that lowers into spec.ProjectSpec.Limits. Its body reuses the
// same nine-field ToolLimitsBlock the per-tool `limits { … }` override uses; the difference is only
// where it lowers (project baseline vs per-tool top-precedence override in ResolveExecutionLimits). A
// project may declare it at most once.
type LimitsDecl struct {
	Pos   Pos
	Block *ToolLimitsBlock
}

func (d *LimitsDecl) Position() Pos { return d.Pos }
func (d *LimitsDecl) declNode()     {}

// EnvironmentDecl is a top-level `environment <Name> { overrides { … } }` declaration (issue #440):
// per-environment agent/policy overrides that lower to spec.EnvironmentResource, applied by
// spec.ApplyEnvironment exactly like the YAML Environment resource.
type EnvironmentDecl struct {
	Pos       Pos
	Name      *Ident
	Overrides *EnvOverridesBlock
}

func (d *EnvironmentDecl) Position() Pos { return d.Pos }
func (d *EnvironmentDecl) declNode()     {}

// EnvOverridesBlock is `overrides { agents { … } policies { … } }`.
type EnvOverridesBlock struct {
	Pos      Pos
	Agents   []*AgentOverrideEntry
	Policies []*PolicyOverrideEntry
}

// AgentOverrideEntry is one `<agentName> { model … constraints { … } }` entry under `agents { … }`.
type AgentOverrideEntry struct {
	Pos         Pos
	Name        *Ident
	Model       *ModelRef
	Constraints *Constraints
}

// PolicyOverrideEntry is one `<policyName> { execution { … } approvals { … } }` entry under
// `policies { … }`.
type PolicyOverrideEntry struct {
	Pos       Pos
	Name      *Ident
	Execution *PolicyExecutionBlock
	Approvals *PolicyApprovalsBlock
}
