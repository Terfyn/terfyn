package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Terfyn/terfyn/internal/schema"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools"
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
