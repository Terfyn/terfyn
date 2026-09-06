# Agent tool-calling loop (issues #160 / #161)

When an Agent declares `spec.tools`, `terfyn run` does **not** fire a single completion. The engine runs a bounded **reason → act → observe** loop: the model may call advertised tools, observe results, and continue until `end_turn` or `constraints.maxIterations`.

This is ADR 002 **Path 1**: agents genuinely select tools. Epic A **shipped** that form ([#160](https://github.com/Terfyn/terfyn/issues/160) / [#161](https://github.com/Terfyn/terfyn/issues/161)). If it had shipped reduced (workflow-only `uses:` steps), the grant semantics below would not apply; they do apply.

Implementation: [`internal/engine`](../internal/engine) (`runAgentToolLoop`, `advertisedAgentTools`). Design: [`DESIGN_DOC.md`](DESIGN_DOC.md) §12.2 F / G.

## Loop

```text
reason  →  Generate (ToolChoice: auto, advertised ToolDefs)
act     →  on tool_use: resolve name → one uses string → CheckToolCall → Tools.Call
observe →  append assistant ToolCalls + user ToolResults; next Generate
```

Stop when `StopReason` is `end_turn` (or empty with no tool calls). Other stop reasons fail the step. Agents with **no** `spec.tools` stay a single Generate with no `Tools` field.

## Grants (ADR 002)

Because the agent selects its own tools, `agent.spec.tools` is an **autonomous capability grant**, not a call list. Every granted tool contributes to the agent's effect bound ([#189](https://github.com/Terfyn/terfyn/issues/189)) whether or not any authored workflow step names it.

A grant is a **concrete operation** (`tool.<name>.<operation>`), not a Tool resource and not an effect class. `agent.spec.tools` entries are grants in this sense (Tool metadata name or a pinned uses string).

An agent's action space is the union of its granted operations' declared effects. Widening the grant list expands the action space of a nondeterministic component — `terfyn plan` reports a new **autonomous** effect at higher severity than a new **static** one, and prints `AUTONOMOUS` `WIDENED` when a grant is added even if the named effect set does not change.

Issue #189 computes the effect bound in [`internal/effects`](../internal/effects) over the desired graph (static `uses:` plus these grants). Issue #190 enforces that bound against `Policy.spec.effects` at validate/plan (exit **2**). Issue #191 prints the bound table and the authority delta vs stored deployment state in `terfyn plan` (`-o table|json|yaml`). Terfyn does not verify what remote systems do with a granted operation.

### Closed world (#204)

For MCP tools the grant is only meaningful against a **pinned operation manifest**. [#204](https://github.com/Terfyn/terfyn/issues/204) ships the dispatch-time closed world: an operation absent from the tool's deployed capability manifest (derived from declared `spec.operations`, never from a live `tools/list`) is denied on the policy path (`ReasonOperationNotInManifest`, exit **5**, trace event), so a remote `tools/list` advertising an extra operation **cannot** expand the callable set. Closed-world enforcement is **opt-in per tool**: a tool that declares no `operations` key keeps an open callable set (see [`docs/SOUNDNESS.md`](SOUNDNESS.md) S2). A resumed run enforces the manifest it **started** with: [#207](https://github.com/Terfyn/terfyn/issues/207) hydrates the run's graph and authority from its pinned deployment snapshot (`runs.deployment_snapshot_digest`), so a widening apply between suspend and resume does not widen the resumed run's callable set. (Runs created before #207 — an empty digest — or backends without artifact retention fall back to current config.)

## How tools are advertised

Each listed Tool becomes one `ToolDef` whose **name is the Tool metadata name**. The model may call only that name. It maps to a **single** advertised uses string:

| `spec.tools` entry | Advertised `uses` |
|---|---|
| Native Tool name | `tool.<name>.echo` |
| Mock / MCP Tool name | `tool.<name>.default` |
| HTTP Tool name | rejected (no default; `default` would become `GET /default`) |
| Pinned `tool.<name>.<operation>` | that exact uses string (HTTP must be a real `method.path`) |

`terfyn validate` / `plan` apply the same rules. Aliases such as `helper.echo` or `helper.command.run` fail **before** `CheckToolCall` / `Tools.Call`. Inner calls use the agent `constraints.timeoutSeconds` context. ToolDef parameters are a permissive object schema.

## `maxIterations` / `ToolChoice`

| Knob | Implemented behavior |
|---|---|
| `ToolChoice` | The loop sends `auto`. The final graceful-finalization turn (below) sends no tools at all. |
| `constraints.maxIterations` | Counts **Generate** turns. Default **8** (unset/zero); the agent still **declares** the turns it wants. The value is clamped to a **ceiling** — the governing policy's `execution.maxIterations` (issue #522), or **32** when the policy sets none. The policy sets the *max the agent may request*, not a grant: a policy of 64 does nothing on its own; the agent must also set `constraints.maxIterations 64` (unset stays 8). Cost/wall-clock still bound runaways. |
| Last-turn `tool_use` | **Not executed.** Emits `limit_hit` (`kind: max_iterations`), then the engine **finalizes gracefully** (issue #518): it appends a "return your final answer, no more tools" instruction and runs **one final tool-free completion**, so the agent returns its best output from what it has gathered and the run **continues** with that value. `maxIterations: 1` is a single tool-capable turn plus this finalize turn; the capped turn's tools never run. Only if that forced final turn still produces no valid output (a `max_tokens` stop, or output that fails to parse/validate) does the step fail. |

After each Generate and each inner tool turn, `CheckRun` re-evaluates `execution.maxTotalCostUsd` / wall-clock against accumulated loop cost (prior workflow steps included). Exceeding the cost ceiling records `limit_hit` (`kind: max_cost`) plus `system_error` and fails the step (exit **5**). Loop model + tool cost accumulates into the step meta.

## Policy on every inner call

Every accepted `tool_use` goes through `CheckToolCall` **before** `Tools.Call` — the same `runToolStep` path as workflow `uses:`.

The evaluator is the **workflow** policy (`wfPol` from `wf.Spec.Policy` into `runAgentStep` / `runToolStep`), not `agent.spec.policy`. Agent policy is YAML/plan documentation; it is not the inner gate. [`examples/multi-agent`](../examples/multi-agent) gives each agent its own Policy resource while both steps run under one workflow policy.

If policy denies the call, the tool is **never invoked**. Trace records `system_error` only (no `tool_selection` / `tool_execution`). Names missing from the advertised map fail in `resolveAgentToolCall` **before** `CheckToolCall` (execution error, not `approval_required`).

## HITL vs exit 5

Do **not** mix these paths:

| Path | What happens |
|---|---|
| Inner agent-loop tool | **Does not HITL.** A **granted** (advertised) operation that `CheckToolCall` denies for missing `--approve` / `ApprovedActions` fail-closes with **exit 5** (`approval_required`). |
| Workflow `uses:` gated by `approvals.requiredFor` or safety metadata | `maybeInterruptForHitl` → run **interrupted**, **exit 0**, checkpoint. Resume with `--resume --decision …`. |

`Policy.spec.hitl.interruptOn` configures review options for calls already gated by `requiredFor` or safety metadata; it does not by itself gate inner-loop tools.

## Trace events (#161)

Inner calls share the workflow `uses:` payload shape. Actor on selection/execution is `agent`.

| Event | When |
|---|---|
| `llm_completion` | After each Generate |
| `tool_selection` | After `CheckToolCall` allows, before `Call`. Fields: `tool`, `uses`, SHA-256 `argumentsDigest` (raw args are not stored) |
| `tool_execution` | After `Call` returns (success or failure). Fields: `durationMs`, `costUsd`, `success`; on failure `error: tool_call_failed` (raw `Error()` is not persisted) |
| `system_error` | `CheckToolCall` denial or budget denial — tool never invoked |
| `limit_hit` | `kind: max_iterations` or `kind: max_cost` |

Events are hash-linked. `terfyn logs --run <id>` shows them; `terfyn audit verify --run <id>` covers them. See [`AUDIT_CHAIN.md`](AUDIT_CHAIN.md).

## Related docs

- [`examples/incident-triage`](../examples/incident-triage) — gated restart; inner-loop exit **5** without `--approve`
- [`examples/hitl-resume`](../examples/hitl-resume) — workflow `uses:` HITL, exit **0**
- [`architecture.md`](architecture.md) — pipeline and plan-time bounds
- [ADR 002](adr/002-language-frontend-and-ir-expressiveness.md) — grants vs effects; Path 1; closed world
