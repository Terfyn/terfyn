package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

func wfWithInputSchema(ref string) *spec.WorkflowResource {
	return &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "wf"},
		Spec:     spec.WorkflowSpec{Input: &spec.WorkflowInput{Schema: ref}},
	}
}

const strictInputSchema = `{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`
const permissiveInputSchema = `{"type":"object"}`

// A pinned resume validates against the schema captured in its deployment snapshot, not a re-read of
// the file on disk. Here the on-disk file has drifted to permissive since the run started; the
// pinned run must still enforce the captured strict schema. The PinnedGraph=false half is the
// "would pass against disk" control that shows the drift the capture prevents.
func TestValidateWorkflowInput_pinnedUsesCapturedSchemaNotDisk(t *testing.T) {
	root := t.TempDir()
	// Simulate schema drift: the file on disk is now permissive.
	if err := os.WriteFile(filepath.Join(root, "s.json"), []byte(permissiveInputSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	wf := wfWithInputSchema("./s.json")
	badInput := map[string]any{"y": 1} // missing required "x"

	pinned := &Executor{PinnedGraph: true, ProjectRoot: root, Schemas: map[string]string{"./s.json": strictInputSchema}}
	if err := pinned.validateWorkflowInputSchema(wf, badInput); err == nil {
		t.Fatal("pinned resume must enforce the captured strict schema, not the drifted permissive file")
	}

	// Control: the on-disk (drifted) schema is permissive, so the non-pinned path accepts the input.
	fresh := &Executor{PinnedGraph: false, ProjectRoot: root}
	if err := fresh.validateWorkflowInputSchema(wf, badInput); err != nil {
		t.Fatalf("control: non-pinned path reads the permissive disk schema and should accept; got %v", err)
	}
}

// A schema not present in the captured bundle (not collected at run start — gradual/absent) is
// treated as allowed on the pinned path, and no disk read happens.
func TestValidateWorkflowInput_pinnedMissingSchemaIsGradual(t *testing.T) {
	wf := wfWithInputSchema("./absent.json")
	pinned := &Executor{PinnedGraph: true, ProjectRoot: "/nonexistent", Schemas: map[string]string{}}
	if err := pinned.validateWorkflowInputSchema(wf, map[string]any{"anything": true}); err != nil {
		t.Fatalf("uncaptured schema must be gradual (allowed) on the pinned path, not read from disk: %v", err)
	}
}

func toolGraphWithOpSchema(ref string) *spec.ProjectGraph {
	return &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{
		"github": {Metadata: spec.Metadata{Name: "github"}, Spec: spec.ToolSpec{
			Type:               "native",
			OperationsDeclared: true,
			Operations:         map[string]spec.ToolOperation{"read_pr": {Effects: []string{"github.read"}, Schema: ref}},
		}},
	}}
}

// A tool call's input is validated against the operation's declared input schema (completing the
// #204 manifest). On a fresh run it reads the schema from disk; on a pinned resume it uses the
// captured bundle.
func TestValidateToolInputSchema_enforcesOperationSchema(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.json"), []byte(strictInputSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Graph: toolGraphWithOpSchema("./in.json"), ProjectRoot: root}

	if err := e.validateToolInputSchema("tool.github.read_pr", map[string]any{"x": "ok"}); err != nil {
		t.Fatalf("valid tool input should pass the operation schema: %v", err)
	}
	if err := e.validateToolInputSchema("tool.github.read_pr", map[string]any{"y": 1}); err == nil {
		t.Fatal("tool input missing required field must be rejected by the operation schema")
	}
	// An operation with no declared schema is gradual (any input).
	e2 := &Executor{Graph: toolGraphWithOpSchema(""), ProjectRoot: root}
	if err := e2.validateToolInputSchema("tool.github.read_pr", map[string]any{"anything": true}); err != nil {
		t.Fatalf("operation without a schema must accept any input: %v", err)
	}
}

func TestValidateToolInputSchema_pinnedUsesCapturedNotDisk(t *testing.T) {
	root := t.TempDir()
	// On-disk schema has drifted to permissive since the run started.
	if err := os.WriteFile(filepath.Join(root, "in.json"), []byte(permissiveInputSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned := &Executor{
		Graph:       toolGraphWithOpSchema("./in.json"),
		ProjectRoot: root,
		PinnedGraph: true,
		Schemas:     map[string]string{"./in.json": strictInputSchema},
	}
	if err := pinned.validateToolInputSchema("tool.github.read_pr", map[string]any{"y": 1}); err == nil {
		t.Fatal("pinned tool input must enforce the captured strict schema, not the drifted disk file")
	}
}

// enforceToolInput (the real runToolStep call site) must reject a tool call whose input violates the
// operation schema, on the payload actually dispatched.
func TestEnforceToolInput_rejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.json"), []byte(strictInputSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Graph: toolGraphWithOpSchema("./in.json"), ProjectRoot: root}
	if _, err := e.enforceToolInput(context.Background(), nil, "r1", "s1", "tool.github.read_pr", map[string]any{"y": 1}); err == nil {
		t.Fatal("enforceToolInput must reject input that violates the operation schema")
	}
	if _, err := e.enforceToolInput(context.Background(), nil, "r1", "s1", "tool.github.read_pr", map[string]any{"x": "ok"}); err != nil {
		t.Fatalf("valid input must pass enforceToolInput: %v", err)
	}
}

// The schema must be validated on the payload AFTER byte-limit truncation, not before: under the
// default truncate policy a valid input can be mutated into a schema-violating one, and dispatch
// must not observe that. Input valid pre-truncation, invalid once "..." is spliced in.
func TestEnforceToolInput_rejectsPostTruncationMismatch(t *testing.T) {
	root := t.TempDir()
	// id must be all lowercase letters — the "..." truncation marker breaks the pattern.
	if err := os.WriteFile(filepath.Join(root, "in.json"),
		[]byte(`{"type":"object","required":["id"],"properties":{"id":{"type":"string","pattern":"^[a-z]+$"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := toolGraphWithOpSchema("./in.json")
	// Force truncation of any non-trivial input.
	g.Tools["github"].Spec.Limits = &spec.ExecutionLimits{MaxToolInputBytes: 30, ToolInputExceedPolicy: spec.LimitExceedTruncate}
	e := &Executor{Graph: g, ProjectRoot: root}

	validPreTruncation := map[string]any{"id": strings.Repeat("a", 500)} // matches ^[a-z]+$, but ~510 bytes
	if _, err := e.enforceToolInput(context.Background(), nil, "r1", "s1", "tool.github.read_pr", validPreTruncation); err == nil {
		t.Fatal("truncation spliced \"...\" into id, breaking the pattern; the dispatched payload must be rejected")
	}
}

func TestValidateAgentOutput_pinnedUsesCapturedSchema(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "a"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./out.json"}},
	}
	pinned := &Executor{PinnedGraph: true, Schemas: map[string]string{"./out.json": strictInputSchema}}
	if err := pinned.validateAgentOutputSchema(agent, `{"y":1}`); err == nil {
		t.Fatal("pinned agent output must enforce the captured schema")
	}
	if err := pinned.validateAgentOutputSchema(agent, `{"x":"ok"}`); err != nil {
		t.Fatalf("valid output should pass the captured schema: %v", err)
	}
}

// A pinned run whose captured schema is a Draft 2020-12 boolean root schema (issue #549) validates
// against exactly those bytes: `false` rejects every instance (never valid), `true` accepts all. The
// drifted on-disk file (permissive) must not be consulted.
func TestValidateWorkflowInput_pinnedBooleanSchemas(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "s.json"), []byte(permissiveInputSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	wf := wfWithInputSchema("./s.json")

	never := &Executor{PinnedGraph: true, ProjectRoot: root, Schemas: map[string]string{"./s.json": "false\n"}}
	for _, in := range []map[string]any{{}, {"x": "y"}} {
		if err := never.validateWorkflowInputSchema(wf, in); err == nil {
			t.Fatalf("pinned false schema must reject every input, accepted %v", in)
		}
	}

	anything := &Executor{PinnedGraph: true, ProjectRoot: "/nonexistent", Schemas: map[string]string{"./s.json": "true"}}
	if err := anything.validateWorkflowInputSchema(wf, map[string]any{"x": 1}); err != nil {
		t.Fatalf("pinned true schema must accept any input: %v", err)
	}
}
