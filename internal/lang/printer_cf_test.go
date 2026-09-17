package lang

import (
	"strings"
	"testing"
)

// TestPrint_ControlFlowIdempotent proves the printer is idempotent over the
// #199 surface (conditionals, loops, dynamic fan-out, and the expression
// language): parse -> Print -> parse -> Print is stable, and the printed output
// re-parses without error.
func TestPrint_ControlFlowIdempotent(t *testing.T) {
	t.Parallel()
	src := `
agent Reviewer {
    model openai/gpt-5
    grants {
        tool.github.read_pr
    }
    input PullRequest
    output Review
}

workflow Ship(input: Batch) -> Report effects { github.read, github.write } {
    pr = github.get_pr(input.repo)
    if input.n >= 10 && !input.done {
        big = github.merge_pr(input.repo, tag: "release", count: 3)
    } else if input.n == 0 {
        small = github.get_pr()
    } else {
        github.noop()
    }
    for repo in input.repos {
        github.build(repo)
    }
    parallel for item in input.items {
        github.scan(item)
    }
    parallel {
        sec = Reviewer(pr)
    }
    return sec
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)

	f2, diags2 := Parse("t.agent", once)
	if diags2.HasErrors() {
		t.Fatalf("printed output does not re-parse cleanly:\n%s\ndiags: %v", once, diags2)
	}
	twice := Print(f2)
	if once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}

	// Spot-check that meaningful constructs survived the round trip.
	for _, want := range []string{
		"if input.n >= 10 && !input.done {",
		"} else if input.n == 0 {",
		"for repo in input.repos {",
		"parallel for item in input.items {",
		`tag: "release"`,
		"count: 3",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ParenthesizesNestedBinary proves logical grouping is preserved so
// the re-parsed tree is identical.
func TestPrint_ParenthesizesNestedBinary(t *testing.T) {
	t.Parallel()
	// Explicit parens force `||` inside `&&`; the printer must keep them, since
	// dropping them would regroup the tree (&& binds tighter than ||).
	src := `
workflow W(input: X) {
    if input.a && (input.b || input.c) {
        github.noop()
    }
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	out := Print(f)
	if !strings.Contains(out, "input.a && (input.b || input.c)") {
		t.Fatalf("expected the || group to stay parenthesized under &&, got:\n%s", out)
	}
	// A naturally-tighter group needs no added parens.
	if strings.Contains(out, "(input.a)") {
		t.Fatalf("unexpected spurious parentheses:\n%s", out)
	}
	// And it must be idempotent.
	f2, _ := Parse("t.agent", out)
	if Print(f2) != out {
		t.Fatalf("not idempotent for nested binary")
	}
}

// TestPrint_ToolTransportAndPresetRoundTrip proves the .agent tool mcp/http transport blocks and the
// policy preset field survive `terfyn fmt` (parse -> print -> parse -> print is idempotent and the
// constructs are retained) — issue #440, plus the #436 preset round-trip gap.
func TestPrint_ToolTransportAndPresetRoundTrip(t *testing.T) {
	t.Parallel()
	src := `tool github {
    type mcp
    mcp {
        transport "stdio"
        command "npx"
        args { "-y" "@server" }
        headers { "Authorization" "env:GITHUB_TOKEN" }
    }
}

tool webhook {
    type http
    http {
        baseUrl "https://api.example.com"
        headers { "Authorization" "env:API_TOKEN" }
    }
}

policy p {
    preset shell_safe
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{
		"mcp {", `transport "stdio"`, `command "npx"`, `args { "-y" "@server" }`,
		`"Authorization" "env:GITHUB_TOKEN"`, "http {", `baseUrl "https://api.example.com"`,
		"preset shell_safe",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_EnvironmentRoundTrip proves the .agent environment overlay block survives `terfyn fmt`
// (parse -> print -> parse -> print is idempotent and the nested constructs are retained) — #440.
func TestPrint_EnvironmentRoundTrip(t *testing.T) {
	t.Parallel()
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
            guarded-writes {
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
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{
		"environment prod {", "overrides {", "agents {", "reviewer {",
		"model anthropic/claude-sonnet-5", "timeoutSeconds 300",
		"policies {", "guarded-writes {", "maxTotalCostUsd 10", "tool.workspace.run_tests",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_PolicyHitlRoundTrip proves the .agent policy hitl block survives `terfyn fmt`
// (parse -> print -> parse -> print is idempotent and every nested construct is retained) — #440.
func TestPrint_PolicyHitlRoundTrip(t *testing.T) {
	t.Parallel()
	src := `policy gated-publish {
    hitl {
        descriptionPrefix "Publishing requires operator approval"
        redactKeys { "token" }
        toolSwitchMap {
            deploy_to_production { missing_operation staging }
        }
        interruptOn {
            deploy
            publish {
                allowedDecisions { approve reject edit }
                description "Review publish"
                allowedEditArgs { "topic" }
                switchMap {
                    a.b { c.d }
                }
            }
        }
    }
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{
		"hitl {", "descriptionPrefix \"Publishing requires operator approval\"",
		"redactKeys { \"token\" }", "deploy_to_production { missing_operation staging }",
		"interruptOn {", "\n            deploy\n", "publish {",
		"allowedDecisions { approve reject edit }", "a.b { c.d }",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ProviderRoundTrip proves the .agent `provider` decl survives `terfyn fmt` (parse ->
// print -> parse -> print is idempotent and the fields are retained) — #440.
func TestPrint_ProviderRoundTrip(t *testing.T) {
	t.Parallel()
	src := `provider corporate-claude {
    type anthropic
    baseUrl "https://api.anthropic.com"
    apiKeyFrom "env:CORP_ANTHROPIC_KEY"
    workspaceIdFrom "env:CORP_WORKSPACE"
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{
		"provider corporate-claude {", "type anthropic",
		"baseUrl \"https://api.anthropic.com\"",
		"apiKeyFrom \"env:CORP_ANTHROPIC_KEY\"", "workspaceIdFrom \"env:CORP_WORKSPACE\"",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_DefaultsRoundTrip proves the .agent `defaults` decl survives `terfyn fmt` (parse ->
// print -> parse -> print is idempotent and the fields are retained) — #440, ADR 007.
func TestPrint_DefaultsRoundTrip(t *testing.T) {
	t.Parallel()
	src := `defaults {
    policy default
    model anthropic/claude-sonnet-5
    runtime container
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{
		"defaults {", "policy default", "model anthropic/claude-sonnet-5", "runtime container",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ProjectLimitsRoundTrip proves the top-level `limits` decl survives `terfyn fmt` (parse ->
// print -> parse -> print is idempotent and the fields are retained) — #440, ADR 007.
func TestPrint_ProjectLimitsRoundTrip(t *testing.T) {
	t.Parallel()
	src := `limits {
    maxToolInputBytes 4096
    maxLoopIterations 100
    toolInputExceedPolicy fail
    checkpointExceedPolicy fail
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{
		"limits {", "maxToolInputBytes 4096", "maxLoopIterations 100",
		"toolInputExceedPolicy fail", "checkpointExceedPolicy fail",
	} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ApprovalStmtRoundTrip proves the .agent workflow `approval` step survives parse -> print
// -> parse -> print idempotently with its description and redactKeys retained (#440).
func TestPrint_ApprovalStmtRoundTrip(t *testing.T) {
	t.Parallel()
	src := `workflow release(input: any) {
    a = svc.prepare(x: input.y)
    approval gate {
        description "Review before publishing"
        redactKeys { "secret" "token" }
        with {
            note: input.note
            draft: a
        }
    }
    b = svc.send(a: a)
    return b
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{"approval gate {", "description \"Review before publishing\"", "redactKeys { \"secret\" \"token\" }", "with {", "note: input.note", "draft: a"} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ToolWorkspaceRoundTrip proves the .agent workspace tool sub-block survives `terfyn fmt`
// (parse -> print -> parse -> print is idempotent and the fields are retained) — #440.
func TestPrint_ToolWorkspaceRoundTrip(t *testing.T) {
	t.Parallel()
	src := `tool workspace {
    type native
    workspace {
        root "sandbox"
        testCommand "go test ./..."
    }
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{"workspace {", "root \"sandbox\"", "testCommand \"go test ./...\""} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ObjectReturnRoundTrip proves an object-literal return survives `terfyn fmt` (#440).
func TestPrint_ObjectReturnRoundTrip(t *testing.T) {
	t.Parallel()
	src := `workflow snippet(input: any) {
    c = helper.echo(product: input.product)
    return { product: c.echo.product, subject: c.echo.subject, count: 3 }
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if !strings.Contains(once, "return { product: c.echo.product, subject: c.echo.subject, count: 3 }") {
		t.Fatalf("printed output missing the object return:\n%s", once)
	}
}

// TestPrint_ToolRetryOpSchemaPolicyToolsRoundTrip proves the #440 retry/op-schema/policy-tools grammar
// survives `terfyn fmt`.
func TestPrint_ToolRetryOpSchemaPolicyToolsRoundTrip(t *testing.T) {
	t.Parallel()
	src := `tool github {
    type mcp
    retry {
        maxAttempts 3
        backoff "exponential"
    }
    operations {
        create_issue { schema "schemas/CreateIssue.json" effects { github.write } }
    }
}

policy strict {
    tools {
        forbidUnknownTools true
    }
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{"retry {", "maxAttempts 3", "backoff \"exponential\"", "schema \"schemas/CreateIssue.json\"", "forbidUnknownTools true"} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}

// TestPrint_ToolLimitsRoundTrip proves the #440 tool limits block survives `terfyn fmt`.
func TestPrint_ToolLimitsRoundTrip(t *testing.T) {
	t.Parallel()
	src := `tool bulk {
    type native
    limits {
        maxToolInputBytes 1024
        maxLoopIterations 10
        toolInputExceedPolicy truncate
        checkpointExceedPolicy fail
    }
}
`
	f, diags := Parse("t.agent", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	once := Print(f)
	f2, d2 := Parse("t.agent", once)
	if d2.HasErrors() {
		t.Fatalf("printed output does not re-parse:\n%s\ndiags: %v", once, d2)
	}
	if twice := Print(f2); once != twice {
		t.Fatalf("Print is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	for _, want := range []string{"limits {", "maxToolInputBytes 1024", "maxLoopIterations 10", "toolInputExceedPolicy truncate", "checkpointExceedPolicy fail"} {
		if !strings.Contains(once, want) {
			t.Fatalf("printed output missing %q:\n%s", want, once)
		}
	}
}
