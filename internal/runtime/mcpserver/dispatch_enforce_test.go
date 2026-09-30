package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/tools"
)

// enforceGraph builds a trusted mock tool `ws` with a read_file operation whose input schema and
// byte limits are set per the test, plus the on-disk schema file. Returns the graph and project root.
func enforceGraph(t *testing.T, limits *spec.ExecutionLimits) (*spec.ProjectGraph, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	schemaBody := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["path"],"additionalProperties":false,"properties":{"path":{"type":"string"}}}`
	if err := os.WriteFile(filepath.Join(root, "schemas", "read_file.json"), []byte(schemaBody), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &spec.ToolResource{
		APIVersion: spec.APIVersionV0, Kind: spec.KindTool, Metadata: spec.Metadata{Name: "ws"},
		Spec: spec.ToolSpec{
			Type:       "mock",
			Operations: map[string]spec.ToolOperation{"read_file": {Effects: []string{"example.effect"}, Schema: "./schemas/read_file.json"}},
			Safety:     &spec.ToolSafety{Trusted: spec.BoolPtr(true), SideEffects: spec.BoolPtr(false), RequiresApproval: spec.BoolPtr(false)},
			Limits:     limits,
		},
	}
	return &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{"ws": tool}}, root
}

// TestPolicyDispatcher_enforcesInputSchema proves the external-runtime dispatch path validates the
// operation input schema and fails closed on a violation, matching the engine (#390/#204). Without
// the fix a schema-violating call reached the tool and returned isError:false.
func TestPolicyDispatcher_enforcesInputSchema(t *testing.T) {
	g, root := enforceGraph(t, nil)
	reg := tools.NewRegistryWithRoot(g, root)
	d := NewPolicyDispatcher(policy.NewEvaluator(g, nil), reg, policy.RunContext{}).WithEnforcement(reg)

	// Schema forbids additional properties, so `bogus` must be rejected before dispatch.
	_, err := d.Call(context.Background(), "tool.ws.read_file", map[string]any{"bogus": 1, "path": "x"})
	if err == nil {
		t.Fatal("schema-violating tool call must be rejected on the external path")
	}
	if !strings.Contains(err.Error(), "input") {
		t.Fatalf("expected a schema input error, got %v", err)
	}

	// A schema-valid call still succeeds.
	if _, err := d.Call(context.Background(), "tool.ws.read_file", map[string]any{"path": "x"}); err != nil {
		t.Fatalf("schema-valid call must pass: %v", err)
	}
}

// TestPolicyDispatcher_enforcesInputByteLimit proves an oversized input under a `fail` policy is
// rejected on the external path (#390/#117), where before no limit was applied at all.
func TestPolicyDispatcher_enforcesInputByteLimit(t *testing.T) {
	g, root := enforceGraph(t, &spec.ExecutionLimits{MaxToolInputBytes: 32, ToolInputExceedPolicy: spec.LimitExceedFail})
	reg := tools.NewRegistryWithRoot(g, root)
	d := NewPolicyDispatcher(policy.NewEvaluator(g, nil), reg, policy.RunContext{}).WithEnforcement(reg)

	big := map[string]any{"path": strings.Repeat("a", 500)}
	_, err := d.Call(context.Background(), "tool.ws.read_file", big)
	if err == nil {
		t.Fatal("oversized tool input must be rejected under the fail policy on the external path")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected a byte-limit error, got %v", err)
	}
}

// TestPolicyDispatcher_enforcesOutputByteLimit proves an oversized tool OUTPUT is rejected under a
// `fail` policy on the external path (#390/#117).
func TestPolicyDispatcher_enforcesOutputByteLimit(t *testing.T) {
	g, root := enforceGraph(t, &spec.ExecutionLimits{MaxToolOutputBytes: 16, ToolOutputExceedPolicy: spec.LimitExceedFail})
	reg := tools.NewRegistryWithRoot(g, root)
	// A mock executor that returns a large output.
	reg.Mock = &bigOutputExecutor{}
	d := NewPolicyDispatcher(policy.NewEvaluator(g, nil), reg, policy.RunContext{}).WithEnforcement(reg)

	_, err := d.Call(context.Background(), "tool.ws.read_file", map[string]any{"path": "x"})
	if err == nil {
		t.Fatal("oversized tool output must be rejected under the fail policy")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected a byte-limit error, got %v", err)
	}
}

// TestPolicyDispatcher_noEnforcerSkips documents that a dispatcher built without an enforcer does
// NOT enforce (unit-test convenience). Production must never rely on this: RunExternalAgent fails
// closed when its executor is not a ToolEnforcer (see agentcli.external_run). Here a schema-violating
// call passes because enforcement was deliberately not wired.
func TestPolicyDispatcher_noEnforcerSkips(t *testing.T) {
	g, root := enforceGraph(t, nil)
	reg := tools.NewRegistryWithRoot(g, root)
	d := NewPolicyDispatcher(policy.NewEvaluator(g, nil), reg, policy.RunContext{}) // no WithEnforcement
	if _, err := d.Call(context.Background(), "tool.ws.read_file", map[string]any{"bogus": 1, "path": "x"}); err != nil {
		t.Fatalf("without an enforcer the dispatcher must not enforce schema: %v", err)
	}
}

// TestPolicyDispatcher_propagatesOutputBudget proves the external path tells the tool the output
// limit it will enforce (the resolved per-tool maxToolOutputBytes), as the engine's runToolStep does,
// so a tool that bounds its own result (native GitHub list ops) sizes it to that limit.
func TestPolicyDispatcher_propagatesOutputBudget(t *testing.T) {
	tests := []struct {
		name    string
		limits  *spec.ExecutionLimits
		enforce bool
		want    int
		wantOK  bool
	}{
		{"default limit", nil, true, spec.DefaultMaxToolOutputBytes, true},
		{"raised per-tool limit", &spec.ExecutionLimits{MaxToolOutputBytes: 1 << 20}, true, 1 << 20, true},
		{"lowered per-tool limit", &spec.ExecutionLimits{MaxToolOutputBytes: 128 << 10}, true, 128 << 10, true},
		{"no enforcer, no budget", nil, false, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, root := enforceGraph(t, tc.limits)
			reg := tools.NewRegistryWithRoot(g, root)
			capture := &budgetCaptureExecutor{}
			reg.Mock = capture
			d := NewPolicyDispatcher(policy.NewEvaluator(g, nil), reg, policy.RunContext{})
			if tc.enforce {
				d = d.WithEnforcement(reg)
			}
			if _, err := d.Call(context.Background(), "tool.ws.read_file", map[string]any{"path": "x"}); err != nil {
				t.Fatal(err)
			}
			if !capture.called || capture.budget != tc.want || capture.ok != tc.wantOK {
				t.Fatalf("tool saw budget (%d, %v) called=%v, want (%d, %v)", capture.budget, capture.ok, capture.called, tc.want, tc.wantOK)
			}
		})
	}
}

type budgetCaptureExecutor struct {
	called bool
	budget int
	ok     bool
}

func (b *budgetCaptureExecutor) Call(ctx context.Context, _ tools.ToolCallRequest) (tools.ToolCallResponse, error) {
	b.called = true
	b.budget, b.ok = tools.OutputBudget(ctx)
	return tools.ToolCallResponse{Output: map[string]any{"ok": true}}, nil
}

type bigOutputExecutor struct{}

func (bigOutputExecutor) Call(_ context.Context, _ tools.ToolCallRequest) (tools.ToolCallResponse, error) {
	return tools.ToolCallResponse{Output: map[string]any{"data": strings.Repeat("z", 500)}}, nil
}
