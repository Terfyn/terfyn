package engine

import (
	"encoding/json"
	"fmt"

	"github.com/Terfyn/terfyn/internal/schema"
	"github.com/Terfyn/terfyn/internal/spec"
)

func lookupWorkflow(g *spec.ProjectGraph, name string) (*spec.WorkflowResource, error) {
	if g == nil || g.Workflows == nil {
		return nil, fmt.Errorf("engine: unknown workflow %q", name)
	}
	wf, ok := g.Workflows[name]
	if !ok || wf == nil {
		return nil, fmt.Errorf("engine: unknown workflow %q", name)
	}
	return wf, nil
}

// ValidateWorkflowInput validates input against the workflow's input.schema when configured.
func ValidateWorkflowInput(projectRoot string, wf *spec.WorkflowResource, input map[string]any) error {
	return validateWorkflowInput(projectRoot, wf, input)
}

func validateWorkflowInput(projectRoot string, wf *spec.WorkflowResource, input map[string]any) error {
	if wf == nil || wf.Spec.Input == nil {
		return nil
	}
	sref := wf.Spec.Input.Schema
	if sref == "" {
		return nil
	}
	path, err := schema.ResolveSchemaPath(projectRoot, sref)
	if err != nil {
		return fmt.Errorf("engine: workflow input schema: %w", err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("engine: marshal workflow input: %w", err)
	}
	if err := schema.Validate(path, raw); err != nil {
		return fmt.Errorf("engine: workflow input: %w", err)
	}
	return nil
}
