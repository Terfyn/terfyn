package runtime

import (
	"context"
	"time"

	"github.com/Terfyn/terfyn/internal/config"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/trace"
)

// HealthState reports runtime readiness.
type HealthState string

const (
	// HealthOK means the runtime is ready to accept work.
	HealthOK HealthState = "ok"
	// HealthDegraded means the runtime can execute but with reduced capability.
	HealthDegraded HealthState = "degraded"
	// HealthError means the runtime cannot execute workflows.
	HealthError HealthState = "error"
)

// HealthStatus is returned by [Runtime.Health].
type HealthStatus struct {
	State   HealthState
	Details string
}

// RunResult is the outcome of [Runtime.Invoke] or [Runtime.Resume].
type RunResult struct {
	RunID string
	// Warnings are advisory messages from run setup (e.g. literal secrets in snapshot-persisted
	// fields, issue #207). Not fatal; surfaced to the operator.
	Warnings []string
}

// Deps are control-plane supplied dependencies shared by all runtime implementations.
type Deps struct {
	Store        state.RuntimeStore
	AgentVersion string
	Now          func() time.Time
}

// InvokeOptions configures a new workflow execution.
type InvokeOptions struct {
	// RunID optional; when empty the runtime generates one.
	RunID string
	// WorkflowName is the metadata.name of a Workflow resource.
	WorkflowName string
	// Env is stored on the run row (e.g. deployment target label).
	Env string
	// EnvironmentName is the CLI -e overlay name; empty skips Environment resource overrides.
	EnvironmentName string
	// InputJSON is JSON object bytes for workflow input. Empty means {}.
	InputJSON []byte
	// ApprovedActions are full tool uses strings approved for policy gates.
	ApprovedActions []string
	// AutoApprove skips interactive HITL prompts and approves gated tool calls.
	AutoApprove bool
	// HitlActor attributes approval decisions in trace events.
	HitlActor string
	// Attribution scopes the run for multi-tenant logs and compliance.
	TenantID       string
	ThreadID       string
	ActorID        string
	ParentRunID    string
	RequestID      string
	IdempotencyKey string
	Source         string
	// RequireAttribution rejects runs when tenant_id, thread_id, or actor_id is omitted.
	RequireAttribution bool
	// EventSink, when set, streams each trace event live as it is appended (issue #450, terfyn run
	// --verbose). Nil is today's behavior (events only persisted). It never changes what is stored.
	EventSink trace.EventSink
	// TraceDetail enriches the streamed/stored trace with each turn's substance — reasoning text,
	// tool arguments, bounded tool output (issue #525, terfyn run --trace-detail). Opt-in; the added
	// fields still pass through redaction+truncation. It DOES change what is stored (by design).
	TraceDetail bool
}

// ResumeOptions continues an existing run from its latest checkpoint.
type ResumeOptions struct {
	// RunID is required.
	RunID string
	// EnvironmentName is the CLI -e value; must match the persisted run when pinned.
	EnvironmentName string
	// ApprovedActions are full tool uses strings approved for policy gates.
	ApprovedActions []string
	// AutoApprove skips interactive HITL prompts and approves gated tool calls.
	AutoApprove bool
	// HitlActor attributes approval decisions in trace events.
	HitlActor string
	// HitlDecision supplies an explicit decision when resuming an interrupted run.
	HitlDecision *HitlDecisionOptions
	// Attribution fields on resume are ignored; persisted run attribution is reused.
	TenantID string
	ThreadID string
	ActorID  string
	// EventSink streams each trace event live as it is appended (issue #450). Nil = persist only.
	EventSink trace.EventSink
	// TraceDetail enriches the streamed/stored trace with each turn's substance (issue #525).
	TraceDetail bool
}

// HitlDecisionOptions configures a non-interactive HITL resolution on resume.
type HitlDecisionOptions struct {
	Kind         spec.HitlDecisionKind
	EditedWith   map[string]any
	SwitchTarget string
}

// Runtime executes workflows from a resolved configuration snapshot supplied by the control plane.
// Implementations must not reload project YAML/TOML; they receive [config.ResolvedConfig] only.
type Runtime interface {
	Invoke(ctx context.Context, cfg *config.ResolvedConfig, opts InvokeOptions) (RunResult, error)
	Resume(ctx context.Context, cfg *config.ResolvedConfig, opts ResumeOptions) (RunResult, error)
	Health(ctx context.Context) HealthStatus
}
