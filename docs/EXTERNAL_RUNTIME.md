# External agent runtimes

Terfyn executes a reviewed `.agent` program on a **runtime target**. The built-in target is the
local engine (`internal/runtime/local`); an **external** target drives a third-party CLI agent —
`claude -p` (Claude Code) is the first — as the execution substrate while Terfyn keeps authority,
budget, HITL, snapshot/resume, and audit around it. This is the external agent-runtime epic
([#335](https://github.com/Terfyn/terfyn/issues/335)).

The governing principle: **the runtime is replaceable; the authority is not.** The same program,
run under `--runtime local` or `--runtime claude-code`, exercises the same bounded authority. A
newcomer can see this end to end in [`examples/external-runtime-reviewer`](../examples/external-runtime-reviewer).

## The AgentRuntime boundary

An external target is a boundary adapter (`internal/runtime/claudecode`) implementing a small
interface distinct from Terfyn's own engine:

- **`AgentRuntime.RunSession`** spawns the CLI non-interactively, streams its structured output
  (`--output-format stream-json`), and collects a `Session` (turns, tool selections, cost, stop
  reason). It performs no tool routing or trace writing itself.
- Around it sit the pieces that keep Terfyn the enforcer of record:
  - **Grant → per-run MCP server** (`internal/runtime/mcpserver`, [#338](https://github.com/Terfyn/terfyn/issues/338)):
    an agent's `grants { tool.<name>.<op> }` compile — against the deployed, pinned capability
    manifest ([#204](https://github.com/Terfyn/terfyn/issues/204)) — into exactly the MCP operations
    that agent may call. Every `tools/call` routes through the **same** inner path as the internal
    loop: policy `CheckToolCall` → HITL → `Tools.Call`.
  - **Budget / iteration / timeout mapping** ([#340](https://github.com/Terfyn/terfyn/issues/340)):
    `constraints.maxIterations → --max-turns` (shared default-8, clamped to the policy's `execution.maxIterations` ceiling or 32, #522), `timeoutSeconds → `
    process deadline, `execution.maxTotalCostUsd → --max-budget-usd`. The harness knobs are a belt;
    Terfyn's `CheckRun` stays authoritative and a breach fails closed with `limit_hit`.
  - **Trace / audit integration** ([#341](https://github.com/Terfyn/terfyn/issues/341)): external
    turns and tool calls fold into the hash-linked `trace_events` chain, so `terfyn logs` and
    `terfyn audit verify` cover an external run identically to a local one.
  - **Plan** ([#342](https://github.com/Terfyn/terfyn/issues/342)): `terfyn plan` shows the selected
    runtime target and proves the effect bound is byte-identical across runtimes.

## The grant-is-not-a-builtin rule

The one decision that makes the whole boundary sound: **a capability grant compiles to a
Terfyn-owned MCP operation, never to a built-in tool of the external CLI.**

Mapping a scoped grant onto a built-in — e.g. `tool.workspace.run_tests → Bash` — would be a
capability escape: `terfyn plan` would advertise `effect bound: workspace.test` while the real
authority is `Bash`'s unbounded filesystem / network / process / git. So:

- The external agent is spawned with **no built-in tools**; the only tools it sees are the granted
  operations served by the per-run MCP server, passed with `--strict-mcp-config`.
- The adapter never emits a built-in-tool allowance, and it **fails closed** before spawning if a
  caller-supplied flag would expose one or bypass the permission boundary
  (`checkNoBuiltinToolExposure`, `checkExtraArgsNoAuthoritySurface`).
- Arbitrary shell is not forbidden — it is made **loud**: it requires an explicit
  `grants { tool.shell.exec }` that compiles to a Terfyn-owned MCP op, so `terfyn plan` shows the
  true authority surface. Broad capability is allowed; *hidden* broad capability is the non-goal
  ([ADR 004 §5](adr/004-scope-and-non-goals.md)).

This is recorded as a permanent obligation — invariant **S9** in
[`docs/SOUNDNESS.md`](SOUNDNESS.md): *an external runtime's callable set is exactly the pinned
grants; no built-in tools.* S9 also carries the live-verification obligation: before the runtime is
wired to a live run, an integration check against the pinned CLI version must confirm that the
emitted flags actually deny built-in tools (a fake-process unit test cannot close that gap).

### Running the S9 live check

`TestS9Live_builtinsAreDeniedByPinnedCLI` (`internal/runtime/claudecode/s9_live_test.go`) is that
exhibit. It spawns the **real** `claude` with the adapter's real argv, pointed at a per-run server
granting one benign op, and asserts (1) the CLI advertises only the granted `mcp__*` tool at init —
no built-in — and (2) an agent instructed to write a sentinel file via a built-in cannot: the file
never appears. It is fenced out of normal CI (it needs the binary, credentials, and network) behind
the `s9live` build tag plus an env gate:

```
TERFYN_S9_LIVE=1 \
TERFYN_S9_CLAUDE_VERSION="$(claude --version)" \
  go test -tags s9live -run TestS9Live -v ./internal/runtime/claudecode/
```

Point `TERFYN_CLAUDE_BIN` at a specific binary to pin the version under test; the resolved version is
logged so a green run names exactly what it verified. **If this test cannot pass against the pinned
CLI, the external runtime must not be used for a live run** — the `--tools ""` denial in
`denyBuiltinToolsArgs` is a contract against a CLI this repo does not own, and this check is what
turns it from documentation into enforcement.

## Selecting a runtime

`terfyn run` picks the target from the workflow's `spec.runtime`, else the project
`defaults.runtime`, else the built-in `local` engine; `--runtime <name>` overrides it for one run.
Names are registered in the runtime registry (`internal/runtime`); `claude-code` registers alongside
`local`. `terfyn plan`'s **Runtime targets** section shows the resolved target per workflow, and a
change to it surfaces as a `runtime_target_change` risk item — an execution-substrate change, never
an authority widening.

See [ADR 006](adr/006-external-agent-runtimes.md) for the decision to promote `RuntimeTarget` from
"later" to a real resource, [`RUNTIME_TARGET_CONTRACT.md`](RUNTIME_TARGET_CONTRACT.md) for the
explicit bar a new external runtime (Codex, Gemini CLI) must meet, and
[`docs/architecture.md`](architecture.md) for where the runtime boundary sits in the control plane.
