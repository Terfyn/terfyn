package spec

// Iteration bound for an agent's reason→act→observe loop (issue #160). Unset or zero uses the
// default; a value above the ceiling is clamped down to it. This is the single source of truth
// for the bound: the internal engine loop (internal/engine) and the external-runtime turn mapping
// (internal/runtime/claudecode → --max-turns, issue #340) both resolve through ResolveMaxIterations
// so a program's iteration ceiling is identical regardless of which runtime executes it.
const (
	DefaultAgentMaxIterations = 8
	// HardAgentMaxIterations is the default ceiling on constraints.maxIterations. It is no longer a
	// frozen global: a policy may raise (or lower) it via execution.maxIterations, so a coding agent
	// that must explore, edit, and run tests in one attempt can be granted more turns while cost and
	// wall-clock still bound genuine runaways (issue #522). This value applies when the governing
	// policy sets no ceiling, so existing programs are unchanged.
	HardAgentMaxIterations = 32
)

// ResolveMaxIterations returns the effective iteration bound for the given agent constraints, clamped
// to the governing policy's ceiling. policyCeiling is execution.maxIterations from the policy that
// governs the step (0 = unset → the default HardAgentMaxIterations). A nil constraints block (or an
// unset/zero maxIterations) resolves to DefaultAgentMaxIterations; the result never exceeds the
// ceiling — including a policy ceiling set below the default, which then bounds even the default.
func ResolveMaxIterations(c *AgentConstraints, policyCeiling int) int {
	ceiling := HardAgentMaxIterations
	if policyCeiling > 0 {
		ceiling = policyCeiling
	}
	n := DefaultAgentMaxIterations
	if c != nil && c.MaxIterations > 0 {
		n = c.MaxIterations
	}
	if n > ceiling {
		return ceiling
	}
	return n
}

// PolicyMaxIterations returns the iteration ceiling declared by a policy's execution block, or 0 when
// unset (the caller then falls back to HardAgentMaxIterations via ResolveMaxIterations).
func PolicyMaxIterations(exec *PolicyExecution) int {
	if exec == nil {
		return 0
	}
	return exec.MaxIterations
}

// DefaultAgentMaxTokens is the per-completion output-token cap when constraints.maxTokens is unset
// (issue #514). It replaces the old hardcoded 4096, which was a chat-era default that truncated any
// agent writing real content (a coding agent's whole-file write_file exceeds it). 16384 is a
// realistic agent default that still stays under non-streaming HTTP timeouts; an author raises it
// per agent with constraints.maxTokens for larger outputs. There is no hard clamp — a value the
// provider cannot honor is rejected loudly at request time rather than silently capped here.
const DefaultAgentMaxTokens = 16384

// ResolveMaxTokens returns the effective output-token cap for the given agent constraints. A nil
// constraints block (or an unset/zero maxTokens) resolves to DefaultAgentMaxTokens; any positive
// value is used verbatim.
func ResolveMaxTokens(c *AgentConstraints) int {
	if c != nil && c.MaxTokens > 0 {
		return c.MaxTokens
	}
	return DefaultAgentMaxTokens
}
