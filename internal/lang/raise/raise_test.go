package raise

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/lang/lower"
	"github.com/Terfyn/terfyn/internal/spec"
)

// lowerToGraph parses+lowers .agent source to a spec graph, failing on any diagnostic.
func lowerToGraph(t *testing.T, src string) *spec.ProjectGraph {
	t.Helper()
	f, d := lang.Parse("t.agent", src)
	if d.HasErrors() {
		t.Fatalf("parse: %v", d)
	}
	res, ld := lower.LowerFile(f, lower.Options{})
	if ld.HasErrors() {
		t.Fatalf("lower: %v", ld)
	}
	return res.ToGraph()
}

// TestRaise_RoundTrip is the migration correctness gate (issue #440): lowering .agent to a spec graph,
// raising it back to AST, printing, and re-lowering must reproduce the SAME spec — the mirror of the
// forward ADR 005 §2 goldens. Covers every declarative kind (provider, tool with mcp/safety/ops,
// policy with preset/execution/approvals/effects/hitl, environment, agent) except agent input/output
// schemas, which the checker (not LowerFile) wires — see TestRaise_AgentIO.
func TestRaise_RoundTrip(t *testing.T) {
	src := `provider corporate-claude {
    type anthropic
    baseUrl "https://api.anthropic.com"
    apiKeyFrom "env:CORP_KEY"
    workspaceIdFrom "env:CORP_WS"
}

defaults {
    policy base
    model corporate-claude/claude-sonnet-5
    runtime container
}

limits {
    maxToolInputBytes 4096
    maxToolOutputBytes 8192
    maxWorkflowNesting 4
    toolInputExceedPolicy fail
    checkpointExceedPolicy fail
}

tool github {
    type mcp
    mcp {
        transport "stdio"
        command "npx"
        args { "-y" "server-github" }
        headers { "Authorization" "env:GITHUB_TOKEN" }
    }
}

tool helper {
    type mock
    safety {
        trusted true
        sideEffects false
    }
    operations {
        echo { effects { workspace.read } }
    }
}

policy base {
    preset shell_safe
}

policy guarded {
    execution {
        maxTotalCostUsd 5
        maxWallClockSeconds 300
    }
    approvals {
        requiredFor {
            tool.helper.echo
        }
    }
    effects {
        permit { workspace.read }
        permitWithApproval { workspace.write }
    }
    hitl {
        descriptionPrefix "review"
        interruptOn {
            helper {
                allowedDecisions { approve reject }
                description "gate"
                allowedEditArgs { "topic" }
            }
        }
    }
}

agent assistant {
    model corporate-claude/claude-sonnet-5
    policy guarded
    description "an assistant"
    constraints {
        timeoutSeconds 60
        maxIterations 8
    }
    grants {
        tool.helper.echo
    }
}
`
	g1 := lowerToGraph(t, src)

	raised, unsup := Graph(g1)
	if len(unsup) != 0 {
		t.Fatalf("unexpected unsupported findings: %v", unsup)
	}
	out := lang.Print(raised)

	g2 := lowerToGraph(t, out)

	// Compare each resource kind's spec JSON.
	assertSpecEqual(t, "providers", g1.Spec.Providers, g2.Spec.Providers, out)
	assertSpecEqual(t, "defaults", g1.Spec.Defaults, g2.Spec.Defaults, out)
	assertSpecEqual(t, "limits", g1.Spec.Limits, g2.Spec.Limits, out)
	for _, name := range sortedKeys(g1.Tools) {
		assertSpecEqual(t, "tool "+name, g1.Tools[name].Spec, g2.Tools[name].Spec, out)
	}
	for _, name := range sortedKeys(g1.Policies) {
		assertSpecEqual(t, "policy "+name, g1.Policies[name].Spec, g2.Policies[name].Spec, out)
	}
	for _, name := range sortedKeys(g1.Agents) {
		assertSpecEqual(t, "agent "+name, g1.Agents[name].Spec, g2.Agents[name].Spec, out)
	}
	if len(g2.Tools) != len(g1.Tools) || len(g2.Policies) != len(g1.Policies) || len(g2.Agents) != len(g1.Agents) {
		t.Fatalf("resource count drift after round-trip")
	}
}

// TestRaise_EnvironmentRoundTrip covers the environment kind end to end (its overrides lower into a
// resource, unlike agent I/O).
func TestRaise_EnvironmentRoundTrip(t *testing.T) {
	src := `environment prod {
    overrides {
        agents {
            reviewer {
                model anthropic/claude-sonnet-5
                constraints {
                    timeoutSeconds 300
                }
            }
        }
        policies {
            guarded {
                execution {
                    maxTotalCostUsd 10
                }
                approvals {
                    requiredFor {
                        tool.workspace.run_tests
                    }
                }
            }
        }
    }
}
`
	g1 := lowerToGraph(t, src)
	raised, unsup := Graph(g1)
	if len(unsup) != 0 {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	g2 := lowerToGraph(t, lang.Print(raised))
	assertSpecEqual(t, "environment prod", g1.Environments["prod"].Spec, g2.Environments["prod"].Spec, lang.Print(raised))
}

// TestRaise_AgentIO covers input/output schema raising, which LowerFile does not populate (the checker
// wires it), so it is exercised from a constructed AgentResource.
func TestRaise_AgentIO(t *testing.T) {
	a := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "assistant"},
		Spec: spec.AgentSpec{
			Model:  "mock/default",
			Input:  &spec.AgentIO{Schema: "schemas/TicketInput.json"},
			Output: &spec.AgentIO{Schema: "schemas/HandoffOutput.json"},
		},
	}
	g := &spec.ProjectGraph{Agents: map[string]*spec.AgentResource{"assistant": a}}
	raised, unsup := Graph(g)
	if len(unsup) != 0 {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	out := lang.Print(raised)
	for _, want := range []string{"input TicketInput", "output HandoffOutput"} {
		if !strings.Contains(out, want) {
			t.Fatalf("printed output missing %q:\n%s", want, out)
		}
	}
	// A non-conventional schema ref cannot be raised to a bare type name — it must be refused.
	a.Spec.Output.Schema = "weird/path.yaml"
	if _, u := Graph(g); len(u) == 0 {
		t.Fatal("a non-conventional output schema ref must be refused")
	}
}

// TestRaise_WorkflowRaisesCleanly: a supported workflow (steps + output) now raises without any
// Unsupported finding — the migrate tool emits it as .agent rather than refusing. Refusal is reserved
// for genuinely unraiseable constructs (see raise_workflow_test.go).
func TestRaise_WorkflowRaisesCleanly(t *testing.T) {
	g := &spec.ProjectGraph{
		Workflows: map[string]*spec.WorkflowResource{"w": {
			Metadata: spec.Metadata{Name: "w"},
			Spec: spec.WorkflowSpec{
				Steps:  []spec.WorkflowStep{{ID: "a", Uses: "tool.helper.echo", With: map[string]any{"topic": "${input.topic}"}}},
				Output: &spec.WorkflowOutput{Value: map[string]any{"out": "${steps.a.output.echo}"}},
			},
		}},
	}
	_, unsup := Graph(g)
	if len(unsup) != 0 {
		t.Fatalf("a supported workflow must raise without Unsupported, got %v", unsup)
	}
}

func assertSpecEqual(t *testing.T, label string, a, b any, printed string) {
	t.Helper()
	aj, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bj, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(aj) != string(bj) {
		t.Fatalf("%s spec drifted after raise->print->lower:\n original: %s\n round:    %s\n printed source:\n%s", label, aj, bj, printed)
	}
}

// TestRaise_ToolWorkspace: a tool with a native workspace sub-block now raises (no longer refused) and
// round-trips through print+lower to the same spec (issue #440).
func TestRaise_ToolWorkspace(t *testing.T) {
	src := `tool workspace {
    type native
    workspace {
        root "sandbox"
        testCommand "go test ./..."
    }
    safety {
        trusted true
        sideEffects true
    }
}
`
	g1 := lowerToGraph(t, src)
	raised, unsup := Graph(g1)
	if len(unsup) != 0 {
		t.Fatalf("workspace tool must raise without Unsupported, got %v", unsup)
	}
	out := lang.Print(raised)
	if !strings.Contains(out, "workspace {") || !strings.Contains(out, "root \"sandbox\"") {
		t.Fatalf("raised source missing workspace block:\n%s", out)
	}
	g2 := lowerToGraph(t, out)
	assertSpecEqual(t, "tool workspace", g1.Tools["workspace"].Spec, g2.Tools["workspace"].Spec, out)
}

// TestRaise_ResidualsAreGenuine characterizes the complete residual set (ADR 007 step 1): after closing
// the field gaps, the ONLY constructs raise refuses are genuinely-unraiseable ones — a YAML-authored
// workflow (no lossless .agent step-DAG form) and a non-convention agent schema ref. Nothing in the
// supported declarative model is refused, so migration is lossless for the supported model.
func TestRaise_ResidualsAreGenuine(t *testing.T) {
	g := &spec.ProjectGraph{
		Workflows: map[string]*spec.WorkflowResource{
			"flow": {Metadata: spec.Metadata{Name: "flow"}, Spec: spec.WorkflowSpec{Steps: []spec.WorkflowStep{{ID: "s"}}}},
		},
		Agents: map[string]*spec.AgentResource{
			"a": {Metadata: spec.Metadata{Name: "a"}, Spec: spec.AgentSpec{Output: &spec.AgentIO{Schema: "weird/path.json"}}},
		},
	}
	_, unsup := Graph(g)
	if len(unsup) == 0 {
		t.Fatal("expected residual Unsupported findings for the workflow + bad schema ref")
	}
	allowed := map[string]bool{
		"spec.steps":         true, // YAML workflow
		"spec.output.schema": true, // non-convention schema ref
		"spec.input.schema":  true,
	}
	for _, u := range unsup {
		if !allowed[u.Field] {
			t.Fatalf("unexpected residual: %s (%s) — only genuinely-unraiseable constructs should remain, got %+v", u.Field, u.Kind, unsup)
		}
	}
}
