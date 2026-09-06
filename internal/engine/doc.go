// Package engine orchestrates workflow execution, steps, and interpolation.
//
// [InterpolateString] and [InterpolateWalk] implement ${input.*} and ${steps.*} dot paths only (design doc section 13.1 MVP).
// Whole-field tokens keep native types; tokens embedded in surrounding text stringify (issue #193).
//
// [Executor.Run] executes every workflow on the execir interpreter: issue #278 retired the
// WorkflowStep DAG runtime, converging both ingress paths (YAML and `.agent`) on one interpreter
// (ADR 002 §5). A run interpolates step inputs, applies policy checks from the workflow's Policy
// resource, runs tool and agent steps, optionally validates agent output against JSON Schema, and
// persists run_steps rows and trace events (design doc sections 12.2, 13.3, 13.4, 14.2). A
// straight-line workflow lowers to a flat node list (sequential YAML order); independent `needs:`
// roots lower to a Graph scheduled with bounded concurrency ([RunInput].MaxConcurrentSteps); join
// steps see all upstream `${steps.*}` outputs; `.agent` control flow lowers to Branch/Loop/Fork
// (issue #259). The interpreter memoizes each completed leaf by its CallSite, so a HITL/approval
// suspend checkpoints and resumes without re-issuing a side effect (issue #258). A `workflow:` step
// runs the callee as a nested execir run (issue #194/#270); its `output.value` becomes the step
// output, with nested progress carried on the checkpoint so resume continues mid-subworkflow. Trace
// events stamp nested `callStack` for subworkflow events; the audit chain verifies insert order. A
// resume requires an execir checkpoint — a pre-execir (DAG) checkpoint is not resumable (#278).
//
// Agent steps with declared tools run a bounded Generate loop (issue #160): the engine attaches
// one [models.ToolDef] per listed Tool (`ToolChoice: auto`). `spec.tools` may name a Tool or pin
// `tool.<name>.<operation>`. Native names advertise `echo`; mock/mcp advertise `default`; HTTP
// requires a pinned `method.path` (including rejecting `tool.<name>.default`). [spec.ValidateProjectGraph]
// applies the same advertised-uses rules. Only that ToolDef name is accepted — aliased ops such as
// `helper.echo` or `helper.command.run` fail before [policy.PolicyEvaluator.CheckToolCall] /
// [tools.ToolExecutor.Call]. Inner calls use the agent `timeoutSeconds` context. After each Generate
// and tool turn, [policy.PolicyEvaluator.CheckRun] re-checks cost and wall-clock budgets
// against already-accumulated run cost before the next Generate or inner tool call, and again
// after each call's actual cost. Exceeding `execution.maxTotalCostUsd` records `limit_hit`
// (`kind: max_cost`) plus `system_error`, then fails the step with `run_error` (issue #163).
// `constraints.maxIterations` (default 8, clamped to the governing policy's `execution.maxIterations`
// ceiling, or 32 when the policy sets none — issue #522) counts Generate turns; `tool_use` on the last
// turn fails without executing those calls. HITL interrupt does not run inside the loop: inner uses
// must be pre-approved (`--approve` / ApprovedActions) or CheckToolCall fails closed. Agents with no
// tools remain a single completion. Inner tool calls share workflow-step tracing: `tool_selection`
// (tool name + arguments digest, not raw args) then `tool_execution` (duration, cost, success,
// and a stable `tool_call_failed` reason — not the raw Error() string) after the tool returns,
// including Call failures (issue #161). Policy denial still records
// `system_error` without invoking the tool.
package engine
