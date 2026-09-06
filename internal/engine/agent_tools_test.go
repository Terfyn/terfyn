package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

// TestAdvertisedAgentTools_DeclaredOperationSchema proves the operation's DECLARED input schema
// (#204) is advertised to the model, from disk on a fresh run and from the pinned bundle on a
// resume — so the model supplies the required arguments instead of being told the tool takes {} and
// failing the call-time schema validation (#393).
func TestAdvertisedAgentTools_DeclaredOperationSchema(t *testing.T) {
	t.Parallel()
	// A distinctive schema so we can tell the declared schema from the native built-in one.
	schemaBody := `{"type":"object","required":["fileref"],"properties":{"fileref":{"type":"string"}},"additionalProperties":false}`

	agent := &spec.AgentResource{Metadata: spec.Metadata{Name: "reader"}, Spec: spec.AgentSpec{Tools: []string{"tool.ws.read_file"}}}
	graph := func() *spec.ProjectGraph {
		return &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{
			"ws": {Metadata: spec.Metadata{Name: "ws"}, Spec: spec.ToolSpec{
				Type:       "native",
				Operations: map[string]spec.ToolOperation{"read_file": {Schema: "./schemas/read_file.json"}},
			}},
		}}
	}

	// Fresh run: schema resolved from disk under ProjectRoot.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schemas", "read_file.json"), []byte(schemaBody), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Graph: graph(), ProjectRoot: root}
	defs, _, err := e.advertisedAgentTools(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || !strings.Contains(string(defs[0].Parameters), `"fileref"`) {
		t.Fatalf("fresh run did not advertise the declared operation schema: %s", defs[0].Parameters)
	}

	// Resume: schema resolved from the pinned bundle (no disk access).
	ep := &Executor{Graph: graph(), PinnedGraph: true, Schemas: map[string]string{"./schemas/read_file.json": schemaBody}}
	defs, _, err = ep.advertisedAgentTools(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || !strings.Contains(string(defs[0].Parameters), `"fileref"`) {
		t.Fatalf("resume did not advertise the pinned operation schema: %s", defs[0].Parameters)
	}
}

func TestAgentMaxIterations(t *testing.T) {
	t.Parallel()
	// No policy ceiling (0): default hard cap of 32 applies.
	if got := agentMaxIterations(nil, 0); got != spec.DefaultAgentMaxIterations {
		t.Fatalf("nil agent = %d", got)
	}
	if got := agentMaxIterations(&spec.AgentResource{}, 0); got != spec.DefaultAgentMaxIterations {
		t.Fatalf("unset = %d", got)
	}
	if got := agentMaxIterations(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{MaxIterations: 0}}}, 0); got != spec.DefaultAgentMaxIterations {
		t.Fatalf("zero = %d", got)
	}
	if got := agentMaxIterations(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{MaxIterations: 2}}}, 0); got != 2 {
		t.Fatalf("explicit = %d", got)
	}
	if got := agentMaxIterations(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{MaxIterations: 99}}}, 0); got != spec.HardAgentMaxIterations {
		t.Fatalf("default hard cap = %d", got)
	}
	// A policy ceiling raises the cap: 64 is now honored (issue #522).
	if got := agentMaxIterations(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{MaxIterations: 64}}}, 100); got != 64 {
		t.Fatalf("raised ceiling = %d, want 64", got)
	}
	if got := agentMaxIterations(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{MaxIterations: 200}}}, 100); got != 100 {
		t.Fatalf("clamp to policy ceiling = %d, want 100", got)
	}
	// A policy ceiling below the default bounds even the default.
	if got := agentMaxIterations(&spec.AgentResource{}, 4); got != 4 {
		t.Fatalf("default clamped to low policy ceiling = %d, want 4", got)
	}
}

func TestAgentTemperature(t *testing.T) {
	t.Parallel()
	if got := agentTemperature(nil); got != nil {
		t.Fatalf("nil agent = %v, want nil", *got)
	}
	if got := agentTemperature(&spec.AgentResource{}); got != nil {
		t.Fatalf("nil constraints = %v, want nil", *got)
	}
	// An unset temperature (nil) falls back to the provider default and is not sent.
	if got := agentTemperature(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{}}}); got != nil {
		t.Fatalf("unset temperature = %v, want nil", *got)
	}
	// An explicit 0 is honored (deterministic sampling), not treated as unset.
	zero := 0.0
	if got := agentTemperature(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{Temperature: &zero}}}); got == nil || *got != 0 {
		t.Fatalf("zero temperature = %v, want 0", got)
	}
	half := 0.2
	got := agentTemperature(&spec.AgentResource{Spec: spec.AgentSpec{Constraints: &spec.AgentConstraints{Temperature: &half}}})
	if got == nil || *got != 0.2 {
		t.Fatalf("explicit temperature = %v, want 0.2", got)
	}
}

func TestResolveAgentToolCall(t *testing.T) {
	t.Parallel()
	advertised := map[string]string{
		"helper": "tool.helper.default",
		"docs":   "tool.docs.default",
	}
	tests := []struct {
		name    string
		given   string
		want    string
		wantErr string
	}{
		{name: "bare tool", given: "helper", want: "tool.helper.default"},
		{name: "tool.op", given: "helper.echo", wantErr: "not declared"},
		{name: "full uses", given: "tool.helper.echo", wantErr: "not declared"},
		{name: "native shell op", given: "helper.command.run", wantErr: "not declared"},
		{name: "http method.path", given: "helper.delete.users", wantErr: "not declared"},
		{name: "tool.name only", given: "tool.helper", wantErr: "not declared"},
		{name: "undeclared", given: "ghost", wantErr: "not declared"},
		{name: "empty", given: "  ", wantErr: "missing name"},
		{name: "undeclared full uses", given: "tool.ghost.default", wantErr: "not declared"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveAgentToolCall(tc.given, advertised)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestParseToolCallArgs(t *testing.T) {
	t.Parallel()
	got, err := parseToolCallArgs(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("nil args %+v %v", got, err)
	}
	got, err = parseToolCallArgs(json.RawMessage(`{"q":"go"}`))
	if err != nil || got["q"] != "go" {
		t.Fatalf("object %+v %v", got, err)
	}
	_, err = parseToolCallArgs(json.RawMessage(`[]`))
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("array err %v", err)
	}
}

func TestEncodeToolResultContent(t *testing.T) {
	t.Parallel()
	if got := encodeToolResultContent(nil); got != "{}" {
		t.Fatalf("nil %q", got)
	}
	if got := encodeToolResultContent(map[string]any{"uses": "tool.helper.default"}); got != `{"uses":"tool.helper.default"}` {
		t.Fatalf("got %s", got)
	}
}

func TestAdvertisedAgentTools(t *testing.T) {
	t.Parallel()
	e := &Executor{Graph: &spec.ProjectGraph{
		Tools: map[string]*spec.ToolResource{
			"helper": {Metadata: spec.Metadata{Name: "helper"}, Spec: spec.ToolSpec{Type: "mock"}},
			"shell":  {Metadata: spec.Metadata{Name: "shell"}, Spec: spec.ToolSpec{Type: "native"}},
		},
	}}
	defs, uses, err := e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "reviewer"},
		Spec:     spec.AgentSpec{Tools: []string{"helper", "helper", "", "shell"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Name != "helper" || defs[1].Name != "shell" {
		t.Fatalf("defs %+v", defs)
	}
	if string(defs[0].Parameters) != string(defaultAgentToolParameters) {
		t.Fatalf("params %s", defs[0].Parameters)
	}
	if uses["helper"] != "tool.helper.default" {
		t.Fatalf("mock uses %q", uses["helper"])
	}
	if uses["shell"] != "tool.shell.echo" {
		t.Fatalf("native uses %q", uses["shell"])
	}
	_, _, err = e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "reviewer"},
		Spec:     spec.AgentSpec{Tools: []string{"ghost"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("got %v", err)
	}

	e.Graph.Tools["api"] = &spec.ToolResource{Metadata: spec.Metadata{Name: "api"}, Spec: spec.ToolSpec{Type: "http"}}
	_, _, err = e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "reviewer"},
		Spec:     spec.AgentSpec{Tools: []string{"api"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no default operation") {
		t.Fatalf("bare http tool err %v", err)
	}
	defs, uses, err = e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "reviewer"},
		Spec:     spec.AgentSpec{Tools: []string{"tool.api.get.users"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "api" || uses["api"] != "tool.api.get.users" {
		t.Fatalf("pinned http defs=%+v uses=%+v", defs, uses)
	}
	_, _, err = e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "reviewer"},
		Spec:     spec.AgentSpec{Tools: []string{"tool.api.default"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no default operation") {
		t.Fatalf("pinned http default err %v", err)
	}
	// Multiple operations on one tool advertise as distinct per-operation tool-defs (#291).
	defs, uses, err = e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "reviewer"},
		Spec:     spec.AgentSpec{Tools: []string{"shell", "tool.shell.command.run"}},
	})
	if err != nil {
		t.Fatalf("multi-op advertise err %v", err)
	}
	// The model-facing handle is sanitized to the provider name pattern (no '.'),
	// and the uses map keys by that sanitized handle (#291 + provider compat).
	if len(defs) != 2 || defs[0].Name != "shell_echo" || defs[1].Name != "shell_command_run" {
		t.Fatalf("multi-op defs %+v", defs)
	}
	if uses["shell_echo"] != "tool.shell.echo" || uses["shell_command_run"] != "tool.shell.command.run" {
		t.Fatalf("multi-op uses %+v", uses)
	}
}

// TestAgentToolCapabilityBoundary is the #291/#292 security property: an operation
// the agent did not advertise is denied at resolution, no matter what the model
// asks for. The Implementer holds three operations on one `workspace` tool; the
// Reviewer holds only read_file + run_tests. The Reviewer's attempt to call
// write_file is refused because it is outside its declared capability — the
// capability boundary is the control, not the prompt.
func TestAgentToolCapabilityBoundary(t *testing.T) {
	t.Parallel()
	e := &Executor{Graph: &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{
		"workspace": {Metadata: spec.Metadata{Name: "workspace"}, Spec: spec.ToolSpec{Type: "mock"}},
	}}}

	implementer := &spec.AgentResource{Metadata: spec.Metadata{Name: "implementer"}, Spec: spec.AgentSpec{
		Tools: []string{"tool.workspace.read_file", "tool.workspace.write_file", "tool.workspace.run_tests"},
	}}
	reviewer := &spec.AgentResource{Metadata: spec.Metadata{Name: "reviewer"}, Spec: spec.AgentSpec{
		Tools: []string{"tool.workspace.read_file", "tool.workspace.run_tests"},
	}}

	_, implUses, err := e.advertisedAgentTools(implementer)
	if err != nil {
		t.Fatalf("implementer advertise: %v", err)
	}
	// The Implementer CAN invoke write_file — it is advertised and maps to its uses.
	// The model-facing handle is the sanitized name (no '.'), not the dotted uses.
	uses, err := resolveAgentToolCall("workspace_write_file", implUses)
	if err != nil || uses != "tool.workspace.write_file" {
		t.Fatalf("implementer write_file should resolve, got uses=%q err=%v", uses, err)
	}

	_, revUses, err := e.advertisedAgentTools(reviewer)
	if err != nil {
		t.Fatalf("reviewer advertise: %v", err)
	}
	// The Reviewer read_file/run_tests resolve; write_file is DENIED (not advertised).
	if _, err := resolveAgentToolCall("workspace_read_file", revUses); err != nil {
		t.Fatalf("reviewer read_file should resolve, got %v", err)
	}
	if _, err := resolveAgentToolCall("workspace_write_file", revUses); err == nil {
		t.Fatalf("reviewer write_file MUST be denied at the capability boundary, but it resolved")
	}
	// A wholly unknown operation is likewise denied.
	if _, err := resolveAgentToolCall("workspace_delete_repo", revUses); err == nil {
		t.Fatalf("an unadvertised operation must be denied")
	}
}

func TestAdvertisedAgentTools_NativeInputSchema(t *testing.T) {
	t.Parallel()
	e := &Executor{Graph: &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{
		"workspace": {Metadata: spec.Metadata{Name: "workspace"}, Spec: spec.ToolSpec{Type: "native"}},
		"local":     {Metadata: spec.Metadata{Name: "local"}, Spec: spec.ToolSpec{Type: "mock"}},
	}}}

	// A native operation advertises its declared input schema (read_file → path), so
	// the model is told the required argument rather than an empty object.
	defs, _, err := e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "impl"},
		Spec:     spec.AgentSpec{Tools: []string{"tool.workspace.read_file"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("defs = %+v", defs)
	}
	if !strings.Contains(string(defs[0].Parameters), `"path"`) || !strings.Contains(string(defs[0].Parameters), `"required"`) {
		t.Fatalf("native read_file params did not advertise the input schema: %s", defs[0].Parameters)
	}

	// A non-native tool (no built-in schema) keeps the permissive default.
	defs, _, err = e.advertisedAgentTools(&spec.AgentResource{
		Metadata: spec.Metadata{Name: "other"},
		Spec:     spec.AgentSpec{Tools: []string{"local"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || string(defs[0].Parameters) != string(defaultAgentToolParameters) {
		t.Fatalf("non-native tool should keep the default params, got %s", defs[0].Parameters)
	}
}
