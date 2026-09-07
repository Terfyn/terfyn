package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/telemetry"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/tools/toolerr"
	"github.com/Terfyn/terfyn/internal/trace"
)

func parseAgentJSONObject(content string) (map[string]any, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("engine: empty agent response")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return nil, fmt.Errorf("engine: agent response is not a JSON object: %w", err)
	}
	return m, nil
}

// recoverAgentJSONObject recovers the single JSON object from an agent completion that may carry a
// prose preamble (a leading "Perfect…") or a ```json code fence around the object (issue #510).
// Providers do not guarantee bare JSON even for a schema-bound output, so a stray leading character
// must not fail an otherwise-successful run. But typed agent output feeds workflow state, so recovery
// must be UNAMBIGUOUS — it never guesses which of several objects is the state:
//
//   - content that already parses as JSON is returned verbatim (the clean fast path);
//   - otherwise the completion is scanned for non-overlapping, brace-balanced, string-aware {…} spans
//     that are valid JSON objects, and:
//   - exactly one   → that object is used;
//   - zero          → the trimmed content is returned unchanged, so the caller reports the original
//     parse error (recovery adds nothing to offer);
//   - two or more   → an ambiguity error, failing closed rather than shipping the leftmost span
//     (which could be an example object, a bare "{}", or a schema-satisfying decoy) as state.
//
// The #510 repro — a prose preamble or a single fenced object around one object — is exactly the
// one-object case and still recovers. This is deliberately stricter than #451's recoverable-tool
// observation: there an adapter marks an error recoverable; here we refuse to invent state.
func recoverAgentJSONObject(content string) (string, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return trimmed, nil
	}
	objs := topLevelJSONObjects(trimmed)
	switch len(objs) {
	case 0:
		return trimmed, nil
	case 1:
		return objs[0], nil
	default:
		return "", fmt.Errorf("engine: agent response is ambiguous: found %d candidate JSON objects in the completion; a schema-bound output must be a single JSON object with no other objects around it", len(objs))
	}
}

// topLevelJSONObjects returns every non-overlapping, brace-balanced {…} span in s that is valid JSON,
// left to right. Braces inside JSON string literals are ignored (a `}` in a value does not close the
// object), and each returned span is a maximal top-level region — a nested object is part of its
// parent span and is never counted separately.
func topLevelJSONObjects(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if s[i] != '{' {
			i++
			continue
		}
		span, end, ok := balancedObject(s[i:])
		if !ok {
			// No complete object starts at i (braces never balance from here); a later, independent
			// '{' may still open one, so advance by a single byte rather than giving up.
			i++
			continue
		}
		if json.Valid([]byte(span)) {
			out = append(out, span)
		}
		// Skip the whole balanced span (valid or not) so a nested object is not counted separately and
		// an invalid span's interior is not rescanned.
		i += end
	}
	return out
}

// balancedObject returns the shortest prefix of s that is a brace-balanced {…} span, treating s[0] as
// the opening '{' and ignoring braces inside JSON string literals (so a `}` in a string value does
// not close the object). end is the index just past the closing brace; ok is false when the braces
// never balance.
func balancedObject(s string) (span string, end int, ok bool) {
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], i + 1, true
			}
		}
	}
	return "", 0, false
}

func (e *Executor) runToolStep(ctx context.Context, runHandle *telemetry.RunHandle, pol policy.PolicyEvaluator, wf *spec.WorkflowResource, runID string, step spec.WorkflowStep, with map[string]any, pctx policy.RunContext, usesOverride string, withOverride map[string]any) (map[string]any, tools.ToolCallMeta, error) {
	uses := strings.TrimSpace(usesOverride)
	if uses == "" {
		uses = strings.TrimSpace(step.Uses)
	}
	withArgs := with
	if withOverride != nil {
		withArgs = withOverride
	}
	tid := e.qualID(step.ID)
	var err error
	withArgs, err = e.enforceToolInput(ctx, wf, runID, tid, uses, withArgs)
	if err != nil {
		return nil, tools.ToolCallMeta{}, err
	}
	if err := pol.CheckToolCall(ctx, policy.ToolCallContext{Run: pctx, StepID: step.ID, Uses: uses, With: withArgs}); err != nil {
		// Fail-closed: the tool is never invoked, so we skip tool_selection/tool_execution and
		// record system_error instead (same as workflow uses: steps).
		if e.Trace != nil {
			if d, ok := policy.AsDenied(err); ok {
				_, _ = e.Trace.Append(ctx, runID, tid, trace.EventSystemError, trace.ActorSystem, d.TraceData())
			}
		}
		return nil, tools.ToolCallMeta{}, err
	}
	if e.Trace != nil {
		sel := toolSelectionData(uses, withArgs)
		if e.TraceDetail {
			// The actual call arguments (issue #525) — an edit's old/new strings, a run_tests command,
			// a grep pattern — so the trace shows WHAT the call operated on, not just a digest. Redacted
			// by key and truncated by the recorder; the digest stays for auditors.
			trace.AddToolArgs(sel, withArgs)
		}
		_, _ = e.Trace.Append(ctx, runID, tid, trace.EventToolSelection, trace.ActorAgent, sel)
	}
	started := e.now()
	if e.Tools == nil {
		err := fmt.Errorf("engine: nil tool executor")
		meta := tools.ToolCallMeta{DurationMs: e.now().Sub(started).Milliseconds()}
		e.appendToolExecution(ctx, runID, tid, uses, meta, err, nil)
		return nil, meta, err
	}
	toolCtx := ctx
	var endTool func(error)
	if runHandle != nil {
		safety := e.toolSafetyForUses(uses)
		toolCtx, endTool = runHandle.StartTool(telemetry.ToolAttrs{
			RunID: runID, StepID: tid, Uses: uses,
			Trusted: safety.Trusted, SideEffects: safety.SideEffects, RequiresApproval: safety.RequiresApproval,
		})
	}
	// Bound the actual invocation by the remaining wall-clock budget, AFTER StartTool (which
	// returns its own span context) so the deadline reaches the transport — otherwise a hung
	// tool blocks the run forever despite maxWallClockSeconds (#394).
	toolCtx, cancelWC := e.wallClockDeadline(toolCtx, pol, pctx)
	defer cancelWC()
	resp, err := e.Tools.Call(toolCtx, tools.ToolCallRequest{Uses: uses, With: withArgs})
	if endTool != nil {
		endTool(err)
	}
	meta := resp.Meta
	if meta.DurationMs == 0 {
		meta.DurationMs = e.now().Sub(started).Milliseconds()
	}
	if err != nil {
		e.appendToolExecution(ctx, runID, tid, uses, meta, err, nil)
		return nil, meta, err
	}
	e.appendToolExecution(ctx, runID, tid, uses, meta, nil, resp.Output)
	out, err := e.enforceToolOutput(ctx, wf, runID, tid, uses, resp.Output)
	if err != nil {
		return nil, meta, err
	}
	if err := pol.CheckStep(ctx, policy.StepContext{StepID: step.ID, OutputIsStructured: true}); err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func (e *Executor) runAgentStep(ctx context.Context, runHandle *telemetry.RunHandle, pol policy.PolicyEvaluator, wf *spec.WorkflowResource, runID string, step spec.WorkflowStep, with map[string]any, pctx policy.RunContext, agent *spec.AgentResource) (map[string]any, models.GenerateMeta, error) {
	if agent == nil {
		return nil, models.GenerateMeta{}, fmt.Errorf("engine: nil agent resource")
	}
	modelRef := strings.TrimSpace(agent.Spec.Model)
	cli, modelID, err := e.modelClient(modelRef)
	if err != nil {
		return nil, models.GenerateMeta{}, err
	}
	sec := 0
	if agent.Spec.Constraints != nil {
		sec = agent.Spec.Constraints.TimeoutSeconds
	}
	ctx2, cancel := withSecondsTimeout(ctx, sec)
	defer cancel()
	// Also bound the model call by the remaining wall-clock budget, so an agent step without
	// constraints.timeoutSeconds still cannot hang a run past maxWallClockSeconds (#394).
	ctx2, cancelWC := e.wallClockDeadline(ctx2, pol, pctx)
	defer cancelWC()

	payload, err := json.Marshal(with)
	if err != nil {
		return nil, models.GenerateMeta{}, err
	}
	instructions := strings.TrimSpace(agent.Spec.Instructions)
	messages := []models.ChatMessage{
		{Role: "system", Content: instructions},
		{Role: "user", Content: string(payload)},
	}

	toolDefs, usesByName, err := e.advertisedAgentTools(agent)
	if err != nil {
		return nil, models.GenerateMeta{}, err
	}
	temperature := agentTemperature(agent)
	maxTokens := spec.ResolveMaxTokens(agent.Spec.Constraints)
	respFormat, err := e.agentResponseFormat(agent)
	if err != nil {
		return nil, models.GenerateMeta{}, err
	}
	if len(toolDefs) == 0 {
		return e.finishAgentTurn(ctx, ctx2, runHandle, pol, cli, modelRef, modelID, runID, step, pctx, agent, models.GenerateRequest{
			Model:          modelID,
			Messages:       messages,
			MaxTokens:      maxTokens,
			Temperature:    temperature,
			ResponseFormat: respFormat,
		})
	}
	return e.runAgentToolLoop(ctx, ctx2, runHandle, pol, wf, cli, modelRef, modelID, runID, step, pctx, agent, messages, toolDefs, usesByName, temperature, maxTokens, respFormat)
}

// maxTokensStopError is the actionable run error when a completion stops at its output-token cap
// (issue #514). Today it fails the step (a truncated write_file would be a corrupted partial file, so
// silently accepting the prefix is worse); the message names the cap and the constraint to raise.
func maxTokensStopError(agentName string, cap int) error {
	return fmt.Errorf("engine: agent %q hit its output token cap (max_tokens=%d) before completing its response; raise it with constraints { maxTokens N } on the agent", agentName, cap)
}

// agentResponseFormat returns the provider structured-output request for agent, or nil when the agent
// does not require it. It is set only when constraints.requireStructuredOutput is true, wiring that
// flag — previously a silent no-op — to provider structured outputs so the model cannot emit a
// non-conforming completion (issue #510). The schema the provider must enforce is the agent's output
// schema; requireStructuredOutput without an output schema has nothing to constrain and is a run
// error rather than a silent skip. On a pinned resume whose snapshot did not capture the schema,
// resolveSchemaContent returns nil (gradual/absent) and structured output is left off.
func (e *Executor) agentResponseFormat(agent *spec.AgentResource) (*models.ResponseFormat, error) {
	if agent == nil || agent.Spec.Constraints == nil || !agent.Spec.Constraints.RequireStructuredOutput {
		return nil, nil
	}
	sref := ""
	if agent.Spec.Output != nil {
		sref = strings.TrimSpace(agent.Spec.Output.Schema)
	}
	if sref == "" {
		return nil, fmt.Errorf("engine: agent %q sets constraints.requireStructuredOutput but declares no output schema to enforce", agent.Metadata.Name)
	}
	content, err := e.resolveSchemaContent(sref)
	if err != nil {
		return nil, fmt.Errorf("engine: agent %q structured output: %w", agent.Metadata.Name, err)
	}
	if len(content) == 0 {
		return nil, nil
	}
	return &models.ResponseFormat{Name: structuredOutputName(agent.Metadata.Name), Schema: content}, nil
}

// structuredOutputName renders an agent name as a provider-safe schema identifier (OpenAI requires
// json_schema.name to match ^[a-zA-Z0-9_-]+$). Disallowed runes collapse to '_'; an empty result
// falls back to "output".
func structuredOutputName(agentName string) string {
	var b strings.Builder
	for _, r := range agentName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		return "output"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// agentTemperature returns the sampling temperature to send for agent, or nil to leave the provider
// default. constraints.temperature is a *float64, so an explicit 0 (deterministic sampling) is
// honored and sent; only an unset constraint (nil) falls back to the provider default (issue #388).
func agentTemperature(agent *spec.AgentResource) *float64 {
	if agent == nil || agent.Spec.Constraints == nil {
		return nil
	}
	if t := agent.Spec.Constraints.Temperature; t != nil {
		v := *t
		return &v
	}
	return nil
}

func (e *Executor) runAgentToolLoop(
	ctx, ctx2 context.Context,
	runHandle *telemetry.RunHandle,
	pol policy.PolicyEvaluator,
	wf *spec.WorkflowResource,
	cli models.ModelClient,
	modelRef, modelID, runID string,
	step spec.WorkflowStep,
	pctx policy.RunContext,
	agent *spec.AgentResource,
	messages []models.ChatMessage,
	toolDefs []models.ToolDef,
	advertised map[string]string,
	temperature *float64,
	maxTokens int,
	respFormat *models.ResponseFormat,
) (map[string]any, models.GenerateMeta, error) {
	// maxIter counts Generate turns. tool_use on the last turn fails without executing those calls
	// (maxIterations: 1 is a single completion; tools never run). HITL interrupt is not consulted
	// inside this loop: inner uses must already be pre-approved (--approve / ApprovedActions) or
	// CheckToolCall fails closed (approval_required).
	maxIter := agentMaxIterations(agent, policyMaxIterationsCeiling(pol))
	var acc models.GenerateMeta
	loopPctx := pctx

	for i := 1; i <= maxIter; i++ {
		// Check already-accumulated cost (prior steps + prior turns) before the next Generate.
		if _, err := e.checkAgentLoopRun(ctx2, pol, runID, step.ID, pctx, acc); err != nil {
			return nil, acc, err
		}
		req := models.GenerateRequest{
			Model:          modelID,
			Messages:       messages,
			Tools:          toolDefs,
			ToolChoice:     models.ToolChoiceAuto,
			MaxTokens:      maxTokens,
			Temperature:    temperature,
			ResponseFormat: respFormat,
		}
		resp, err := e.generateAgentTurn(ctx, ctx2, runHandle, cli, modelRef, runID, step, req)
		if err != nil {
			return nil, acc, err
		}
		addGenerateMeta(&acc, resp.Meta)
		loopPctx, err = e.checkAgentLoopRun(ctx2, pol, runID, step.ID, pctx, acc)
		if err != nil {
			return nil, acc, err
		}

		switch resp.StopReason {
		case models.StopReasonEndTurn, "":
			// Empty stop is treated as end_turn only when the model did not request tools.
			if resp.StopReason == "" && len(resp.ToolCalls) > 0 {
				resp.StopReason = models.StopReasonToolUse
			} else {
				return e.completeAgentOutput(ctx, pol, agent, step, resp.Content, acc)
			}
		case models.StopReasonMaxTokens:
			// The completion was cut at the output cap; the content (a partial tool call or a truncated
			// output object) is unusable, so fail with an actionable message rather than the opaque
			// "stop reason ... is not end_turn or tool_use" (issue #514).
			return nil, acc, maxTokensStopError(agent.Metadata.Name, maxTokens)
		}
		if resp.StopReason != models.StopReasonToolUse {
			return nil, acc, fmt.Errorf("engine: agent %q stop reason %q is not end_turn or tool_use", agent.Metadata.Name, resp.StopReason)
		}
		if len(resp.ToolCalls) == 0 {
			return nil, acc, fmt.Errorf("engine: agent %q returned tool_use without tool calls", agent.Metadata.Name)
		}
		if i == maxIter {
			if e.Trace != nil {
				_, _ = e.Trace.Append(ctx, runID, step.ID, trace.EventLimitHit, trace.ActorSystem, map[string]any{
					"kind":        "max_iterations",
					"max":         maxIter,
					"iterations":  i,
					"stepId":      step.ID,
					"agent":       step.Agent,
					"stopReason":  resp.StopReason,
					"toolCallIds": toolCallIDs(resp.ToolCalls),
				})
			}
			// Graceful finalization (issue #518): the agent wants more tool calls but its iteration
			// budget is spent. Rather than failing the run, force one final tool-free completion so it
			// returns its best output from what it has already gathered. The pending tool_use is
			// intentionally NOT executed — the budget is spent — and the run continues with the result.
			return e.finalizeAgentAtCap(ctx, ctx2, runHandle, pol, cli, modelRef, modelID, runID, step, pctx, agent, messages, temperature, maxTokens, respFormat, acc)
		}

		results := make([]models.ToolResult, 0, len(resp.ToolCalls))
		for _, call := range resp.ToolCalls {
			content, isErr, tmeta, fatalErr := e.runAgentToolCall(ctx2, runHandle, pol, wf, runID, step, pctx, loopPctx, acc, advertised, call)
			if fatalErr != nil {
				return nil, acc, fatalErr
			}
			addToolMeta(&acc, tmeta)
			var err error
			loopPctx, err = e.checkAgentLoopRun(ctx2, pol, runID, step.ID, pctx, acc)
			if err != nil {
				return nil, acc, err
			}
			results = append(results, models.ToolResult{ToolCallID: call.ID, Content: content, IsError: isErr})
		}
		messages = append(messages,
			models.ChatMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls},
			models.ChatMessage{Role: "user", ToolResults: results},
		)
	}
	return nil, acc, fmt.Errorf("engine: agent %q reached maxIterations (%d)", agent.Metadata.Name, maxIter)
}

// maxIterationsFinalizePrompt is the user turn appended when an agent hits its iteration budget: it
// tells the agent to stop calling tools and return its output now, from what it has gathered (#518).
const maxIterationsFinalizePrompt = "You have reached your tool-use iteration budget. Do not call any more tools. Using everything you have gathered so far, provide your final answer now in the required output format."

// finalizeAgentAtCap runs one final, tool-free completion after an agent reaches maxIterations, so the
// run degrades gracefully to the agent's best output instead of dying at the cap (issue #518). It
// appends a finalize instruction and generates WITHOUT tools (tool use disabled), keeping the same
// temperature, max_tokens, and structured-output schema as the loop; the accumulated loop cost is
// carried into the returned meta. If the forced turn still fails to produce a valid output (a
// max_tokens stop, or output that does not parse/validate), that error propagates — the same spirit
// as #514/#451, where a limit is a signal to wrap up, but an unusable result still fails.
func (e *Executor) finalizeAgentAtCap(
	ctx, ctx2 context.Context,
	runHandle *telemetry.RunHandle,
	pol policy.PolicyEvaluator,
	cli models.ModelClient,
	modelRef, modelID, runID string,
	step spec.WorkflowStep,
	pctx policy.RunContext,
	agent *spec.AgentResource,
	messages []models.ChatMessage,
	temperature *float64,
	maxTokens int,
	respFormat *models.ResponseFormat,
	acc models.GenerateMeta,
) (map[string]any, models.GenerateMeta, error) {
	final := make([]models.ChatMessage, len(messages), len(messages)+1)
	copy(final, messages)
	final = append(final, models.ChatMessage{Role: "user", Content: maxIterationsFinalizePrompt})
	// No Tools and no ToolChoice: a plain, tool-free completion the agent must answer directly.
	req := models.GenerateRequest{
		Model:          modelID,
		Messages:       final,
		MaxTokens:      maxTokens,
		Temperature:    temperature,
		ResponseFormat: respFormat,
	}
	resp, err := e.generateAgentTurn(ctx, ctx2, runHandle, cli, modelRef, runID, step, req)
	if err != nil {
		return nil, acc, err
	}
	addGenerateMeta(&acc, resp.Meta)
	if _, err := e.checkAgentLoopRun(ctx2, pol, runID, step.ID, pctx, acc); err != nil {
		return nil, acc, err
	}
	if resp.StopReason == models.StopReasonMaxTokens {
		return nil, acc, maxTokensStopError(agent.Metadata.Name, maxTokens)
	}
	return e.completeAgentOutput(ctx, pol, agent, step, resp.Content, acc)
}

// runAgentToolCall executes one tool call from the agent loop. The split between fatal and
// recoverable is fixed and fail-closed (issue #451):
//
//   - PRE-execution errors are always fatal: an unadvertised tool name (ADR 002 capability boundary)
//     and arguments that are not a JSON object are broken calls, not observations.
//   - a policy/capability denial and a run-budget/cancel/timeout breach are always fatal.
//   - the tool EXECUTION result is classified by toolErrorIsRecoverable: an error is handed back to
//     the agent as an {"error": …} OBSERVATION only if the adapter deliberately marked it recoverable
//     (toolerr.ErrRecoverable — e.g. a read_file MISS on a path that does not exist). Any UNMARKED
//     execution error — a transport/config failure, a sandbox-escape rejection, a schema/limit
//     rejection, a nil executor — is fatal, so an adapter diagnostic never becomes prompt text.
//
// The returned content is the tool-result payload for the model (a model-safe {"error": …} object on
// a recoverable miss); isErr is true for that recoverable-miss case so the loop marks the tool_result
// is_error (#524); tmeta is the step metadata to fold into the loop's cost.
func (e *Executor) runAgentToolCall(
	ctx2 context.Context,
	runHandle *telemetry.RunHandle,
	pol policy.PolicyEvaluator,
	wf *spec.WorkflowResource,
	runID string,
	step spec.WorkflowStep,
	pctx, loopPctx policy.RunContext,
	acc models.GenerateMeta,
	advertised map[string]string,
	call models.ToolCall,
) (content string, isErr bool, tmeta tools.ToolCallMeta, fatalErr error) {
	// Resolving the call name and parsing its arguments are PRE-execution and always fatal: an
	// unadvertised tool name is a capability-boundary violation (ADR 002 — no operation is
	// agent-callable unless advertised) and arguments that are not a JSON object are a broken call.
	// Only the tool EXECUTION below (runToolStep) is classified by toolErrorIsRecoverable, and even
	// there the default is fatal — an execution error is an observation only if an adapter marked it.
	uses, err := resolveAgentToolCall(call.Name, advertised)
	if err != nil {
		return "", false, tools.ToolCallMeta{}, err
	}
	args, err := parseToolCallArgs(call.Arguments)
	if err != nil {
		return "", false, tools.ToolCallMeta{}, fmt.Errorf("engine: tool call %q: %w", call.Name, err)
	}
	if _, err := e.checkAgentLoopRun(ctx2, pol, runID, step.ID, pctx, acc); err != nil {
		return "", false, tools.ToolCallMeta{}, err // run-level budget breach — fatal
	}
	out, tmeta, err := e.runToolStep(ctx2, runHandle, pol, wf, runID, step, args, loopPctx, uses, args)
	if err != nil {
		if toolErrorIsRecoverable(err) {
			// A recoverable failure is answered as a well-formed is_error tool_result so the model
			// reads it as an error observation to correct from, never a successful output (#524).
			return recoverableToolObservation(err), true, tmeta, nil
		}
		return "", false, tmeta, err
	}
	return encodeToolResultContent(out), false, tmeta, nil
}

// maxRecoverableObservationBytes bounds the observation text handed back to the agent so a
// pathological tool error cannot balloon the next prompt.
const maxRecoverableObservationBytes = 4 << 10

// recoverableToolObservation renders a recoverable tool error as the {"error": …} tool result an
// agent receives, so it can correct course (try a neighbor path, narrow down) instead of the run
// dying on a miss (issue #451). The text is the adapter-CONSTRUCTED, model-safe observation — never
// the raw Error(), which the trace layer redacts precisely because it can embed URLs, bodies, or
// secrets. It is bounded on a UTF-8 rune boundary (a byte cut can split a rune). The tool_execution
// trace event for the failed call is still emitted (success=false, redacted reason) by runToolStep,
// so the audit record is intact.
func recoverableToolObservation(err error) string {
	msg, _ := toolerr.SafeObservation(err)
	if msg == "" {
		msg = "tool call failed"
	}
	if len(msg) > maxRecoverableObservationBytes {
		b := maxRecoverableObservationBytes
		for b > 0 && !utf8.RuneStart(msg[b]) {
			b--
		}
		msg = msg[:b] + "…"
	}
	return encodeToolResultContent(map[string]any{"error": msg})
}

// toolErrorIsRecoverable reports whether a tool-EXECUTION error may be delivered to the agent as a
// recoverable observation instead of aborting the run. It is FAIL-CLOSED: an error is recoverable
// only when an adapter deliberately marked it (toolerr.ErrRecoverable) AND it is not a denial or a
// cancel/timeout (those are always fatal, even if somehow also marked). Everything unmarked — a
// transport/config failure, a sandbox escape, a schema/limit rejection, a nil executor, a new
// adapter's error type — stays fatal, so an adapter diagnostic never silently reaches the model.
func toolErrorIsRecoverable(err error) bool {
	if err == nil {
		return false
	}
	if _, denied := policy.AsDenied(err); denied {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, toolerr.ErrRecoverable)
}

func (e *Executor) finishAgentTurn(
	ctx, ctx2 context.Context,
	runHandle *telemetry.RunHandle,
	pol policy.PolicyEvaluator,
	cli models.ModelClient,
	modelRef, modelID, runID string,
	step spec.WorkflowStep,
	pctx policy.RunContext,
	agent *spec.AgentResource,
	req models.GenerateRequest,
) (map[string]any, models.GenerateMeta, error) {
	resp, err := e.generateAgentTurn(ctx, ctx2, runHandle, cli, modelRef, runID, step, req)
	if err != nil {
		return nil, models.GenerateMeta{}, err
	}
	if _, err := e.checkAgentLoopRun(ctx2, pol, runID, step.ID, pctx, resp.Meta); err != nil {
		return nil, resp.Meta, err
	}
	// A single-completion agent that stops at the output cap produces a truncated (unparseable) output
	// object; report the cap and the constraint to raise instead of a confusing JSON parse error (#514).
	if resp.StopReason == models.StopReasonMaxTokens {
		return nil, resp.Meta, maxTokensStopError(agent.Metadata.Name, req.MaxTokens)
	}
	return e.completeAgentOutput(ctx, pol, agent, step, resp.Content, resp.Meta)
}

func (e *Executor) generateAgentTurn(
	ctx, ctx2 context.Context,
	runHandle *telemetry.RunHandle,
	cli models.ModelClient,
	modelRef, runID string,
	step spec.WorkflowStep,
	req models.GenerateRequest,
) (models.GenerateResponse, error) {
	var resp models.GenerateResponse
	err := withAgentRetry(ctx2, func() error {
		callCtx := ctx2
		var endModel func(error)
		if runHandle != nil {
			callCtx, endModel = runHandle.StartModel(telemetry.ModelAttrs{
				RunID: runID, StepID: step.ID, AgentName: step.Agent, ModelRef: modelRef,
			})
		}
		r, genErr := cli.Generate(callCtx, req)
		if endModel != nil {
			endModel(genErr)
		}
		if genErr != nil {
			return genErr
		}
		resp = r
		return nil
	})
	if err != nil {
		return models.GenerateResponse{}, err
	}
	if e.Trace != nil {
		data := trace.LLMCompletionData(step.Agent, modelRef, resp.Meta.CostUSD)
		if e.TraceDetail {
			// The model's own narration of what it is about to do (issue #525). Redacted+truncated by
			// the recorder; the "text" key is absent on an empty (pure tool_use) completion.
			trace.AddCompletionText(data, resp.Content)
		}
		_, _ = e.Trace.Append(ctx, runID, step.ID, trace.EventLLMCompletion, trace.ActorAgent, data)
	}
	return resp, nil
}

func (e *Executor) checkAgentLoopRun(ctx context.Context, pol policy.PolicyEvaluator, runID, stepID string, base policy.RunContext, acc models.GenerateMeta) (policy.RunContext, error) {
	loop := base
	loop.AccumulatedCostUSD = base.AccumulatedCostUSD + acc.CostUSD
	if !base.StartedAt.IsZero() {
		loop.Elapsed = e.now().Sub(base.StartedAt)
	}
	if pol == nil {
		return loop, nil
	}
	if err := pol.CheckRun(ctx, loop); err != nil {
		if e.Trace != nil {
			if d, ok := policy.AsDenied(err); ok {
				_, _ = e.Trace.Append(ctx, runID, stepID, trace.EventSystemError, trace.ActorSystem, d.TraceData())
			}
		}
		e.appendCostLimitHit(ctx, runID, stepID, err)
		return loop, err
	}
	return loop, nil
}

func (e *Executor) appendCostLimitHit(ctx context.Context, runID, stepID string, err error) {
	if e == nil || e.Trace == nil {
		return
	}
	d, ok := policy.AsDenied(err)
	if !ok || d.Reason != policy.ReasonMaxCost {
		return
	}
	_, _ = e.Trace.Append(ctx, runID, stepID, trace.EventLimitHit, trace.ActorSystem, costLimitHitData(d, stepID))
}

// costLimitHitData is the limit_hit payload for execution.maxTotalCostUsd (issue #163).
// This is not [trace.LimitHitTraceData], which is byte-oriented (issue #117).
func costLimitHitData(d *policy.DeniedError, stepID string) map[string]any {
	var extra map[string]any
	if d != nil {
		extra = d.Extra
	}
	return trace.LimitHitData("max_cost", stepID, extra)
}

func (e *Executor) completeAgentOutput(ctx context.Context, pol policy.PolicyEvaluator, agent *spec.AgentResource, step spec.WorkflowStep, content string, meta models.GenerateMeta) (map[string]any, models.GenerateMeta, error) {
	// Recover the single JSON object from any prose preamble / code fence before validating and
	// parsing, so both operate on the same bytes and a stray leading character does not fail the run;
	// an ambiguous completion (two+ candidate objects) fails closed rather than guessing (issue #510).
	content, err := recoverAgentJSONObject(content)
	if err != nil {
		return nil, meta, err
	}
	// Validates against the pinned schema bundle on resume, or the on-disk schema on a fresh run.
	if err := e.validateAgentOutputSchema(agent, content); err != nil {
		return nil, meta, err
	}
	out, err := parseAgentJSONObject(content)
	if err != nil {
		return nil, meta, err
	}
	if err := pol.CheckStep(ctx, policy.StepContext{StepID: step.ID, OutputIsStructured: true}); err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func toolCallIDs(calls []models.ToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if id := strings.TrimSpace(c.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func (e *Executor) appendToolExecution(ctx context.Context, runID, stepID, uses string, meta tools.ToolCallMeta, callErr error, output map[string]any) {
	if e == nil || e.Trace == nil {
		return
	}
	data := toolExecutionData(uses, meta, callErr)
	if e.TraceDetail && callErr == nil && output != nil {
		// The tool's result (issue #525) — run_tests pass/fail + output tail, a grep hit count, a
		// read_file head — so a checkmark is backed by what actually came back. Redacted+truncated by
		// the recorder; a failed call still records only the stable redacted reason, never raw output.
		trace.AddToolOutput(data, output)
	}
	_, _ = e.Trace.Append(ctx, runID, stepID, trace.EventToolExecution, trace.ActorAgent, data)
}

// toolSelectionData / toolExecutionData / argumentsDigest delegate to the shared trace builders
// (issue #341) so the internal loop and the external runtime emit structurally identical events.
func toolSelectionData(uses string, args map[string]any) map[string]any {
	return trace.ToolSelectionData(uses, toolNameFromUses(uses), args)
}

func toolExecutionData(uses string, meta tools.ToolCallMeta, callErr error) map[string]any {
	return trace.ToolExecutionData(uses, toolNameFromUses(uses), meta.DurationMs, meta.CostUSD, callErr != nil)
}

func argumentsDigest(args map[string]any) string { return trace.ArgumentsDigest(args) }

// toolCallFailedReason is the redacted tool_execution error value (see trace.ToolCallFailedReason).
const toolCallFailedReason = trace.ToolCallFailedReason

func toolNameFromUses(uses string) string {
	name, _, err := tools.ParseUses(uses)
	if err != nil {
		return ""
	}
	return name
}
