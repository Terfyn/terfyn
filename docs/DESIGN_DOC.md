# Terfyn

## Design Document v0

## 0. Summary

Terfyn is a **statically analyzable, capability-oriented execution platform for nondeterministic
programs.** Its purpose is to make the **authority** granted to autonomous components — the set of
operations they may call and the effects those may produce — **statically bounded, reviewable before
execution (as a `plan` diff), and invariant across the execution lifecycle** (a suspended run cannot
wake up under widened authority). Terfyn bounds and diffs that grant; it does **not** verify what
remote systems (GitHub, a shell, an MCP server) actually do when invoked — see ADR 002, *Soundness
assumptions and limits*, and [`docs/SOUNDNESS.md`](SOUNDNESS.md).

Operationally it is a **declarative control plane**: teams define agents, tools, workflows, policies,
and environments as versioned config (`.agent` is the authoring surface, YAML the compilation
output/interchange — [ADR 003](adr/003-yaml-as-compilation-output.md)) and drive it Terraform-style.
The scope boundary — what belongs inside this platform and what is an adapter, not an owned system —
is [ADR 004](adr/004-scope-and-non-goals.md).

The goal is not to be another Python agent framework.

The goal is to let teams define agents, tools, workflows, policies, and environments as **versioned config**, then:

* validate
* diff
* plan
* apply
* run
* trace
* govern

them like real systems.

The closest mental model is:

* **Terraform** for desired state and plan/apply
* **Kubernetes** for declarative resources and reconciliation
* **GitOps** for versioned, reviewable changes
* **OpenAPI** for explicit contracts

This document defines:

1. Go project structure
2. YAML spec v0
3. CLI UX and commands
4. internal engine architecture
5. MVP vs end goal

---

# 1. Problem Statement

Today, most agent systems are built as:

* Python/JS code
* prompts embedded in source
* tool bindings hidden in runtime code
* weak contracts
* unclear permissions
* poor change review
* little or no drift detection
* little or no governance

This creates:

* prompt spaghetti
* hidden behavior changes
* weak reproducibility
* hard reviews
* weak deployment discipline
* poor portability across runtimes/providers

We want a system where teams can say:

> “This is the desired shape of my agent system.”

And the platform can answer:

* Is it valid?
* What changed?
* What risk changed?
* What will be applied?
* What is deployed?
* What drifted?
* What happened during execution?

---

# 2. Non-Goals

This project is **not**:

* a foundation model runtime
* a model serving system
* a training platform
* an attempt to standardize chain-of-thought
* a replacement for every orchestration framework
* a magic auto-agent builder
* a general-purpose programming language for agents — see
  [ADR 002](adr/002-language-frontend-and-ir-expressiveness.md) for the bounded exception
  (conditionals and loops arrive as a `.agent` frontend compiling to this resource model, never
  as expression fields in YAML)

This project does **not** try to define:

* exact internal reasoning behavior
* latent planner internals
* hidden model state
* model training/inference kernels

It defines the **control plane** around agent systems.

---

# 3. Core Principles

## 3.1 Declarative first

Users define desired state in YAML.

## 3.2 Contracts over vibes

Inputs, outputs, tools, permissions, and policies should be explicit.

## 3.3 Separate control-plane state from runtime state

Deployment state and execution traces are different things.

## 3.4 Portable by default

Specs should not be hard-coupled to one runtime.

## 3.5 Reviewable changes

Behavior, cost, permissions, and policy changes should be diffable.

## 3.6 Safe by default

Tool permissions, approvals, budgets, and policy limits must be first-class.

## 3.7 Start small, grow cleanly

MVP should be local and simple. End state can add remote runtimes and reconciliation.

---

# 4. Conceptual Model

The system manages these resource types:

* **Project**
* **Agent**
* **Tool**
* **Workflow**
* **Policy**
* **Environment**
* **ModelProvider**
* **MemoryStore** later
* **RuntimeTarget** — a named execution adapter (issue #336). A runtime is a name registered in the
  `internal/runtime` registry (`local` is the built-in disk-backed engine); `terfyn run --runtime <name>`
  overrides the workflow's `spec.runtime` to select one. External agent-runtime adapters (e.g.
  `claude-code`, epic #335) register alongside `local`. A first-class RuntimeTarget *resource* with
  per-target config surfaces in `plan` later (#342).
* **Module** later

Each resource has:

* `apiVersion`
* `kind`
* `metadata`
* `spec`

This is intentionally Kubernetes-like because agent systems are graph-shaped and nested.

---

# 5. Go Project Structure

## 5.1 High-level layout

```text
terfyn/
  cmd/
    terfyn/
      main.go

  internal/
    app/
      app.go
      wiring.go

    cli/
      root.go
      validate.go
      plan.go
      apply.go
      diff.go
      run.go
      logs.go
      inspect.go
      test.go

    spec/
      types.go
      kinds.go
      loader.go
      parser.go
      normalize.go
      defaults.go
      validator.go
      refs.go
      errors.go

    schema/
      jsonschema.go
      registry.go
      validate.go

    project/
      loader.go
      resolver.go
      graph.go

    plan/
      planner.go
      diff.go
      risk.go
      cost.go
      output.go

    state/
      store.go
      models.go
      sqlite/
        store.go
        migrations.go
      memory/
        store.go

    apply/
      applier.go
      executor.go
      checkpoint.go

    runtime/
      runtime.go
      local/
        runtime.go
        runner.go
      interfaces/
        tool_runtime.go
        agent_runtime.go
        workflow_runtime.go

    engine/
      workflow.go
      steps.go
      interpolation.go
      execution.go
      approvals.go
      retries.go
      timeout.go

    tools/
      registry.go
      mcp/
        client.go
        transport_stdio.go
        transport_http.go
      http/
        client.go
      native/
        registry.go

    models/
      registry.go
      openai/
        client.go
      anthropic/
        client.go
      local/
        client.go

    policy/
      engine.go
      evaluator.go
      approvals.go
      budget.go
      permissions.go

    effects/
      types.go
      compute.go

    trace/
      recorder.go
      events.go
      reader.go

    logs/
      printer.go
      formatter.go

    testkit/
      runner.go
      fixtures.go
      assertions.go

    module/
      resolver.go
      lockfile.go

    render/
      yaml.go
      json.go
      table.go

    util/
      fs.go
      ids.go
      clock.go
      errors.go
      slices.go

  api/
    proto/
      controlplane.proto
      execution.proto

  pkg/
    sdk/
      types.go

  examples/
    minimal/
    pr-review/
    incident-triage/

  docs/
    spec-v0.md
    architecture.md

  scripts/
    generate.sh
    lint.sh
    test.sh

  migrations/
    sqlite/
    postgres/

  go.mod
  go.sum
  Makefile
```

---

## 5.2 Package responsibilities

### `cmd/terfyn`

Binary entrypoint.

### `internal/cli`

Command definitions, flag parsing, output formatting.

### `internal/spec`

Parsing YAML resources, type definitions, defaults, normalization, reference resolution.

### `internal/schema`

JSON Schema loading and validation for structured inputs/outputs.

### `internal/project`

Loads a project directory, merges resources, resolves imports.

### `internal/plan`

Computes desired vs current state diff, plus risk/cost delta.

### `internal/state`

Stores deployment state and runtime metadata.
MVP: SQLite.
Later: Postgres backend.

### `internal/apply`

Takes a plan and mutates runtime/control-plane state.

### `internal/runtime`

Runtime abstraction.
MVP: local runtime only.
Later: remote runtimes.

### `internal/engine`

Workflow execution engine, step orchestration, retries, interpolation.

### `internal/tools`

Tool abstraction and integrations.
MVP: native mock tools + MCP stdio.
Later: HTTP, gRPC, plugins.

### `internal/models`

Model abstraction and providers.

### `internal/policy`

Permission checks, budget checks, approval rules, safety gates.

### `internal/trace`

Structured execution events and trace persistence.

### `internal/audit`

Tamper-evident hash chain for `trace_events` (issue #116): canonical serialization, append-time hashing, and run-level verification.

### `internal/testkit`

Fixture-driven workflow tests.

### `internal/module`

Later feature for reusable modules and lockfiles.

### `api/proto`

Optional internal control-plane API for remote mode later.

---

# 6. Domain Model

---

## 6.1 Resource envelope

Every YAML file uses:

```yaml
apiVersion: agentic.dev/v0
kind: <Kind>

metadata:
  name: <resource-name>
  labels: {}
  annotations: {}

spec: {}
```

Rules:

* `apiVersion` required
* `kind` required
* `metadata.name` required, DNS-like identifier
* `labels` optional
* `annotations` optional
* `spec` required

---

## 6.2 Supported kinds in v0

MVP:

* `Project`
* `Agent`
* `Tool`
* `Workflow`
* `Policy`
* `Environment`

End goal later:

* `Module`
* `MemoryStore`
* `RuntimeTarget`
* `ApprovalPolicy` as separate kind
* `SecretRef`
* `Schedule`

---

# 7. YAML Spec v0

> **Authoring surface (ADR 002 / ADR 003).** Agents and workflows are authored in
> [`.agent`](LANGUAGE.md). `.agent` files anywhere under the project root are discovered and
> compiled through the checker (type/effect checking plus the workflow-argument rebind) into the
> resource graph by the loader (a `project.yaml` is refused, ADR 007). `.agent` workflows execute
> end-to-end, **including conditionals, loops, and dynamic fan-out (#199/#259)**: a control-flow
> workflow lowers to the execution IR, is pinned into the deployment snapshot (#260), and runs on
> the `execir` interpreter (the taken arm only) rather than the resource DAG, whose flattened arms
> are kept only for effect analysis. **YAML is a one-way output serialization**, not a source: under
> [ADR 007](adr/007-remove-yaml-ingestion.md) `.agent` is the **only executable source** — a
> `project.yaml` handed to `validate`/`plan`/`apply`/`run` is refused with a `terfyn migrate --to-agent`
> hint. Machine producers build the graph through the **typed ResourceGraph ingress** (the same
> normalization/validation/effect pipeline as `.agent`), not a YAML frontend. `terfyn export --format yaml`
> materializes the compiled graph on demand for inspection/handoff, and nothing generated is written to
> disk by default. Tools, policies, environments, custom providers, and workflows all have first-class
> `.agent` surfaces (issues #440/#478/#479), so a project is authored entirely in `.agent`; the `Project`
> config below is derived (project name from the directory, built-in providers, discovered `.agent` files)
> rather than hand-authored. The kinds and fields in this section describe the resource model both the
> `.agent` compiler and the typed ingress produce — agents, workflows, tools (including
> `mcp`/`http`/`workspace` config), policies (including `hitl`), environments, and providers — which is
> fully authorable in `.agent`.

## 7.1 Project

Defines root project settings and imports.

```yaml
apiVersion: agentic.dev/v0
kind: Project

metadata:
  name: platform-assistant

spec:
  imports:
    - ./agents
    - ./tools
    - ./workflows
    - ./policies
    - ./env

  defaults:
    runtime: local
    model: openai/gpt-4.1
    policy: default

  providers:
    models:
      openai:
        type: openai
        apiKeyFrom: env:OPENAI_API_KEY
      anthropic:
        type: anthropic
        apiKeyFrom: env:ANTHROPIC_API_KEY

  state:
    backend: sqlite
    dsn: .agentic/state.db

  traces:
    backend: sqlite
    retentionDays: 14
```

### Notes

* `imports` are relative file or directory paths
* `defaults` apply when resources omit explicit values
* `providers` configures integrations
* `state` and `traces` are local in MVP

---

## 7.2 Agent

Defines an agent contract and runtime binding.

```yaml
apiVersion: agentic.dev/v0
kind: Agent

metadata:
  name: reviewer

spec:
  description: Reviews pull requests for correctness, security, and maintainability.

  model: openai/gpt-4.1

  instructions: |
    You are a senior code reviewer.
    Prioritize correctness, security, and maintainability.
    Cite concrete evidence from tool outputs when possible.

  tools:
    - github
    - docs

  policy: default

  constraints:
    maxIterations: 8
    timeoutSeconds: 90
    temperature: 0.2
    requireStructuredOutput: true

  input:
    schema: ./schemas/review-input.json

  output:
    schema: ./schemas/review-output.json
```

### MVP fields

* `description`
* `model`
* `instructions`
* `tools` — Tool metadata names, or pinned `tool.<name>.<operation>` uses strings (one advertised operation per Tool; HTTP must be pinned)
* `policy`
* `constraints`
* `input.schema`
* `output.schema`

### End goal additions

* few-shot examples
* tool-choice policy
* retrieval sources
* external memory refs
* model fallback chains
* cost ceilings per agent
* redaction rules
* audit annotations

---

## 7.3 Tool

Defines an external capability.

### MCP stdio tool

```yaml
apiVersion: agentic.dev/v0
kind: Tool

metadata:
  name: github

spec:
  type: mcp

  mcp:
    transport: stdio
    command: npx
    args:
      - -y
      - "@modelcontextprotocol/server-github"

  permissions:
    allow:
      - pull_requests.read
      - issues.read
      - contents.read
    deny: []

  retry:
    maxAttempts: 3
    backoff: exponential
```

### MCP HTTP tool (streamable HTTP)

Remote MCP servers may expose a single JSON-RPC endpoint over HTTP(S). Set **`transport: http`**, **`url`** to that endpoint, and optional **`headers`** (including **`env:`** tokens). **`command`** / **`args`** must not be set together with **`url`** (see validator).

```yaml
spec:
  type: mcp
  mcp:
    transport: http
    url: https://mcp.example.com/v1/mcp
    headers:
      Authorization: env:MCP_TOKEN
```

### Native HTTP tool

```yaml
apiVersion: agentic.dev/v0
kind: Tool

metadata:
  name: webhook

spec:
  type: http

  http:
    baseUrl: https://api.example.com
    headers:
      Authorization: env:API_TOKEN

  permissions:
    allow:
      - request.send

  retry:
    maxAttempts: 2
    backoff: fixed
```

### Named effects on operations (issue #188)

Per-operation **effects** are classes of consequence (ADR 002), distinct from grants
(`tool.<name>.<operation>`). Operation keys are ident-shaped (`read_pr`), not HTTP `GET /users`.
Identifiers are bare dotted names matching
`[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*` — for example `github.read`, `external.visible`,
`destructive`. Identifiers beginning with `tool.` are rejected so they cannot be confused
with grants. Effects are opaque: membership and dotted-prefix matching only. The only
reserved name is **`destructive`**, which may set `spec.safety.sideEffects: true` when the
author omitted that field. Author-set `sideEffects` wins.

```yaml
spec:
  type: native
  operations:
    read_pr:
      effects: [github.read]
      schema: ./schemas/read_pr.json   # optional per-operation input schema
    post_comment:
      effects: [github.write, external.visible]
    merge_pr:
      effects: [github.write, destructive]
```

An operation may also declare an **input `schema`** (a JSON Schema ref, same convention as
`agent`/`workflow` input schemas) — the "operation → effects → schema" the capability manifest
(#204) describes. When set, a tool call's input is validated against it before dispatch; absent
means gradual (any input). The ref is part of the capability manifest and its digest (a changed ref
is manifest drift), and the schema's *content* is captured into the deployment snapshot's schema
bundle (#207) so a pinned resume enforces the schema it started with. `validate` checks the ref
resolves and compiles.

A tool with **no** declared effects is fail-closed in the **effect resolver**
(`[ResolveToolEffects]`): it carries an unknown effect that no policy permits unless the
tool opts in. That is **not** a runtime `CheckToolCall` change — existing ToolSafety + Policy
gating is unchanged until #190. `spec.operations` is additive to `spec.safety`.

Transitive bounds over these declarations (static `uses:` plus autonomous grants) are
computed by [`internal/effects`](../internal/effects) — see §12.2 J.

### Capability manifest — the closed callable world (issue #204)

`spec.operations` is also the Tool's **allowed-operation manifest**: the closed set of
operations that may become agent-callable. The bound in #189 is only a *sound* upper bound if
that set cannot grow after the bound is computed, so the manifest is authoritative — never a live
`tools/list`. Discovery ([`internal/tools/mcp_safety.go`](../internal/tools/mcp_safety.go)) may
*populate* a desired manifest during authoring, but it merges only `spec.safety`; it never adds
operations. The deployed manifest is reconstructed from the applied Tool spec.

`[tools.DeriveManifest]` builds the manifest — each operation's name, effects, and input-schema ref
(see *Named effects on operations* above). **Manifest drift** — an operation appearing,
disappearing, or changing its effects or input-schema ref — is reported by `plan` as a Tool state
change because `spec.operations` lives in the Tool's normalized spec, so it already changes the
resource spec hash that `plan`/`apply` diff; #204 coordinates with that existing pin rather than
adding a second one. `[tools.CapabilityManifest.Digest]` / `[tools.GraphManifestDigest]` are
manifest-identity primitives for direct comparison and the #207 run-pin, not a separate plan/apply
pin, and they cover each operation's input-schema ref. The schema's *content* is captured into the
deployment snapshot's schema bundle (#207), so a pinned resume validates tool input against the
schema it started with.

**Closed vs open.** Enforcement is opt-in per tool. An **omitted** `operations` key is an *open*
callable set (backward compatible — existing MCP/HTTP examples dispatch every operation). A
*declared* `operations` manifest — including an empty `operations: {}` — is a **closed** world:
only its declared operations are callable, and an empty one denies all. Closedness is a presence
bit (`ToolSpec.OperationsDeclared`), not the operation count, so shrinking a manifest to empty
cannot silently widen it to the universe. Because `Operations` is `omitempty`, an empty map would
serialize away; the bit is therefore **part of identity** (`json:"operationsDeclared"`), flowing
into the normalized spec hash, plan diffs, `NormalizedSpecJSON`, and the deployed manifest
reconstructed from applied spec (`graphFromApplied`, and the #207 snapshot). So deleting the
`operations:` key from a locked tool is a visible plan change, and the deployed world matches what
`CheckToolCall` enforces — closed-empty is not distinguishable from open only at runtime. The YAML
interchange codec preserves it too (ADR 003): `ToolSpec.MarshalYAML` emits an explicit
`operations: {}` for a declared-but-empty manifest — so a `terfyn export` and the deployment
snapshot both carry the closed world rather than dropping the empty mapping and silently reopening
the callable set. (`terfyn export` is one-way output; the round-trip is through the private YAML
codec and the applied-spec reconstruction, not a re-load of exported YAML as project source — ADR 007.)

Runtime enforcement is on the policy path
(`[policy.PolicyEvaluator.CheckToolCall]` → `ReasonOperationNotInManifest`, in **both** the
compiled snapshot evaluator that `terfyn run` uses and the legacy evaluator): an operation absent
from the deployed manifest is **denied**, traced (`system_error`), and exits **5**. The manifest is
a hard authority boundary: it binds even a nil or permissive policy, before any approval or
`DecisionAllow` short-circuit.

**Run-pinned (shipped by #207).** A run pins its deployment snapshot at start
(`runs.deployment_snapshot_digest`); `run --resume` hydrates the resolved graph from that snapshot,
and the engine takes its authority from the hydrated graph — `Executor.PinnedGraph` compiles the
policy from the pinned graph instead of reading the on-disk `.agentic/policy-snapshot.json` (which
`apply` overwrites), and skips live schema I/O under the current project root. So a resumed run
enforces the policy **and** manifest it started with — approvals, presets, and safety-derived
`CheckToolCall` decisions included — and an `apply` that lands mid-run cannot widen an in-flight
run's authority. The canonical graph payload is a semantic projection: `WorkflowStep.NeedsDeclared`
(the graph-vs-sequential signal) is part of identity and round-trips, so a resumed parallel-only
workflow keeps its concurrent roots. Referenced JSON Schemas are **captured** into the snapshot (a
`schema_bundle` artifact) at run start, so a pinned resume validates workflow input, agent output,
and tool-operation input against the schema bytes it started with — never a re-read of a changed
file; a schema uncaptured at
start (e.g. a missing file) stays gradual (allowed). Captured schemas compile in **isolation** — a
fixed opaque URL and a loader that cannot open files — so a same-document `#/$defs/...` `$ref`
resolves within the captured bytes, while an external `$ref` (`file://`, another document) is a loud
compile error, never a live disk read (which would be the drift the capture prevents). **Limits:**
schemas must be self-contained (no cross-file `$ref`). The execution IR is now pinned too:
`execution_ir_digest` is the content digest of the `execution_ir` artifact (the serialized
per-workflow `execir.Program`), non-empty for any project with a lowerable workflow (#260). See
§14 and ADR 002, *Soundness assumptions and limits*.

The scope limit still holds: the manifest bounds the callable *set* and each operation's *declared*
effects. It does not verify what a remote endpoint actually does — the trust anchor is human review
of the manifest, not runtime verification of semantics.

### MVP tool types

* `mcp`
* `http`
* `native` mock/local

### End goal tool types

* gRPC
* queue
* SQL
* filesystem
* plugin SDK

---

## 7.4 Workflow

Defines graph execution.

```yaml
apiVersion: agentic.dev/v0
kind: Workflow

metadata:
  name: pr-review

spec:
  description: Review a pull request and post a summary.

  trigger:
    type: manual

  input:
    schema: ./schemas/pr-review-input.json

  policy: default

  steps:
    - id: fetch_pr
      uses: tool.github.pull_request.get
      with:
        repo: ${input.repo}
        number: ${input.number}

    - id: review
      agent: reviewer
      with:
        pr: ${steps.fetch_pr.output}

    - id: post_comment
      uses: tool.github.pull_request.comment
      with:
        repo: ${input.repo}
        number: ${input.number}
        body: ${steps.review.output.summary}

  output:
    value:
      summary: ${steps.review.output.summary}
      findings: ${steps.review.output.findings}
```

Static fan-in (author-declared parallel branches) uses `needs:`:

```yaml
    - id: security
      agent: security-reviewer
    - id: quality
      agent: reviewer
    - id: synthesize
      agent: synthesizer
      needs: [security, quality]
      with:
        security: ${steps.security.output}
        quality: ${steps.quality.output}
```

A `workflow:` step invokes another Workflow by static name (ADR 002 graph structure — not an expression):

```yaml
    - id: compose
      workflow: fetch-and-review
      with:
        repo: ${input.repo}
        number: ${input.number}
    - id: post_comment
      uses: tool.github.pull_request.comment
      with:
        body: ${steps.compose.output.summary}
```

`with:` is the callee's input. The callee's `output.value` becomes this step's output (`${steps.compose.output…}`).

### Workflow graph rules

* If **no** step declares `needs:`, YAML order is an **implicit chain** (step *i* waits for step *i-1*). Existing sequential workflows keep that behavior.
* If **any** step declares `needs:`, the workflow is a DAG: omitted `needs` means a root (ready immediately). Independent roots run concurrently. A joining step lists every branch it waits on.
* Edges are static and author-declared (ADR 002 graph structure, not computation). No `when`, `foreach`, expressions, or dynamic fan-out on `WorkflowStep`.
* `${steps.*}` interpolation may only reference **predecessors** (transitive `needs`, or earlier YAML steps in implicit sequential mode). A join sees every upstream output.
* Independent steps run concurrently with a **bounded** worker pool (`DefaultMaxConcurrentSteps`, 8). Dependent steps do not start until every listed predecessor has completed.
* Validation rejects **cycles** and **dangling `needs` references** with YAML positions (issue #187 `Pos` / `NeedsPos`).
* Checkpoints store a **per-step completion set** (`completed` plus `steps` outputs). Resume skips completed IDs and can continue a parallel group after one branch finished.
* Trace events record wall-clock insert order (`seq`) and a stable **`logicalOrder`** (YAML step index) in `data_json` so concurrent runs remain deterministically replayable; the audit chain still hashes stored rows including `data_json`.

### Subworkflow steps (issue #194)

* Exactly one of `agent:`, `uses:`, or `workflow:` on each step. Sequential YAML without `workflow:` is unchanged.
* The callee is a statically named `Workflow` resource in the project graph. No conditionals, loops, or expression-selected callees.
* Direct and mutual recursion fail `validate` with YAML positions on the `workflow:` field (same cycle-detection style as `needs:`).
* Nesting is bounded by `spec.limits.maxWorkflowNesting` (default [`DefaultMaxWorkflowNesting`](../internal/spec/limits.go) = 8; 0/omitted uses the default). Exceeding the cap fails validate (and run) with a clear `maxWorkflowNesting` message.
* **Policy (fail-closed):** a `workflow:` step enforces **both** the caller's and the callee's `spec.policy` ([`policy.StricterOf`](../internal/policy/stricter.go)). Either evaluator may deny. Merged `PolicySpec` for HITL uses the tighter cost/time ceilings, the **union** of `hitl.interruptOn` keys and `redactKeys`, and the **intersection** of allowed decisions / edit allowances. Nested DAGs admit and commit cost against the parent's live `totalCostUsd`.
* **`hitl.interruptOn.<tool>.allowedEditTools`** lists the **operations on the gated tool** a `switch` decision may retarget to — a `switch` rewrites the gated call to `tool.<tool>.<entry>` ([`policy.switchUses`](../internal/policy/hitl_edit.go)), so entries are operation names, **not** `Tool` resource names. `validate` checks each entry against that tool's declared `spec.operations` (a tool with an open manifest — no declared operations — permits any).
* Effect bounds walk `workflow:` steps; the caller's bound includes the callee's effects and the witness path shows the nesting (caller workflow → call step → callee workflow → …).
* Traces emit `workflow_call_started` / `workflow_call_finished` and stamp `data_json.callStack` (callee names from the root) so `terfyn logs` shows the call structure. Nested `run_steps` use qualified ids `parentStep/childStep`. Step ids must not contain `/` (validate + engine) so those ids stay injective.
* Checkpoints stack in-flight callee progress (`nested`) plus the outer DAG completion set. Resume continues **mid-subworkflow** without replaying completed inner or outer steps. Each nested frame is validated against the named callee (workflow exists, parent step is `workflow:`, inner ids belong to the callee) using the resolved `maxWorkflowNesting` cap.

### Human approval steps (issue #195)

An `approval:` step is a **graph node** (ADR 002) that suspends the run for a human decision that is not about a specific tool call. Policy still decides *what requires approval* for `uses:` / agent tool calls; this form only says *where the workflow pauses*. Do not inline `approvals.requiredFor` into workflow text.

```yaml
- id: review_plan
  approval: true
  with:
    plan: ${steps.draft.output.summary}
```

`approval: true` or `approval: { description: "...", redactKeys: [secret] }` are equivalent except for review presentation. Exactly one of `agent:`, `uses:`, `workflow:`, or `approval:`.

* The step interpolates `with:`, writes a HITL checkpoint (`pendingHitl.kind: approval`, sentinel uses `workflow.approval`), and returns [ErrInterrupted]. Resume uses the existing `--decision approve|reject|edit` path (#106). `switch` is not applicable (there is no tool identity to retarget).
* **Approve** completes the step; step output is the reviewed `with:` payload.
* **Edit** applies `#106` allow-list rules (top-level keys of the presented `with:`); the edited object is the step output.
* **Reject aborts the whole workflow** (same as tool-call HITL reject). The DAG has no skip-descendants / partial-success status; a rejected gate is a failed step and fails the run. An approval step inside a **parallel group suspends only its branch** (siblings are not marked failed; the checkpoint `completed` set is reconstructed on resume, issue #192).
* `--auto-approve` skips the pause and treats the interpolated `with:` as approved output.
* The audit chain continues across suspend/resume (`hitl_request_created` → decision/resolution events on the same run).

### MVP workflow rules

* steps execute as a DAG; with no `needs:` keys, YAML order is an implicit sequential chain
* each step has exactly one of `agent`, `uses`, `workflow`, or `approval`
* `with` maps inputs (`workflow:` maps to the callee's input; callee `output.value` is the step output)
* `${...}` interpolation supported
* output can map from prior step outputs
* only manual trigger in MVP
* `needs:` is an optional static dependency list of step IDs (parallel branches / fan-in)

### End goal additions

Each bullet is ruled on in [ADR 002](adr/002-language-frontend-and-ir-expressiveness.md).
Graph structure lands in this authored resource model; computation lands in the `.agent`
frontend and must never become an expression field on `WorkflowStep`. Conditionals and loops do
lower to an internal **execution IR** (`Branch`, `Loop`, `Fork`, `Join`) that is derived rather
than authored and has no YAML surface — see ADR 002 §5.

Conditionals, loops, and dynamic fan-out are **delivered and executed** end-to-end (#199 frontend,
#255 epic): they parse, type/effect-check, lower to the execution IR
([`internal/execir`](../internal/execir)), and **run on the engine** — see
[`docs/LANGUAGE.md`](LANGUAGE.md#control-flow-and-the-execution-ir-199) and
`examples/agent-control-flow`. The effect bound remains sound as the union over all branches, and
loops are bounded by `limits.maxLoopIterations`. The engine executes `execir` at parity with the
DAG (#257), durably resumes it including HITL / concurrent per-branch suspend / nested subworkflows
(#258/#270), pins the compiled program into the deployment snapshot and executes it (#260 — the
execution-IR digest fold via `plan.WorkflowSpecHashWithExec` is now live workflow identity), and
runs `.agent` control flow through the pinned program (#259). The `WorkflowStep` DAG runtime has
been **retired (#278)**: every workflow — straight-line, `needs`, `parallel { }`, control flow, and
subworkflows — now executes on `execir`, so both ingress paths converge on one interpreter (ADR 002
§5 complete). This was a hard format cut: a run interrupted before the upgrade has a pre-execir
(DAG) checkpoint, which is not resumable — resume fails loudly and the run must be started anew.

| Addition | Surface | Status |
|----------|---------|--------|
| parallel branches | YAML / IR | delivered (#192) |
| subworkflows | YAML / IR | delivered (#194) |
| human approval steps | YAML / IR | delivered |
| scheduled triggers | YAML / IR | planned |
| event triggers | YAML / IR | planned |
| fan-out/fan-in | static fan-out is YAML / IR; dynamic fan-out over a runtime collection is a loop and belongs to the frontend | static delivered; dynamic delivered (#199) |
| conditional steps | `.agent` frontend | delivered (#199) |
| loops | `.agent` frontend | delivered (#199) |

---

## 7.5 Policy

Defines execution and governance limits.

```yaml
apiVersion: agentic.dev/v0
kind: Policy

metadata:
  name: default

spec:
  execution:
    maxWallClockSeconds: 180
    maxTotalCostUsd: 3.00
    requireStructuredOutput: true

  tools:
    forbidUnknownTools: true

  approvals:
    requiredFor:
      - tool.github.pull_request.merge
      - tool.slack.message.send

  effects:
    permit:
      - github.read
      - github.write
      - external.visible
    permitWithApproval:
      - destructive
```

### MVP

* cost ceiling
* wall clock limit
* require structured output
* forbid unknown tools
* approval-required actions
* `spec.effects.permit` / `permitWithApproval` — static bound vs Policy (issue #190)

`permit` is unattended allow for matching effect identifiers (`[spec.EffectCovers]`, so
`permit: [github]` covers `github.read`). `permitWithApproval` is a second set: the effect
is allowed only subject to approval. Do not overload `permit`. Identifiers follow the same
dotted rules as tool operation effects (#188); a `tool.` prefix is rejected.

Once any Tool declares `spec.operations` effects, a Policy with no `effects.permit` /
`permitWithApproval` block **permits nothing** (fail-closed; the error names the Policy).
Projects with no declared tool effects skip this check so existing examples still validate.
Enforcement is `internal/effects.Check` at validate/plan command paths (exit **2**), not
shared `config.Resolve`. Runtime `CheckToolCall` separately enforces the #204 closed-world
capability manifest (an operation outside declared `spec.operations` is denied, exit **5**); a
resumed run enforces the manifest it started with, hydrated from its `deployment_snapshot_digest`
(#207), not whatever is deployed at resume time.

### End goal

* per-step policy overrides
* redaction policy
* PII handling rules
* tenant isolation
* prompt injection controls
* environment-specific policy inheritance

---

## 7.6 Environment

Overrides resources for a target environment.

```yaml
apiVersion: agentic.dev/v0
kind: Environment

metadata:
  name: prod

spec:
  overrides:
    agents:
      reviewer:
        model: anthropic/claude-sonnet-4
        constraints:
          timeoutSeconds: 60

    policies:
      default:
        execution:
          maxTotalCostUsd: 10.00
        approvals:
          requiredFor:
            - tool.notify.default
```

### MVP

* agent overrides (`model`, `constraints`)
* policy execution overrides (`maxTotalCostUsd`, `maxWallClockSeconds`, `maxIterations`, `requireStructuredOutput`)
* policy `approvals.requiredFor` overlay (union onto the named Policy; issue #171)
* no Tool.allow / HTTP endpoint overlays

### End goal

* tool endpoint overrides
* secret binding overrides
* runtime target selection
* scheduling overrides
* provider selection overrides

---

# 8. File Layout for a User Project

A project is authored **entirely in `.agent`** (ADR 007) — there is no `project.yaml` and no YAML
resources. Every `.agent` file anywhere under the project root is discovered and compiled into one
graph, so the split across files is organizational, not semantic; the smallest project is a single
`main.agent`. A larger project might group declarations by concern:

```text
my-agent-system/
  main.agent            # project `defaults` / `limits`, and the top-level workflow(s)
  agents.agent          # agent declarations (reviewer, incident, …)
  tools.agent           # tool declarations (github, slack, workspace, …)
  policies.agent        # policy declarations (default, strict, …)
  environments.agent    # environment overlays (dev, prod, …)

  schemas/
    pr-review-input.json
    review-output.json

  .agentic/             # generated: state.db, resolved-config.json, snapshots (git-ignored)
```

---

# 9. Validation Rules

## 9.1 Global validation

* all resources must have unique `kind/name`
* all references must resolve
* all imported paths must exist
* all schemas must be readable
* environment overrides must target existing resources

## 9.2 Agent validation

* referenced tools must exist
* referenced policy must exist
* input/output schema files must exist
* constraints must be sane
* model string must match configured provider namespace or allowed local alias

## 9.3 Tool validation

* exactly one transport block for the selected `type`
* retry values must be non-negative
* `spec.operations` keys and `effects` identifiers match `[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*`
* effect identifiers must not begin with `tool.`
* empty effect identifiers are rejected
* omitting `spec.operations` is valid YAML (fail-closed in the effect resolver, not a validate error)

## 9.4 Workflow validation

* step ids must be unique
* each step must specify exactly one of `agent`, `uses`, `workflow`, or `approval`
* interpolation refs must resolve
* interpolation may only reference predecessor steps (`needs` ancestors, or earlier YAML steps when `needs:` is omitted)
* `needs:` must name existing step IDs; cycles and dangling references fail validation with positions

## 9.5 Policy validation

* budgets non-negative
* action identifiers syntactically valid
* approval actions unique
* `spec.effects.permit` / `permitWithApproval` identifiers match `[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*` and must not begin with `tool.`
* validate and plan (after graph validate, not shared `config.Resolve`) run `effects.Check`; exit 2 when a workflow bound contains an effect not covered by that policy’s permit lists — skipped when no Tool declares `spec.operations` effects

---

# 10. CLI UX and Commands

## 10.1 Philosophy

The CLI should feel like:

* Terraform in clarity
* kubectl in resource mental model
* git in inspectability

Commands should be boring, stable, and scriptable.

---

## 10.2 Core commands

## `terfyn init`

Create starter project.

```bash
terfyn init my-agent-system
```

Creates an **`.agent`-only** project (ADR 007 — no `project.yaml`, no YAML resources):

* `main.agent` — a starter agent, a `default` policy, and the `hello` workflow, all `.agent` declarations

Add tools, agents, policies, and project `defaults`/`limits` as more `.agent` declarations under the root.

### MVP

yes

---

## `terfyn export`

Materialize the compiled resource graph as YAML (ADR 003): compilation output produced on
demand, never written to disk by default.

```bash
terfyn export --format yaml            # multi-document YAML stream to stdout
terfyn export --format yaml --output out/   # a YAML project directory (interchange output; NOT executable source — ADR 007)
```

The generated YAML is not the trustworthy record (applied deployment state plus the audit chain
is) and is not committed. It is a **one-way output**: under [ADR 007](adr/007-remove-yaml-ingestion.md)
`.agent` is the only executable source, so `LoadProject` refuses a `project.yaml` (`internal/project/loader.go`)
and an exported directory is for inspection/interchange, not re-execution — see #507.

### MVP

yes

---

## `terfyn fmt`

Format `.agent` sources to canonical form and normalize project YAML. Idempotent. The YAML
formatter is retained for the interchange path but not extended (ADR 003); comments are not
preserved on either surface.

### MVP

yes

---

## `terfyn validate`

Validate project.

```bash
terfyn validate
terfyn validate -e prod
```

Checks:

* YAML syntax
* schema correctness
* references
* imports
* interpolation refs
* policy and permission issues

### Output example

```text
Project: platform-assistant
Environment: prod

✓ Loaded 7 resources
✓ References resolved
✓ Schemas valid
✓ Workflow pr-review valid

Validation successful
```

### MVP

yes

---

## `terfyn plan`

Show desired vs current diff.

```bash
terfyn plan
terfyn plan -e prod
```

### Output example

```text
Plan: 2 to add, 1 to change, 0 to delete

+ create Agent/reviewer
+ create Workflow/pr-review
~ update Policy/default
    maxTotalCostUsd: 3.00 -> 10.00

Effect bound (Workflow/pr-review):
high:
- [high] effect_bound: github.write       autonomous  Agent/reviewer may select tool.github.post_comment
- [high] effect_bound: external.visible   autonomous  Agent/reviewer may select tool.github.post_comment
medium:
- [medium] effect_bound: github.read        static      step fetch_pr
low:
- [low] effect_bound: destructive        unreachable no grant path to tool.github.merge_pr

Capability delta:
Agent/reviewer
+ tool.github.post_comment

Effect delta:
+ github.write
+ external.visible

Authority:
  static      -> unchanged
  autonomous  -> WIDENED

Risk delta:
high:
- [high] authority_widening: AUTONOMOUS authority WIDENED.
- [high] budget_relaxation: Cost ceiling increased (Policy/default).
```

### MVP

partial

MVP plan supports:

* create/update detection
* field diff
* basic risk summary

MVP does not support:

* remote drift detection
* advanced behavioral estimates

---

## `terfyn apply`

Apply desired state.

```bash
terfyn apply
terfyn apply -e prod
terfyn apply --auto-approve
```

Behavior:

* runs validate
* computes plan
* prompts for approval unless `--auto-approve`
* writes deployment state

### MVP

yes, but local only

---

## `terfyn diff`

Show detailed resource diff.

```bash
terfyn diff
terfyn diff Agent/reviewer
```

### MVP

optional but strongly recommended

---

## `terfyn run`

Execute workflow ad hoc.

```bash
terfyn run workflow/pr-review --input repo=acme/api --input number=42
terfyn run workflow/pr-review --input-file input.json
```

Behavior:

* loads deployed or local desired config depending on mode
* validates input against workflow schema
* executes steps
* stores trace

### MVP

yes

---

## `terfyn logs`

Show execution traces.

```bash
terfyn logs
terfyn logs --run <run-id>
terfyn logs --workflow pr-review
```

### MVP

yes, basic trace/event view

---

## `terfyn audit`

Verify tamper-evident hash chains over `trace_events`.

```bash
terfyn audit verify
terfyn audit verify --run <run-id>
terfyn audit verify --limit 200
```

Re-derives each stored `hash` and checks `prev_hash` linkage. Without `--run`, scans recent runs only (`--limit`, default 50, max 500). Pre-migration rows without hashes are reported as **unchained** and do not fail verification. Exit **1** on chain break. See [`docs/AUDIT_CHAIN.md`](AUDIT_CHAIN.md).

### MVP

yes (issue #116)

---

## `terfyn inspect`

Print normalized resource.

```bash
terfyn inspect Workflow/pr-review
terfyn inspect Agent/reviewer -o yaml
```

Useful for debugging defaults and env overrides.

### MVP

optional but useful

---

## `terfyn test`

Run fixture-based tests.

```bash
terfyn test
terfyn test workflow/pr-review
```

### Test file example

```yaml
workflow: pr-review

cases:
  - name: happy-path
    input:
      repo: acme/api
      number: 42
    expect:
      outputContains:
        - summary

  - name: invalid-number
    input:
      repo: acme/api
      number: -1
    expectError: true
```

### MVP

stretch MVP or early post-MVP

---

## `terfyn fmt`

Normalize YAML formatting.

```bash
terfyn fmt
```

### MVP

nice-to-have

---

## `terfyn state`

Inspect stored state.

```bash
terfyn state list
terfyn state show Agent/reviewer
```

### MVP

optional

---

# 11. CLI UX Details

## 11.1 Common flags

```text
-e, --env <name>         environment override
-o, --output <fmt>       table|json|yaml
--project <path>         project root
--state <path>           explicit state DB path
--no-color               disable color output
```

## 11.2 Exit codes

* `0` success
* `1` generic failure
* `2` validation error
* `3` plan/apply conflict
* `4` execution error
* `5` policy denial

---

# 12. Internal Architecture of the Engine

## 12.1 Top-level architecture

```text
YAML Project
   ↓
Loader / Parser
   ↓
Normalization / Defaults
   ↓
Reference Resolution
   ↓
Validation
   ↓
Desired State Graph
   ↓
Planner
   ↓
Apply
   ↓
Stored Deployment State

Run Workflow
   ↓
Execution Engine
   ↓
Policy Engine
   ↓
Model + Tool Adapters
   ↓
Trace Recorder
   ↓
Runtime State
```

---

## 12.2 Core subsystems

## A. Spec subsystem

Responsibilities:

* load YAML files
* decode into typed structs
* normalize defaults
* resolve imports
* resolve references
* return canonical in-memory project graph

Key types:

```go
type ResourceID struct {
    Kind string
    Name string
}

type Project struct {
    Meta        Metadata
    Spec        ProjectSpec
    Agents      map[string]*Agent
    Tools       map[string]*Tool
    Workflows   map[string]*Workflow
    Policies    map[string]*Policy
    Environments map[string]*Environment
}
```

---

## B. Planner subsystem

Responsibilities:

* compare desired project state against stored deployment state
* compute create/update/delete operations
* compute human-readable diffs
* compute risk summary

Key output:

```go
type Plan struct {
    Operations []Operation
    Risk       RiskSummary
}

type Operation struct {
    Action   string // create, update, delete
    Target   ResourceID
    Diff     []FieldChange
}
```

### MVP risk summary

Structured `RiskItem` list (category, severity, reason, target, witness path; issue #165):

* approval removal — entries removed from `policy.approvals.requiredFor`
* budget relaxation — `maxTotalCostUsd` / `maxWallClockSeconds` increased, or the effective `maxIterations` ceiling raised (0/absent = the default 32, so removing an explicit lower value is also a relaxation)
* model changes — agent `model` provider or id
* tool surface change — tools added to an agent's `tools` list (write-capability risk is derived from the tool's declared `safety.sideEffects`; the removed `tool.permissions` `permission_widening` name heuristic no longer applies)
* runtime target change — a workflow's `runtime` target changed (e.g. `local` → `claude-code`)

C1 witness hops are resource-level (static). Effect-bound Workflow→step→Agent→tool.operation hops land on the same `Witness` field and table/JSON/YAML render path (`FormatPlanSection` / `ExportRisk`). Capability delta and effect delta are separate `RiskItem` categories; `authority.static` / `authority.autonomous` (`unchanged` | `widened`) are structural JSON/YAML fields so CI can gate on `AUTONOMOUS` `WIDENED`. `RiskSummary.Messages` remains the item reasons for string consumers; JSON/YAML keep `"risk": []string` and expose structured `"riskItems"`. Table output groups items under `high:` / `medium:` / `low:` (issue #166) and prints the desired effect bound plus authority delta (issue #191).

### End goal risk summary

* behavioral contract widening
* policy relaxations
* prompt changes with semantic classification
* runtime target change impact

---

## C. State subsystem

Two different state domains:

### 1. Deployment state

Tracks what has been applied.

Example records:

* resource kind/name
* normalized spec hash
* applied timestamp
* env target
* version

### 2. Runtime state

Tracks workflow runs.

Example records:

* run id
* workflow name
* start/end
* status
* step events
* tool calls
* token/cost summary
* errors

### MVP storage

SQLite for both.

### End goal

Postgres for team/shared mode.

---

## D. Apply subsystem

Responsibilities:

* take a plan
* confirm/persist operations
* update deployment state
* optionally prepare runtime-specific artifacts later

MVP apply does **not** deploy to an external cluster.
It records local deployed desired state.

That is enough to establish plan/apply discipline.

---

## E. Execution engine

Responsibilities:

* execute workflows
* resolve step inputs
* call tools
* invoke agents
* enforce retries/timeouts
* collect outputs
* produce final workflow output

### MVP execution model

* DAG steps with optional `needs:` (implicit sequential when omitted)
* independent steps run concurrently with a bounded worker pool
* local execution only
* no background daemons
* no reconciliation loop

### Engine flow

1. load workflow
2. validate runtime input
3. initialize run context
4. for each ready step (all `needs:` / implicit predecessors complete), up to the concurrency bound:

   * resolve interpolations from completed `${steps.*}` outputs
   * enforce policy (`CheckRun` against accumulated run cost; concurrent steps share one total without double-counting)
   * execute tool or agent (agent steps with `spec.tools` run a bounded Generate / `tool_use` / `tool_result` loop; each listed Tool advertises one operation; `policy.CheckToolCall` runs before every tool execution; the loop stops on `end_turn` or `constraints.maxIterations`, default 8, clamped to the policy `execution.maxIterations` ceiling or 32 — #522)
   * validate output if configured
   * record trace (`logicalOrder` = YAML step index alongside wall-clock `seq`)
   * checkpoint the completion set before marking the step succeeded
5. compute workflow output
6. persist run result

---

## F. Agent runtime adapter

Responsibilities:

* assemble prompt payload
* attach tools
* invoke provider
* return structured output

The engine implements the bounded tool-calling loop (issue #160). Each agent-declared Tool resource is advertised as one `ToolDef` (name = Tool metadata.name, permissive object schema). `agent.spec.tools` entries may be the Tool metadata name or a pinned uses string `tool.<name>.<operation>`. `ToolChoice` is `auto`. Type defaults when only the name is listed: native → `tool.<name>.echo`; mock/mcp → `tool.<name>.default`. HTTP has no default (`parseOperation` would treat `default` as `GET /default`); list `tool.<name>.<method.path>` — pinned `tool.<name>.default` is rejected the same way as a bare HTTP name. `terfyn validate` / `plan` apply these advertised-uses rules (unknown tools, HTTP method.path, conflicting ops on one Tool name). Only the ToolDef name is accepted as a `ToolCall.Name` (ADR 002: no operation is agent-callable unless it was advertised). Aliases such as `helper.echo`, `tool.helper.echo`, `helper.command.run`, or HTTP `delete.users` fail before `CheckToolCall` / `Tools.Call`. On `StopReason: tool_use`, each accepted call is checked with `CheckToolCall`, then executed via `Tools.Call` on the agent `constraints.timeoutSeconds` context. Results are appended as `ChatMessage.ToolResults` (with the assistant `ToolCalls` replayed) and the loop continues. Agents that declare no tools stay a single `Generate` with no `Tools` field. Loop cost (model + tool) accumulates into the step `GenerateMeta`; `policy.CheckRun` runs after each Generate and tool turn so `execution.maxTotalCostUsd` / wall-clock apply inside a single agent step. `constraints.maxIterations` (default 8, clamped to the policy `execution.maxIterations` ceiling, or 32 when unset — #522) counts **Generate turns**; the capped turn's `tool_use` is **not** executed. On reaching the cap the engine emits `limit_hit` (`kind: max_iterations`) and then **finalizes gracefully** (#518): it forces one final tool-free completion so the agent returns its best output and the run **continues** — the step fails only if that final turn still yields no valid output (`maxIterations: 1` is a single tool-capable turn plus this finalize turn; the capped turn's tools never run). HITL interrupt is **not** consulted inside the loop: inner uses must already be pre-approved (`terfyn run --approve` / `ApprovedActions`) or `CheckToolCall` fails closed. Policy denial uses the existing `DeniedError` path (CLI exit **5**).

`agent.spec.tools` is an **autonomous capability grant**, not a static call list (ADR 002 Path 1). Epic A shipped genuine tool selection (#160 / #161); grant semantics therefore apply. Each entry is a grant of a **concrete operation** (`tool.<name>.<operation>`), not a Tool resource and not an effect class. Every granted operation contributes to the agent's action space whether or not a workflow `uses:` step names it. Widening the list expands a nondeterministic component's action space — `terfyn plan` reports a new **autonomous** effect at higher severity than a new **static** one, and prints `AUTONOMOUS` `WIDENED` when a grant is added even if the named effect set is unchanged. Issue #189 computes the bound in [`internal/effects`](../internal/effects) over the **desired** graph; issue #191 prints `bound(desired)` vs `bound(deployed)` (reconstructed from applied `NormalizedSpecJSON`; empty store is an empty baseline). For MCP tools the grant is sound against the pinned operation manifest (#204): runtime `CheckToolCall` denies any operation outside the tool's declared `spec.operations`, so a live `tools/list` can no longer expand the callable world (see §7.3, *Capability manifest*). The run-**pinned** deployed manifest — enforcing what a resumed run started with, via `runs.deployment_snapshot_digest` (§14) — ships in #207: on a pinned resume the graph and authority are hydrated from the snapshot, so a widening apply does not widen the resumed run's callable set. Loop, traces, and HITL vs exit **5**: [`docs/AGENT_LOOP.md`](AGENT_LOOP.md).

Abstraction:

```go
type ModelClient interface {
    Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}
```

Model contract (issue #156):

* `GenerateRequest` — `Model`, `Messages`, optional `Tools []ToolDef`, `ToolChoice` (`auto` | `none` | `required`; zero value = `auto`).
* `GenerateResponse` — `Content`, optional `ToolCalls []ToolCall`, `StopReason` (`end_turn` | `tool_use` | `max_tokens`; unknown provider values are passed through and must not be treated as `end_turn`), `Meta`.
* `ChatMessage` — `Role`, `Content`, optional `ToolCalls []ToolCall` to replay an assistant tool-use turn, optional `ToolResults []ToolResult` for returning tool output to the model.
* `GenerateMeta` — `DurationMs`, `PromptTokens`, `CompletionTokens`, `CostUSD` (OpenAI and Anthropic estimate USD from token usage × the per-model table in `internal/models/cost.go`; unknown ids stay 0; issue #162).

Provider adapters map these neutral shapes to OpenAI `tools` / `tool_calls` and Anthropic `tools` / `tool_use` / `tool_result`. The OpenAI client implements that mapping (issue #157): `ToolDef` → Chat Completions `tools`; `tool_calls` or compatible `finish_reason: stop` with calls → `StopReason: tool_use` and populated `ToolCalls`; `finish_reason: length` or `content_filter` (including truncated or complete `tool_calls` blocks) → `max_tokens` / `content_filter` with `ToolCalls` cleared — calls are only actionable when `StopReason` is `tool_use`; prior `ToolCalls` + `ToolResults` → assistant `tool_calls` then `role: "tool"` messages. The Anthropic adapter implements the same contract (issue #158): `ToolDef` → Messages API `tools` (`input_schema`); `ToolChoice` `auto`/`none`/`required` → `tool_choice.type` `auto`/`none`/`any`; `tool_use` blocks (or `stop_reason: end_turn` with those blocks) → `StopReason: tool_use`; `max_tokens` / `refusal` (including incomplete `tool_use` input) → those stop reasons with `ToolCalls` cleared; prior `ToolCalls` + `ToolResults` → assistant `tool_use` then user `tool_result` blocks (extra text on a result turn stays in that user message; consecutive `ToolResults` ChatMessages are merged so roles still alternate). Empty tool output still sends `tool_result.content`. Plain `end_turn` with no text still errors; `tool_use` may omit text.

MVP:

* OpenAI-compatible
* Anthropic optional
* mock provider for tests

---

## G. Tool runtime adapter

Responsibilities:

* resolve tool name to executable transport
* enforce permissions
* execute operation
* normalize result

Inner-loop Calls from the agent tool-calling loop use the same `CheckToolCall` then `ToolExecutor.Call` path as workflow `uses:` steps (`runToolStep`). The evaluator is the **workflow** policy (`wf.Spec.Policy` → `wfPol` into `runAgentStep` / `runToolStep`), not `agent.spec.policy` (YAML/plan documentation; not the inner gate). Policy denial records `system_error` and does not invoke the tool. HITL interrupt (`maybeInterruptForHitl`) applies only to workflow `uses:` steps, not inner agent-loop tools. See [`docs/AGENT_LOOP.md`](AGENT_LOOP.md).

Abstraction:

```go
type ToolExecutor interface {
    Call(ctx context.Context, req ToolCallRequest) (ToolCallResponse, error)
}
```

MVP:

* MCP stdio
* HTTP
* mock/native

---

## H. Policy engine

Responsibilities:

* decide whether a workflow/step/tool call is allowed
* enforce budgets/timeouts
* gate approval-required actions

Abstraction:

```go
type PolicyEvaluator interface {
    CheckRun(ctx context.Context, run RunContext) error
    CheckStep(ctx context.Context, step StepContext) error
    CheckToolCall(ctx context.Context, call ToolCallContext) error
}
```

### MVP checks

* workflow wall-clock budget
* total cost ceiling
* no unknown tools
* approval-required actions denied unless explicitly approved

### End goal checks

* environment-sensitive rules
* tenant rules
* sensitive output handling
* prompt injection mitigation hooks
* egress restrictions

---

## I. Trace recorder

Responsibilities:

* append structured events
* persist for logs and debugging
* support replay-ish inspection

Event examples:

```go
type TraceEvent struct {
    RunID     string
    Timestamp time.Time
    Type      string
    StepID    string
    Message   string
    Data      map[string]any
}
```

Event types (issue #115 closed taxonomy, `TaxonomyVersion` 3):

* run_started, run_finished, run_error
* llm_completion
* tool_selection, tool_execution
* hitl_request_created, hitl_decision_submitted, hitl_resolution_applied
* memory_read, memory_write (reserved)
* system_error, limit_hit
* workflow_call_started, workflow_call_finished (issue #194)

Legacy dot-notation types (`run.started`, `tool.called`, …) are normalized to the above on read and by SQLite migration `006`.

Issue #116 adds a **tamper-evident hash chain** per run: each persisted event stores `prev_hash` and `hash` over canonical (redacted) fields. See [`docs/AUDIT_CHAIN.md`](AUDIT_CHAIN.md).

---

## J. Effect bound (issue #189)

[`internal/effects.Compute`](../internal/effects) walks an already-resolved **desired**
`ProjectGraph` and returns a bound for every Agent and Workflow. It does not apply
Environment overlays, call MCP `tools/list`, or change `CheckToolCall`. `terfyn plan`
renders the bound and the authority delta vs stored deployment state (issue #191) in
table/JSON/YAML.

[`internal/effects.Check`](../internal/effects) (issue #190) compares each **workflow**
bound (including autonomous agent grants) against that workflow’s `Policy.spec.effects`.
It runs from the validate and plan command paths after graph validate, not from
`config.Resolve`. Unpermitted effects fail validate and plan with exit **2** and a witness
path that tags `AUTONOMOUS` on agent-selection edges. Unknown reachable operations fail
closed (the message names the tool). A policy with no permit block permits nothing once
any tool declares operations; if no tool declares operations, Check is skipped. `permit`
vs `requiresApproval` / `approvals.requiredFor`: the stricter rule wins (any reachable op
for an ident; `permitWithApproval` when the same ident is in both lists) and the error
says which applied. This static check is separate from the #204 runtime closed-world
manifest enforcement in `CheckToolCall` (§7.3, *Capability manifest*).

The bound is a sound **upper** set of named effects the root may perform, over both
deterministic and autonomous paths. Two edge kinds are preserved on each witness hop:

| Kind | Source | Reachability |
|---|---|---|
| **static** | workflow step `uses:` naming `tool.<name>.<operation>` | authored call |
| **autonomous** | `agent.spec.tools` grant, resolved to one advertised uses string | the agent may choose the operation |

Autonomous edges resolve through **concrete operations**, never effect classes. A grant is
`tool.<name>.<operation>` (or a Tool metadata name resolved by `[ResolveAgentAdvertisedTools]`:
native → `echo`, mock/mcp → `default`, HTTP must be pinned). The bound unions
`[ResolveOperationEffects]` for those operations. There is no `grants { github.read }`.

Witness path: `Workflow → step → Agent → tool.operation`, each hop tagged `static` or
`autonomous`. Agent-only roots omit workflow/step hops. If an ident is reachable by
both a static `uses:` and an autonomous grant, the bound tags it **autonomous** and the
exported witness is that grant path (path-max, not first-witness). Hop fields match
`plan.WitnessHop` (`kind`, `name`, `id`, `reachability`) so plan output maps without
`effects` importing `plan`. Kinds: `workflow`, `step`, `agent`, `tool_operation`. Pos is
metadata only and is not part of the bound. Plan JSON/YAML expose those hops on
`effectBound` / `riskItems` plus `authority.static` / `authority.autonomous`
(`unchanged` | `widened`) for CI gates. Capability changes (concrete
`tool.<name>.<operation>` grants) and effect changes (named consequence classes) are
separate lines: a new grant whose effects are already reachable widens capability with
an empty effect delta and still marks autonomous authority `WIDENED`.

**Unknown vs unreachable.** A reachable operation with no declared effects
(`[ResolveToolEffects].Unknown` / empty operation set) is an **explicit unknown** in the
bound — fail-closed, not empty/allow, not omitted. Effects **declared** on a tool operation
that is not reachable from that root are listed as **unreachable**, not dropped.

**#204 closed world (shipped).** This package computes desired-graph bounds over
declared `spec.operations` and advertised uses; `validate`/`plan` bound desired. The runtime
closed-world enforcement — `run` denying any operation outside the deployed manifest — ships
on the `CheckToolCall` policy path (§7.3), so a remote `tools/list` expansion is denied, not
absorbed. The **run-pinned** deployed manifest (a resumed run enforcing what it started with)
ships in #207: `prepareForResume` hydrates the run's graph and authority from its pinned
deployment snapshot, so a widening apply between suspend and resume does not widen the resumed
run's callable set (covered by `TestCompiledWorkflowEvaluator_pinnedIgnoresWidenedDiskSnapshot`).

Walks use a visiting set (least fixed point) so cyclic graphs terminate. Production `workflow:`
steps are walked (issue #194); diamond reuse of one agent does not duplicate infinitely.

---

# 13. Execution Semantics

## 13.1 Interpolation

Supported syntax:

* `${input.foo}`
* `${steps.fetch_pr.output}`
* `${steps.review.output.summary}`

MVP:

* dot path lookup only
* no expressions
* no functions
* no loops in interpolation

This should stay simple.
Do not invent a scripting language.

Fan-in: a step whose `needs:` lists multiple predecessors may interpolate `${steps.<id>.output}` for every ancestor. Siblings that do not precede the step cannot be referenced.

Whole-field vs embedded (issue #193): when the entire field value is one `${...}` token, interpolation preserves the resolved JSON type (object, array, number, boolean, string). Tokens embedded in surrounding text are still coerced to strings (scalars printed; objects/arrays as JSON text).

```yaml
body: ${steps.review.output.summary}           # typed value
body: "Summary: ${steps.review.output.summary}"  # string
```

`input.schema` / `output.schema` file references are loaded onto the graph at validate time. A step that consumes `${steps.x.output.foo}` is checked against step `x`'s declared output schema when one exists, and against the consuming agent's input schema when that exists. Mismatches fail validate with a YAML position. Missing schemas remain allowed (gradual typing); a declared schema is honored.

---

## 13.2 Step types

### Agent step

```yaml
- id: review
  agent: reviewer
  with:
    pr: ${steps.fetch_pr.output}
```

### Tool step

```yaml
- id: fetch_pr
  uses: tool.github.pull_request.get
  with:
    repo: ${input.repo}
    number: ${input.number}
```

### Workflow step (issue #194)

```yaml
- id: compose
  workflow: fetch-and-review
  with:
    repo: ${input.repo}
    number: ${input.number}
```

The callee is a static `Workflow` metadata name. `with:` is interpolated in the caller then validated as the callee's input. The callee's `output.value` is this step's `output`. Recursion is a validate error (positions on `workflow:`). Depth is capped by `maxWorkflowNesting` (default 8). Policy is the stricter of caller and callee (both must pass; tighter budgets win; HITL `interruptOn` is unioned). Nested traces and mid-subworkflow checkpoints are described in §7.4.

### Approval step (issue #195)

```yaml
- id: review_plan
  approval: true
  with:
    plan: ${steps.draft.output.summary}
```

The engine interpolates `with:`, then suspends through the same HITL checkpoint used for gated tool calls (`pendingHitl`, `hitl_request_created`). Resume `--decision approve` / `edit` completes the node; **`reject` fails the run** (not only the branch). In a parallel group the interrupt is scoped to this step's branch; resume skips completed siblings. Step output is the reviewed payload (original `with:` or the edited object). `switch` is not offered.

MVP step result shape:

```json
{
  "output": {},
  "meta": {
    "durationMs": 1200,
    "costUsd": 0.02
  }
}
```

## 13.2.1 Graph execution (issue #192)

> **Single run path (#278).** Every workflow runs on the `execir` interpreter; the `WorkflowStep`
> DAG runtime was retired. A `needs:` graph lowers to an `execir.Graph` scheduled with the same
> depends-ready, bounded-concurrency semantics described below; straight-line workflows lower to a
> flat node list. The behavior in this section is unchanged — only the executor is now the shared
> interpreter (ADR 002 §5).

`WorkflowStep.needs` is a static list of step IDs. This is graph structure (ADR 002), not computation: no conditionals, loops, or dynamic fan-out.

**Implicit sequential:** when every step omits `needs:`, execution order is YAML order (step *i* waits for *i-1*). Existing examples keep this behavior.

**Explicit DAG:** when any step sets `needs:`, omitted `needs` means a root. Roots run concurrently (bounded). A join lists every branch; it does not start until all listed steps have completed, and then sees those outputs via `${steps.*}`.

Checkpoints persist `completed` (sorted step IDs) plus step outputs. Resume skips completed IDs, so a parallel group can continue after one branch finished. `StepIndex` remains the YAML index of the step that wrote the checkpoint (HITL / interrupt identity), not a linear cursor through remaining work.

Trace `seq` follows SQLite insert (wall-clock, nondeterministic under concurrency); `audit verify` hashes stored rows in their persisted order. (The DAG-era `data_json.logicalOrder` stamp was removed with the DAG runtime in #278; deterministic replay comes from the interpreter's `CallSite`-keyed memo, not a trace-order stamp.)

## 13.2.2 Subworkflow execution (issue #194)

A `workflow:` step is a nested `execir` run of the named callee on the same run id. Interpolation inside the callee uses the callee's input (`with:`) and the callee's local step ids. The engine qualifies persisted step ids as `callerStep/calleeStep` so a parent and child may reuse ids. Checkpoints wrap in-flight callee state in `nested` (stackable) while the outer `completed` set still skips finished caller steps. Resume restores the inner DAG and continues; a later outer join sees the callee's `output.value` as `${steps.<id>.output}`.

Trace taxonomy 3 adds `workflow_call_started` and `workflow_call_finished`. Nested events include `callStack` (callee names from the root) and `workflow` (innermost).

## 13.2.3 Approval-step execution (issue #195)

An `approval:` step always pauses (unless `--auto-approve`). The engine reuses the #106 HITL checkpoint and `--decision` resume path. `pendingHitl.kind` is `approval`; `uses` is the sentinel `workflow.approval`. Allowed decisions are `approve`, `reject`, and `edit` when `with:` is non-empty (`switch` is omitted).

**Reject** fails the run (`HitlRejectedError`), not only the branch. **Interrupt** in a parallel group does not fail sibling steps; resume reconstructs `completed` and continues remaining roots/joins.

HITL events (`hitl_request_created`, `hitl_decision_submitted`, `hitl_resolution_applied`) append onto the same per-run audit chain; `audit verify --run` must succeed after resume.

---

## 13.3 Output validation

If a step or agent has output schema, validate returned output.
Failure should fail the step unless policy later allows soft-fail.

MVP:
hard fail

Schema files named by `input.schema` / `output.schema` are compiled at validate (not only checked for existence) and held on the graph. Static wiring (issue #193) uses those documents so a consumer `${steps.x.output.foo}` is rejected before run when it cannot inhabit the producer output schema or the consumer input schema.

---

## 13.4 Retries

Tool retries in MVP:

* configured per tool
* only on retryable transport/provider errors

Agent retries in MVP:

* off by default
* optional single retry on transient provider failure

Do not retry semantic failure blindly.

---

# 14. State Model

## 14.1 Deployment state schema

Suggested tables:

### `applied_resources`

* `kind`
* `name`
* `env`
* `spec_hash`
* `normalized_spec_json`
* `applied_at`

### `applied_projects`

* `project_name`
* `env`
* `version`
* `applied_at`

### `deployment_artifacts` (issue #207)

Immutable, content-addressed payloads, deduped by content. Retained until no snapshot references
them; never mutated once written (`INSERT … ON CONFLICT(digest) DO NOTHING`). `format_version` says
how to decode the payload — an unknown format fails loudly, never reinterpreted.

* `digest` (PRIMARY KEY — SHA-256 of `payload`)
* `kind` (`resolved_graph` | `execution_ir` | `capability_manifest`)
* `format_version`
* `payload` (BLOB)
* `created_at`

### `deployment_snapshots` (issue #207)

The content-addressed root of the immutable configuration a run executed under. `digest` is over
the canonical snapshot identity (`format_version`, `compiler_version`, `environment`, and the three
artifact digests) — **not** timestamps or paths — so it is stable across a change of `--state` path
or project directory. `compiler_version` is provenance for the compilation as a whole.

* `digest` (PRIMARY KEY)
* `format_version`
* `compiler_version`
* `environment`
* `graph_digest` → `deployment_artifacts`
* `execution_ir_digest` → `deployment_artifacts` (the `execution_ir` artifact: serialized per-workflow `execir.Program`; empty only for a project with no lowerable workflow)
* `capability_manifest_digest` → `deployment_artifacts`
* `schema_bundle_digest` → `deployment_artifacts` (the `schema_bundle` artifact: referenced JSON
  Schemas captured at run start; empty when the project references none). A pinned resume validates
  workflow input / agent output against these bytes, not a re-read of the file on disk.
* `created_at`

### `deployment_env_current` (issue #207)

The **current deployed** snapshot per environment — a mutable pointer, distinct from the immutable
content-addressed rows above. `apply` upserts it on **every** apply (including a re-apply of an
earlier digest, a rollback `A → B → A`), so `superseded` on a run means "differs from what is
deployed now", not "not the newest `created_at` row". Content-addressed rows cannot double as a
recency index.

* `environment` (PRIMARY KEY)
* `snapshot_digest` → `deployment_snapshots`
* `updated_at`

```text
                 DeploymentSnapshot
                 /          |          \
        resolved graph  execution IR  capability manifest
        (policy, tools,   (deferred)   (#204 authority boundary)
         agents, models)
```

---

## 14.2 Runtime state schema

### `runs`

* `run_id`
* `workflow_name`
* `env`
* `status`
* `started_at`
* `finished_at`
* `input_json`
* `output_json`
* `error_text`
* `total_cost_usd`
* `workflow_spec_hash`, `environment_name`
* `deployment_snapshot_digest` → `deployment_snapshots` (issue #207): the pinned deployment the run
  executes under. **Resume hydrates configuration and authority from this snapshot, not from
  re-resolved current config**, so a policy/tool/manifest edit landing mid-run cannot change an
  in-flight run's authority (the invariant ADR 002 states). Empty for runs created before #207,
  which fall back to current config.

### `run_steps`

* `run_id`
* `step_id`
* `status`
* `started_at`
* `finished_at`
* `input_json`
* `output_json`
* `error_text`
* `cost_usd`

### `trace_events`

* `run_id`
* `seq`
* `timestamp`
* `type`
* `actor_type` (issue #115)
* `step_id`
* `data_json`
* `tenant_id`, `thread_id`, `actor_id` (issue #111; copied from parent run)
* `prev_hash`, `hash` (issue #116; nullable for pre-migration rows)

Per-run hash chain: `hash = SHA-256(canonical_event ‖ prev_hash)`. First chained event in a run links to a run-scoped genesis anchor. See [`docs/AUDIT_CHAIN.md`](AUDIT_CHAIN.md).

---

# 15. Modules and Registry

## MVP

No modules.

## End goal

Modules should allow reuse.

Example:

```yaml
module:
  source: github.com/acme/agent-modules/pr-reviewer
  version: 0.2.1

inputs:
  model: openai/gpt-4.1
  githubTool: github
```

Needs:

* lockfile
* version resolution
* input schema
* module output exposure
* integrity checks

Registry later:

* public or private modules
* reusable workflows
* policy packs
* tool packs

---

# 16. Remote Runtime and Reconciliation

## MVP

No controller. No daemon. No remote cluster.

`apply` only writes local deployed state.

This is enough to prove:

* spec
* validation
* plan
* run
* trace

## End goal

Add remote runtime support.

Possible modes:

* local
* server mode
* Kubernetes-backed
* Temporal-backed
* worker pool mode

At that stage, reconciliation becomes meaningful:

* desired resources stored centrally
* controller compares desired vs actual
* controller converges state
* drift detectable remotely

This is **post-MVP**.

---

# 17. Testing Strategy

## 17.1 Unit tests

* spec parser
* reference resolution
* interpolation
* planner diff
* policy checks
* state store

## 17.2 Integration tests

* run sample workflow locally
* mock model/tool providers
* SQLite state/traces

## 17.3 Golden tests

CLI output for:

* validate
* plan
* diff

## 17.4 Fixture tests

Workflow-level tests via YAML fixtures.

---

# 18. MVP Scope

## Included in MVP

### Spec

* Project
* Agent
* Tool
* Workflow
* Policy
* Environment

### Runtime

* local only

### Tools

* MCP stdio
* HTTP
* mock/native

### Models

* at least one provider
* mock provider for tests (`MockClient.Script` sequences `tool_use` then final text for CI loops; issue #159)

### CLI

* `init`
* `validate`
* `plan`
* `apply`
* `run`
* `logs`

### Engine

* sequential workflows
* interpolation
* schema validation
* basic policy enforcement
* trace recording

### State

* SQLite
* deployment state
* runtime traces

### Output

* table + json

---

## Explicitly out of MVP

* modules
* registry
* reconciliation controller
* remote shared state
* scheduled/event triggers
* loops/conditionals
* rich approval workflows
* distributed execution
* plugin SDK
* advanced drift detection
* semantic change classification
* multi-tenant authn/authz

---

# 19. End Goal

The end-state system should support:

* declarative multi-agent systems
* environment-aware config
* plan/apply/diff/drift
* reusable modules
* policy packs
* remote control plane
* centralized state
* controller reconciliation
* multiple runtimes
* team collaboration
* approval workflows
* event and schedule triggers
* observability and auditability
* registry ecosystem

In other words:

> a real control plane for agent systems

not just a local runner.

---

# 20. Recommended Implementation Phases

## Phase 1

Foundations.

* resource structs
* YAML loader
* validation
* project graph
* SQLite state
* local runtime
* sequential workflow execution
* model/tool interfaces
* trace recorder
* core CLI

## Phase 2

Deployment discipline.

* better plan output
* apply confirmation
* environment overrides
* richer policy engine
* diff command
* inspect command
* test command

## Phase 3

Reuse and governance.

* modules
* lockfile
* policy packs
* richer risk summaries
* workflow approvals

## Phase 4

Control plane.

* server mode
* remote state
* controller loop
* remote runners
* drift detection
* team auth

## Phase 5

Effects and IR expressiveness. See [ADR 002](adr/002-language-frontend-and-ir-expressiveness.md).

* source positions as first-class IR data (prerequisite; see
  [ADR 003](adr/003-yaml-as-compilation-output.md))
* declared effects on tool operations
* transitive effect bounds over the project graph, including autonomous agent tool selection
* effect enforcement against Policy at validate/plan time
* parallel branches and fan-in
* subworkflows
* typed step outputs flowing through interpolation
* workflow-level human approval steps

## Phase 6

Language frontend. See [ADR 002](adr/002-language-frontend-and-ir-expressiveness.md) and
[ADR 003](adr/003-yaml-as-compilation-output.md).

* `.agent` lexer, parser, typed AST
* lowering to the resource model with source maps
* type and effect checking, including the checked `effects` clause
* conditional steps and loops
* YAML demoted to compilation output and interchange (`terfyn export`)

---

# 21. Recommended Go Libraries

Practical picks:

* CLI: `cobra`
* YAML: `gopkg.in/yaml.v3`
* config/schema helpers: `invopop/jsonschema` or JSON Schema validator libs
* SQLite: `modernc.org/sqlite` or `mattn/go-sqlite3`
* table output: `charmbracelet/lipgloss` + simple table lib
* gRPC later: `google.golang.org/grpc`
* protobuf later: `google.golang.org/protobuf`

Keep dependencies conservative.

---

# 22. Example MVP Flow

## Author

User writes `.agent` source (ADR 007 — no `project.yaml`), e.g.:

* `main.agent` — the `pr-review` workflow (and any project `defaults`/`limits`)
* `reviewer.agent` — the reviewer agent, the `github` tool, and the `default` policy

(split across files however you like — every `.agent` file under the root compiles into one graph)

## Validate

```bash
terfyn validate
```

## Plan

```bash
terfyn plan
```

Output:

```text
Plan: 4 to add, 0 to change, 0 to delete
+ Agent/reviewer
+ Tool/github
+ Workflow/pr-review
+ Policy/default
```

## Apply

```bash
terfyn apply
```

## Run

```bash
terfyn run workflow/pr-review --input repo=acme/api --input number=42
```

## Inspect logs

```bash
terfyn logs --workflow pr-review
```

That is enough to prove the product.

---

# 23. Final Recommendation

Build this as:

* **Go CLI**
* **YAML declarative spec**
* **SQLite local state**
* **local-first engine**
* **clear separation between deployment state and execution state**

Do **not** start with:

* server mode
* Kubernetes operator
* module registry
* remote control plane

That is how the project dies early.

The correct MVP is:

> local declarative agent systems with validate, plan, apply, run, and logs.

That is small enough to build and sharp enough to matter.
