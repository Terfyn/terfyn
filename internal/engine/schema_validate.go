package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/Terfyn/terfyn/internal/schema"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/trace"
)

// validateAgainstSchema validates instance against the schema referenced by sref. On a pinned
// resume (issue #207 follow-up) it uses the schema content captured in the run's deployment snapshot
// (e.Schemas), so it never re-reads a possibly-changed file — a schema absent from the bundle was
// not captured at run start and is treated as gradual (allowed). Otherwise it resolves and reads the
// schema file under ProjectRoot as before.
func (e *Executor) validateAgainstSchema(sref string, instance []byte) error {
	sref = strings.TrimSpace(sref)
	if sref == "" {
		return nil
	}
	if e.PinnedGraph {
		content, ok := e.Schemas[sref]
		if !ok {
			return nil
		}
		return schema.ValidateContent(sref, []byte(content), instance)
	}
	path, err := schema.ResolveSchemaPath(e.ProjectRoot, sref)
	if err != nil {
		return err
	}
	return schema.Validate(path, instance)
}

// resolveSchemaContent returns the raw JSON Schema bytes for sref, mirroring the source-selection of
// [Executor.validateAgainstSchema]: on a pinned resume it reads the content captured in the run's
// deployment snapshot (never a possibly-changed file); otherwise it reads the schema file under
// ProjectRoot. It returns (nil, nil) when sref is empty, or when a pinned run has no captured content
// for sref (treated as gradual/absent, exactly like validation). Used to hand the provider a
// structured-output schema (issue #510).
func (e *Executor) resolveSchemaContent(sref string) ([]byte, error) {
	sref = strings.TrimSpace(sref)
	if sref == "" {
		return nil, nil
	}
	if e.PinnedGraph {
		content, ok := e.Schemas[sref]
		if !ok {
			return nil, nil
		}
		return []byte(content), nil
	}
	path, err := schema.ResolveSchemaPath(e.ProjectRoot, sref)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// validateWorkflowInputSchema validates a workflow's input against its declared input schema,
// choosing the pinned bundle or the on-disk file per [Executor.validateAgainstSchema].
func (e *Executor) validateWorkflowInputSchema(wf *spec.WorkflowResource, input map[string]any) error {
	if wf == nil || wf.Spec.Input == nil {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("engine: marshal workflow input: %w", err)
	}
	if err := e.validateAgainstSchema(wf.Spec.Input.Schema, raw); err != nil {
		return fmt.Errorf("engine: workflow input: %w", err)
	}
	return nil
}

// validateAgentOutputSchema validates an agent's output against its declared output schema.
func (e *Executor) validateAgentOutputSchema(agent *spec.AgentResource, content string) error {
	if agent == nil || agent.Spec.Output == nil {
		return nil
	}
	if err := e.validateAgainstSchema(agent.Spec.Output.Schema, []byte(strings.TrimSpace(content))); err != nil {
		return fmt.Errorf("engine: agent output: %w", err)
	}
	return nil
}

// validateToolInputSchema validates a tool call's input against the operation's declared input
// schema, completing the #204 manifest's "operation → schema" half. Absent schema means gradual
// (any input). Uses the pinned schema bundle on resume, the on-disk schema on a fresh run.
func (e *Executor) validateToolInputSchema(uses string, with map[string]any) error {
	if e == nil || e.Graph == nil {
		return nil
	}
	toolName, operation, err := tools.ParseUses(uses)
	if err != nil {
		return nil // malformed uses is handled by the registry/policy; not this concern.
	}
	tr := e.Graph.Tools[toolName]
	if tr == nil {
		return nil
	}
	op, ok := tr.Spec.Operations[operation]
	if !ok || strings.TrimSpace(op.Schema) == "" {
		return nil
	}
	raw, err := json.Marshal(with)
	if err != nil {
		return fmt.Errorf("engine: marshal tool input: %w", err)
	}
	if err := e.validateAgainstSchema(op.Schema, raw); err != nil {
		return fmt.Errorf("engine: tool %q input: %w", uses, err)
	}
	return nil
}

// restoreReadOnlyAgentOutput copies JSON Schema readOnly properties from the agent's prior
// input onto its parsed output (issue #533). Schema validation only checks structure, so a
// model can emit a schema-valid placeholder for a field the author marked as identity
// ("preserve verbatim"). The engine, not the prompt, keeps those fields invariant: it
// overwrites a mutation with the prior value and records a system_error so the rewrite is
// diagnosable instead of silent. Fields absent from prior input are left as the agent
// emitted them (nothing to restore). Gradual/untyped agents (no output schema) are a no-op.
//
// prior is the agent's whole input document (already normalized by execir when the
// call was a positional .agent argument). This helper does not guess call shape
// from a user-visible "arg0" key. A declared output schema that cannot be read
// or parsed is an error so identity enforcement cannot silently no-op.
func (e *Executor) restoreReadOnlyAgentOutput(ctx context.Context, runID string, step spec.WorkflowStep, agent *spec.AgentResource, prior, out map[string]any) (map[string]any, error) {
	if e == nil || agent == nil || agent.Spec.Output == nil || out == nil {
		return out, nil
	}
	raw, err := e.resolveSchemaContent(agent.Spec.Output.Schema)
	if err != nil {
		return out, fmt.Errorf("engine: agent %q output schema: %w", agent.Metadata.Name, err)
	}
	if len(raw) == 0 {
		return out, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out, fmt.Errorf("engine: agent %q output schema: %w", agent.Metadata.Name, err)
	}
	names := schema.ReadOnlyPropertyNames(doc)
	if len(names) == 0 {
		return out, nil
	}
	restored := restoreReadOnlyFields(out, prior, names)
	if len(restored) == 0 || e.Trace == nil {
		return out, nil
	}
	_, _ = e.Trace.Append(ctx, runID, step.ID, trace.EventSystemError, trace.ActorSystem, map[string]any{
		"reason": "immutable_field_restored",
		"fields": restored,
		"agent":  agent.Metadata.Name,
		"stepId": step.ID,
	})
	return out, nil
}

// enforceReadOnlyOutput restores identity fields then re-validates the result
// against the output schema so a restored value cannot bypass the output contract
// (prior input is not assumed to be output-schema-valid).
func (e *Executor) enforceReadOnlyOutput(ctx context.Context, runID string, step spec.WorkflowStep, agent *spec.AgentResource, prior, out map[string]any) (map[string]any, error) {
	out, err := e.restoreReadOnlyAgentOutput(ctx, runID, step, agent, prior, out)
	if err != nil {
		return out, err
	}
	if agent == nil || agent.Spec.Output == nil || out == nil {
		return out, nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, fmt.Errorf("engine: marshal restored agent output: %w", err)
	}
	if err := e.validateAgentOutputSchema(agent, string(raw)); err != nil {
		return out, fmt.Errorf("engine: agent output after restoring readOnly fields: %w", err)
	}
	return out, nil
}

// restoreReadOnlyFields copies prior[name] onto out[name] for each name whose JSON value
// differs. It mutates out and returns the names it restored, sorted as given.
func restoreReadOnlyFields(out, prior map[string]any, names []string) []string {
	if out == nil || prior == nil {
		return nil
	}
	var restored []string
	for _, name := range names {
		want, ok := prior[name]
		if !ok {
			continue
		}
		got, exists := out[name]
		if exists && jsonValuesEqual(got, want) {
			continue
		}
		out[name] = want
		restored = append(restored, name)
	}
	return restored
}

// agentInputDocument is the object the agent is asked to transform. Call shape
// is normalized in execir (InvokeAgent.WholeDocument) before InvokeAgent; this
// is identity so a YAML with: {arg0: ...} is not rewritten.
func agentInputDocument(with map[string]any) map[string]any {
	return with
}

func jsonValuesEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}
