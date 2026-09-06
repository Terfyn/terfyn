package agentcli

import (
	"context"
	"time"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
)

// Budget / iteration / timeout mapping (issue #340). Terfyn's constraints and budget are mapped
// onto the external harness's own knobs, but the harness's accounting is only a belt: Terfyn stays
// the enforcer of record. A run that would exceed the Terfyn budget fails closed via CheckRun even
// if the harness disagreed (see EnforceBudget), so the authority bound is identical across runtimes.

// Limits are the Terfyn-derived bounds for one external run, mapped to the harness where a knob
// exists and enforced by Terfyn regardless.
type Limits struct {
	// MaxTurns bounds Generate turns → --max-turns, resolved from constraints.maxIterations clamped to
	// the policy's execution.maxIterations ceiling (or the default 32 when unset), via the shared
	// spec.ResolveMaxIterations — so the turn bound is identical to the internal runtime (issue #522).
	MaxTurns int
	// Timeout is the process/context deadline → derived from constraints.timeoutSeconds (0 = none).
	Timeout time.Duration
	// BudgetUSD is the harness cost ceiling → --max-budget-usd, mirrored from
	// execution.maxTotalCostUsd (0 = none). A belt only; CheckRun is authoritative.
	BudgetUSD float64
}

// MapLimits derives the external-run bounds from an agent's constraints and the merged policy's
// execution budget. Either argument may be nil. MaxTurns always resolves (default when unset), so
// the external run is never unbounded in turns; Timeout and BudgetUSD are set only when declared.
func MapLimits(c *spec.AgentConstraints, exec *spec.PolicyExecution) Limits {
	l := Limits{MaxTurns: spec.ResolveMaxIterations(c, spec.PolicyMaxIterations(exec))}
	if c != nil && c.TimeoutSeconds > 0 {
		l.Timeout = time.Duration(c.TimeoutSeconds) * time.Second
	}
	if exec != nil && exec.MaxTotalCostUsd > 0 {
		l.BudgetUSD = exec.MaxTotalCostUsd
	}
	return l
}

// ApplyTo writes the harness-mappable bounds onto a RunSpec (MaxTurns, MaxBudgetUSD). Timeout is
// not a RunSpec field — the caller applies it to the context (see WithTimeout).
func (l Limits) ApplyTo(spec *RunSpec) {
	if spec == nil {
		return
	}
	spec.MaxTurns = l.MaxTurns
	spec.MaxBudgetUSD = l.BudgetUSD
}

// WithTimeout wraps ctx with the limit's process deadline. When the limit sets no timeout it
// returns ctx and a no-op cancel, so callers can always defer the returned cancel.
func (l Limits) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if l.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, l.Timeout)
}

// EnforceBudget folds an external session's reported cost into the accumulated run cost and asks
// the Terfyn policy evaluator whether the run is still within budget. It is the enforcer-of-record
// step: the returned RunContext carries the new accumulated cost, and a non-nil error is a
// fail-closed budget/wall-clock denial (policy.ReasonMaxCost / ReasonMaxWallClock, carrying the
// limit_hit trace data) — returned even if the harness's own --max-budget-usd accounting let the
// run finish. When eval is nil there is no budget to enforce and the folded context is returned.
func EnforceBudget(ctx context.Context, eval policy.PolicyEvaluator, run policy.RunContext, session Session) (policy.RunContext, error) {
	run.AccumulatedCostUSD += session.CostUSD
	if eval == nil {
		return run, nil
	}
	if err := eval.CheckRun(ctx, run); err != nil {
		return run, err
	}
	return run, nil
}
