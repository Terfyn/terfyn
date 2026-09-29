package raise

import (
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// yamlWorkflowGraph wraps a spec.WorkflowResource (built as the YAML loader would) in a graph.
func yamlWorkflowGraph(wr *spec.WorkflowResource) *spec.ProjectGraph {
	return &spec.ProjectGraph{Workflows: map[string]*spec.WorkflowResource{wr.Metadata.Name: wr}}
}

// raiseWorkflowStep extracts the raised+re-lowered projection of the named workflow: raise the graph,
// print it, and lower the printed .agent back to a graph. This is the behavioral-equivalence path — the
// re-lowered steps reconstruct the same callees, args, and output as the YAML original.
func raiseAndRelower(t *testing.T, g *spec.ProjectGraph, name string) (*spec.WorkflowResource, string) {
	t.Helper()
	raised, unsup := Graph(g)
	if len(unsup) != 0 {
		t.Fatalf("unexpected Unsupported: %v", unsup)
	}
	out := lang.Print(raised)
	g2 := lowerToGraph(t, out)
	wr := g2.Workflows[name]
	if wr == nil {
		t.Fatalf("raised workflow %q missing after re-lower:\n%s", name, out)
	}
	return wr, out
}

// TestRaise_WorkflowRoundTrip proves a YAML workflow (tool + agent steps, interpolated args, object
// output) raises to a .agent workflow that re-lowers to the SAME steps — same callees, the same
// interpolated with-args, and the same output value references. Equivalence is behavioral, not
// byte-identical: needs/positions may differ, the observable dependencies and I/O do not.
func TestRaise_WorkflowRoundTrip(t *testing.T) {
	src := &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "demo"},
		Spec: spec.WorkflowSpec{
			Input:  &spec.WorkflowInput{Schema: "schemas/DemoInput.json"},
			Policy: "default",
			Steps: []spec.WorkflowStep{
				{ID: "fetch", Uses: "tool.helper.echo", With: map[string]any{"topic": "${input.topic}", "extra": "x"}},
				{ID: "summarize", Agent: "reviewer", With: map[string]any{"echo": "${steps.fetch.output.echo}"}},
			},
			Output: &spec.WorkflowOutput{Value: map[string]any{
				"topic":   "${input.topic}",
				"summary": "${steps.summarize.output.summary}",
			}},
		},
	}
	wr, out := raiseAndRelower(t, yamlWorkflowGraph(src), "demo")

	byID := map[string]spec.WorkflowStep{}
	for _, st := range wr.Spec.Steps {
		byID[st.ID] = st
	}
	fetch, ok := byID["fetch"]
	if !ok || fetch.Uses != "tool.helper.echo" {
		t.Fatalf("fetch step not reconstructed: %+v", wr.Spec.Steps)
	}
	if fetch.With["topic"] != "${input.topic}" || fetch.With["extra"] != "x" {
		t.Fatalf("fetch args not reconstructed: %+v", fetch.With)
	}
	sum, ok := byID["summarize"]
	if !ok || sum.Agent != "reviewer" || sum.With["echo"] != "${steps.fetch.output.echo}" {
		t.Fatalf("summarize step not reconstructed: %+v", sum)
	}
	if wr.Spec.Output == nil || wr.Spec.Output.Value["topic"] != "${input.topic}" || wr.Spec.Output.Value["summary"] != "${steps.summarize.output.summary}" {
		t.Fatalf("output not reconstructed: %+v", wr.Spec.Output)
	}
	// The input schema is emitted as the typed param `input: DemoInput` (the checker wires it to
	// Spec.Input during a full load; LowerFile alone does not). Assert on the printed .agent source.
	if !strings.Contains(out, "workflow demo(input: DemoInput)") {
		t.Fatalf("input param not emitted in raised source:\n%s", out)
	}
	if wr.Spec.Policy != "default" {
		t.Fatalf("policy not reconstructed: %q", wr.Spec.Policy)
	}
}

// TestRaise_WorkflowApprovalStep proves an approval step raises to a .agent `approval` statement and
// re-lowers to a WorkflowStep carrying the Approval value with its config and review payload.
func TestRaise_WorkflowApprovalStep(t *testing.T) {
	src := &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "gated"},
		Spec: spec.WorkflowSpec{
			Steps: []spec.WorkflowStep{
				{ID: "prep", Uses: "tool.helper.echo", With: map[string]any{"topic": "${input.topic}"}},
				{ID: "gate", Approval: &spec.WorkflowApprovalValue{Enabled: true, Config: &spec.WorkflowApprovalConfig{
					Description: "Review before publishing",
					RedactKeys:  []string{"secret"},
				}}, With: map[string]any{"summary": "${steps.prep.output.echo}"}},
			},
		},
	}
	wr, _ := raiseAndRelower(t, yamlWorkflowGraph(src), "gated")
	var gate *spec.WorkflowStep
	for i := range wr.Spec.Steps {
		if wr.Spec.Steps[i].ID == "gate" {
			gate = &wr.Spec.Steps[i]
		}
	}
	if gate == nil || !spec.StepIsApproval(*gate) {
		t.Fatalf("approval step not reconstructed: %+v", wr.Spec.Steps)
	}
	if gate.Approval.Config == nil || gate.Approval.Config.Description != "Review before publishing" || len(gate.Approval.Config.RedactKeys) != 1 {
		t.Fatalf("approval config not reconstructed: %+v", gate.Approval)
	}
	if gate.With["summary"] != "${steps.prep.output.echo}" {
		t.Fatalf("approval payload not reconstructed: %+v", gate.With)
	}
}

// TestRaise_WorkflowRefusesMetaRef proves the raiser refuses (records an Unsupported for) a workflow
// whose interpolation uses steps.<id>.meta — which has no .agent reference form — rather than
// mistranslating it.
func TestRaise_WorkflowRefusesMetaRef(t *testing.T) {
	src := &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "meta"},
		Spec: spec.WorkflowSpec{
			Steps:  []spec.WorkflowStep{{ID: "a", Uses: "tool.helper.echo", With: map[string]any{"topic": "x"}}},
			Output: &spec.WorkflowOutput{Value: map[string]any{"cost": "${steps.a.meta.cost}"}},
		},
	}
	_, unsup := Graph(yamlWorkflowGraph(src))
	if len(unsup) == 0 {
		t.Fatal("a steps.<id>.meta reference must be refused, not mistranslated")
	}
	if unsup[0].Kind != "Workflow" {
		t.Fatalf("expected a Workflow Unsupported, got %v", unsup)
	}
}

// TestRaise_WorkflowRefusesArrayValue proves an array with-value (no .agent literal form) is refused.
func TestRaise_WorkflowRefusesArrayValue(t *testing.T) {
	src := &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "arr"},
		Spec: spec.WorkflowSpec{
			Steps: []spec.WorkflowStep{{ID: "a", Uses: "tool.helper.echo", With: map[string]any{"tags": []any{"x", "y"}}}},
		},
	}
	_, unsup := Graph(yamlWorkflowGraph(src))
	if len(unsup) == 0 {
		t.Fatal("an array argument value must be refused")
	}
}

// TestRaise_WorkflowRefusesNullValue proves a YAML null is refused, not silently downgraded to "".
// .agent has no null literal, and null is observably distinct from an empty string.
func TestRaise_WorkflowRefusesNullValue(t *testing.T) {
	src := &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "nul"},
		Spec: spec.WorkflowSpec{
			Steps: []spec.WorkflowStep{{ID: "a", Uses: "tool.helper.echo", With: map[string]any{"opt": nil}}},
		},
	}
	_, unsup := Graph(yamlWorkflowGraph(src))
	if len(unsup) == 0 {
		t.Fatal("a null argument value must be refused, not mistranslated to \"\"")
	}
	if unsup[0].Kind != "Workflow" {
		t.Fatalf("expected a Workflow Unsupported, got %v", unsup)
	}
}

// TestRaise_WholeDocumentCallShapeRoundTrips: the explicit whole-document bit
// (#550) raises to a positional argument and a named `arg0:` call stays named, so
// both re-lower to the shape they started with — the with: maps are identical.
func TestRaise_WholeDocumentCallShapeRoundTrips(t *testing.T) {
	src := &spec.WorkflowResource{
		Metadata: spec.Metadata{Name: "demo"},
		Spec: spec.WorkflowSpec{
			Input:  &spec.WorkflowInput{Schema: "schemas/DemoInput.json"},
			Policy: "default",
			Steps: []spec.WorkflowStep{
				{ID: "whole", Agent: "reviewer", With: map[string]any{"arg0": "${input}"}, WholeDocument: true},
				{ID: "named", Agent: "reviewer", With: map[string]any{"arg0": "${steps.whole.output}"}},
			},
			Output: &spec.WorkflowOutput{Value: map[string]any{"r": "${steps.named.output}"}},
		},
	}
	wr, out := raiseAndRelower(t, yamlWorkflowGraph(src), "demo")
	if !strings.Contains(out, "whole = reviewer(input)") || !strings.Contains(out, "named = reviewer(arg0: whole)") {
		t.Fatalf("raised source did not keep call shapes:\n%s", out)
	}
	got := map[string]bool{}
	for _, st := range wr.Spec.Steps {
		got[st.ID] = st.WholeDocument
	}
	if !got["whole"] || got["named"] {
		t.Fatalf("re-lowered shapes = %v", got)
	}
}
