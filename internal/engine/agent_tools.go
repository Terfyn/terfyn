package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Terfyn/terfyn/internal/jsonnum"
	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/schema"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools"
	"github.com/Terfyn/terfyn/internal/tools/native"
)

var defaultAgentToolParameters = json.RawMessage(`{"type":"object","properties":{}}`)

// operationInputSchemaJSON returns the raw JSON Schema declared for a tool operation's input (#204),
// resolved from the pinned schema bundle on a resume or the on-disk file on a fresh run — the same
// sources validateToolInputSchema enforces against. An empty ref, an unresolved pinned schema, or an
// unreadable file returns (nil,false) so the caller falls back to a more permissive advertisement.
func (e *Executor) operationInputSchemaJSON(sref string) (json.RawMessage, bool) {
	sref = strings.TrimSpace(sref)
	if e == nil || sref == "" {
		return nil, false
	}
	if e.PinnedGraph {
		content, ok := e.Schemas[sref]
		if !ok || strings.TrimSpace(content) == "" {
			return nil, false
		}
		return json.RawMessage(content), true
	}
	path, err := schema.ResolveSchemaPath(e.ProjectRoot, sref)
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return json.RawMessage(b), true
}

func (e *Executor) advertisedAgentTools(agent *spec.AgentResource) (defs []models.ToolDef, usesByName map[string]string, err error) {
	if agent == nil || len(agent.Spec.Tools) == 0 {
		return nil, nil, nil
	}
	if e == nil || e.Graph == nil {
		return nil, nil, fmt.Errorf("engine: agent %q declares tools but the project graph has none", agent.Metadata.Name)
	}
	advertised, err := spec.ResolveAgentAdvertisedTools(agent, e.Graph.Tools)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: %w", err)
	}
	defs = make([]models.ToolDef, 0, len(advertised))
	usesByName = make(map[string]string, len(advertised))
	for _, item := range advertised {
		// ResolveAgentAdvertisedTools owns provider-safe naming and rejects collisions,
		// so this map cannot silently replace one granted operation with another.
		usesByName[item.Name] = item.Uses
		desc := "Project tool " + item.Name
		params := defaultAgentToolParameters
		// Resolve the backing Tool by the name parsed from the uses string (not the
		// tool-def name) to keep the type in the description and advertise the operation's
		// input schema so the model passes the required arguments. The operation's DECLARED
		// schema (#204) takes precedence: it is exactly what enforceToolInput validates
		// against at call time, so advertising anything else (or an empty {} — #393) tells
		// the model the tool takes no arguments and the call fails closed on a `required`
		// property. Native tools without a declared schema fall back to their built-in
		// fixed argument shape.
		if toolName, operation, perr := tools.ParseUses(item.Uses); perr == nil {
			if tr := e.Graph.Tools[toolName]; tr != nil {
				if typ := strings.TrimSpace(tr.Spec.Type); typ != "" {
					desc = "Project tool " + item.Name + " (" + typ + ")"
					declared := false
					if op, ok := tr.Spec.Operations[operation]; ok {
						if sj, ok := e.operationInputSchemaJSON(op.Schema); ok {
							params = sj
							declared = true
						}
					}
					if !declared && strings.EqualFold(typ, "native") {
						if sc, ok := native.OperationInputSchema(operation); ok {
							params = sc
						}
					}
				}
			}
		}
		defs = append(defs, models.ToolDef{
			Name:        item.Name,
			Description: desc,
			Parameters:  params,
		})
	}
	return defs, usesByName, nil
}

// resolveAgentToolCall maps a model tool name onto the single advertised uses string.
// Only the ToolDef name given to the model is accepted — not an arbitrary operation on
// that tool (ADR 002: no operation is agent-callable unless it was advertised).
func resolveAgentToolCall(name string, advertised map[string]string) (uses string, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("engine: tool call missing name")
	}
	uses, ok := advertised[name]
	if !ok || uses == "" {
		return "", fmt.Errorf("engine: tool %q is not declared on the agent", name)
	}
	return uses, nil
}

func parseToolCallArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := jsonnum.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("engine: tool call arguments are not JSON: %w", err)
	}
	if v == nil {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("engine: tool call arguments must be a JSON object")
	}
	return m, nil
}

func encodeToolResultContent(out map[string]any) string {
	if out == nil {
		return "{}"
	}
	b, err := json.Marshal(out)
	if err != nil {
		return fmt.Sprintf("%v", out)
	}
	return string(b)
}

func addGenerateMeta(dst *models.GenerateMeta, src models.GenerateMeta) {
	if dst == nil {
		return
	}
	dst.DurationMs += src.DurationMs
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.CostUSD += src.CostUSD
}

func addToolMeta(dst *models.GenerateMeta, src tools.ToolCallMeta) {
	if dst == nil {
		return
	}
	dst.DurationMs += src.DurationMs
	dst.CostUSD += src.CostUSD
}
