# `.agent` grammar-gap audit (issue #440, part of #430)

**Goal:** before migrating any project off YAML, enumerate what the `.agent` language still *cannot*
express, so we know the true scope of "remove YAML as a project source" (#430 Phase 3).

**Method:** compared every top-level production and field list in `internal/lang/parser.go` /
`internal/lang/parser_resources.go` against the full YAML resource model in `internal/spec/kinds.go`,
then counted real usage across the 15 `examples/*` projects.

**Status:** audit only — nothing changed. This is scoping input for #440. Phase 1 (#430) shipped in
PRs #432 (built-in providers), #434 (`.agent`-only loading), #436 (runnable `.agent`-only `init` +
`.agent` policy `preset`); follow-ups #438 (no-policy safe default) and #439 (`.agent`-native
`terfyn new`) are merged.

## What `.agent` fully expresses today

- **Top-level declarations:** `agent`, `workflow`, `tool`, `policy`. (No `project`, no `environment`.)
- **Agent:** `model`, `policy`, `description`, `instructions`, `grants`, `input`, `output`,
  `constraints { maxIterations, timeoutSeconds, temperature, requireStructuredOutput }`.
- **Workflow:** `description`, `policy`, `input`, `output`, and the full control-flow body.
- **Tool:** `type`, `safety { trusted, sideEffects, requiresApproval }`, `operations { <op> { effects { … } } }`.
- **Policy:** `preset` (added #436), `execution`, `approvals`, `effects`.
- **Model providers:** built-in namespaces (`anthropic`/`openai`/`gemini`/`grok`/`kimi`/`mock`)
  resolve with no config (#432).

## Gaps — capabilities that still require YAML

Categorized by #430's framework. "Examples" = how many of the 15 example projects use it.

### A. Hard grammar blockers (program semantics, no `.agent` syntax, no reduction)

| Capability | Spec location | Examples | Notes |
|---|---|---|---|
| **Environments** (`kind: Environment`, per-agent/per-policy overrides) | `EnvironmentResource` | 3 | ✅ Closed by #448 — `environment <Name> { overrides { … } }`. |
| **Tool `mcp` transport** (`command`/`args`/`url`/`headers`) | `ToolSpec.MCP` | 0 | ✅ Closed by #447 — `tool { mcp { … } }`. |
| **Tool `http` transport** (`baseURL`/headers/methods) | `ToolSpec.HTTP` | 0 | ✅ Closed by #447 — `tool { http { … } }`. |
| **Policy `hitl`** (interrupt-on, review config) | `PolicySpec.Hitl` | 1 | ✅ Closed by this PR — `policy { hitl { … } }`. |
| **Custom model providers / aliases** (`type` + `baseURL` + key) | `Providers.models` | 0 | ✅ Closed by #440 + #546 — top-level `provider <alias> { type … baseUrl … apiKeyFrom … workspaceIdFrom … }`. `baseUrl` is an HTTP(S) endpoint passed to the selected adapter. Built-ins stay implicit. |

### B. Softer gaps (program semantics, but a workaround exists)

| Capability | Spec location | Examples | Workaround |
|---|---|---|---|
| **`defaults.policy`** (project-wide default policy) | `ProjectDefaults.Policy` | 15 | Reference the policy per-workflow, or declare a policy named `default` (+ the #438 no-policy fallback). |
| **Tool `workspace {}`** (native adapter root / test command) | `ToolSpec.Workspace` | 1 | ✅ Closed (#440) — `tool { workspace { root "…" testCommand "…" } }`; `TERFYN_WORKSPACE_*` env fallback still applies. |
| **`runtime`** on agent/workflow (`claude-code`/`gemini`) | `AgentSpec.Runtime` / `WorkflowSpec.Runtime` | 14 (all `local`) | `--runtime` flag; `local` is the default, so the declaration is usually droppable. |
| **Agent `memory`** | `AgentSpec.Memory` | 0 | none yet |
| **Policy `tools.forbidUnknownTools`**, **Policy `security`** | `PolicySpec.Tools` / `.Security` | 0 | some reduce to built-in defaults |
| **Tool `permissions` / `retry` / `limits`** | `ToolSpec.*` | 0 | none yet |
| **Workflow `trigger` / `limits`** | `WorkflowSpec.*` | 0 | none yet |
| **Project `limits`, `traces`, `telemetry`** | `ProjectSpec.*` | 0 | reduce to built-in defaults / env / config |

### C. Not authoring concerns (per #430 — no `.agent` syntax needed)

- **`imports`** — `.agent` auto-discovers files; obsolete.
- **`state` (dsn/backend)** — machine-local; `--state` flag + built-in `.agentic/state.db` default.
- **Secrets** (`apiKeyFrom` for built-ins) — environment variables.
- **Deployment snapshots / trace DB** — `.agentic/` internal state.

## Bottom line

- **Migrating the 15 examples** is blocked by exactly four things: **Environments (3)**, **hitl (1)**,
  **workspace (1)**, and the ubiquitous **`defaults.policy` (15)** — plus dropping the now-redundant
  `providers`/`runtime: local` boilerplate. Every example's `providers: { mock: { type: mock } }` block
  is already a built-in namespace and simply deletable.
- **Full Phase-3 removal** additionally requires closing the hard blockers **mcp/http tool transport**
  and **custom providers** — otherwise entire classes of project (any MCP/HTTP tool, any aliased
  provider) would have no authoring path.

## Critical path to Phase 3 (grammar additions required) — ✅ COMPLETE

1. ✅ `tool { mcp { … } }` and `tool { http { … } }` transport blocks (#447).
2. ✅ `environment <name> { overrides { … } }` for env overlays (#448; loader fold-back fixed in #453).
3. ✅ `policy { hitl { … } }` (#449).
4. ✅ `provider <alias> { type … apiKeyFrom … workspaceIdFrom … }` for custom providers/aliases (this PR).

All four hard grammar blockers are now closed: no class of project is YAML-only for source authoring.
The remaining B/C items are individually small (add a field) or reducible to built-in defaults / env /
flags, and the softer `defaults.policy` / `workspace` gaps remain for Phase 2c (example migration).

## Suggested #440 sequencing (unchanged from the tracking issue, informed by this audit)

1. **Phase 2a — deprecation warning** on hand-authored YAML project *source* (non-breaking; independent
   of the gaps above — can land first).
2. ✅ **Phase 2b — migration path** — `terfyn migrate --to-agent` raises YAML declarative resources
   (providers/tools/policies/environments/agents) to `.agent`, dropping redundant built-in providers,
   and refuses (reports) any construct with no `.agent` form. YAML-authored **workflows are not
   raised**: `.agent` has no object-literal return for a multi-field `output.value`, and forward
   lowering always stamps explicit DAG needs where YAML uses an implicit sequential chain — so a
   workflow body is migrated by hand. (This mattered for only one example — `example1`, the sole
   pure-YAML workflow — which was hand-restructured during Phase 2c, below.)
3. ✅ **Grammar additions** (critical path 1–4) — each its own PR; the real work, now complete.
4. ✅ **Phase 2c — migrate the 15 examples + docs** — **all 15 examples are now `.agent`-only.** Each
   example's YAML tools/policies/environments were raised into its `main.agent` and `project.yaml`
   dropped; `defaults.policy`/`defaults.model` were made explicit on the resources that relied on them,
   and redundant built-in providers / `defaults.runtime: local` were dropped (all verified with a
   `terfyn export` before/after diff showing only those benign changes). `example1` — the one pure-YAML
   workflow with a multi-field `output.value` — was **restructured** so the agent emits the whole object
   and the workflow returns it (the standard `.agent` `value` envelope), preserving the data flow and
   fields. **No example depends on the object-literal-return gap**, so it does not block Phase 3; it
   remains a general `.agent` grammar limitation for hand-authored multi-field workflow outputs.
   The `spec.workspace` tool sub-block (root/testCommand) was subsequently given a `.agent` grammar
   too (#440), so it is no longer a gap; `implement-review-loop` keeps the env-var path but could now
   declare it inline. Object-literal returns — `return { a: x, b: y }` — were **also added** (#440),
   closing the multi-field-output gap that had forced `example1`'s hand-restructure. **The `.agent`
   grammar now expresses the entire resource model** with no known authoring gaps.
5. **Phase 3 — make `.agent` the sole *authoring* surface** (non-breaking, per ADR 003 §2 / ADR 005 §1).
   "Remove YAML as a project source" is a **documentation and positioning** change, **not** a loader
   change: the YAML codec/loader is *retained* as non-authoring infrastructure (interchange, `terfyn
   export` round-trip, machine-generated resources, and the internal test fixtures) exactly as ADR 003
   §2 and ADR 005 §1 commit. So Phase 3 updates `init`/`new` (already `.agent`-only), all examples
   (done in 2c), and the docs (README, `docs/EXAMPLES.md`, `docs/LANGUAGE.md`, `docs/DESIGN_DOC.md`) so a
   normal project is `.agent`-only and YAML is never generated, recommended, or required for authoring;
   the deprecation notice points to `terfyn migrate --to-agent`. Actually *removing* YAML ingestion
   would reverse ADR 003/005 and must be a **separate ADR** that explicitly supersedes them.
