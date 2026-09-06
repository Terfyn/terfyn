package lang

import "testing"

func TestParseToolDecl(t *testing.T) {
	f, diags := Parse("t.agent", `tool workspace {
    type native
    safety {
        trusted true
        sideEffects true
    }
    operations {
        read_file  { effects { workspace.read } }
        pull_request.get { effects { github.read } }
    }
}`)
	if diags.HasErrors() {
		t.Fatalf("diags: %v", diags)
	}
	if len(f.Decls) != 1 {
		t.Fatalf("want 1 decl, got %d", len(f.Decls))
	}
	d, ok := f.Decls[0].(*ToolDecl)
	if !ok {
		t.Fatalf("want *ToolDecl, got %T", f.Decls[0])
	}
	if identName(d.Name) != "workspace" || d.Type == nil || d.Type.Name != "native" {
		t.Fatalf("name/type: %+v", d)
	}
	if d.Safety == nil || d.Safety.Trusted == nil || !*d.Safety.Trusted || d.Safety.SideEffects == nil || !*d.Safety.SideEffects {
		t.Fatalf("safety: %+v", d.Safety)
	}
	if d.Operations == nil || len(d.Operations.Ops) != 2 {
		t.Fatalf("operations: %+v", d.Operations)
	}
	if identName(d.Operations.Ops[1].Name) != "pull_request.get" {
		t.Fatalf("dotted op name: %q", identName(d.Operations.Ops[1].Name))
	}
}

func TestParsePolicyDecl(t *testing.T) {
	f, diags := Parse("t.agent", `policy guarded {
    execution { maxTotalCostUsd 5 maxIterations 64 }
    approvals { requiredFor { tool.github.pull_request.post_comment } }
    effects {
        permit { github.read }
        permitWithApproval { github.write }
    }
}`)
	if diags.HasErrors() {
		t.Fatalf("diags: %v", diags)
	}
	d, ok := f.Decls[0].(*PolicyDecl)
	if !ok {
		t.Fatalf("want *PolicyDecl, got %T", f.Decls[0])
	}
	if d.Execution == nil || d.Execution.MaxTotalCostUsd == nil || *d.Execution.MaxTotalCostUsd != 5 {
		t.Fatalf("execution: %+v", d.Execution)
	}
	if d.Execution.MaxIterations == nil || *d.Execution.MaxIterations != 64 {
		t.Fatalf("execution.maxIterations: %+v", d.Execution.MaxIterations)
	}
	if d.Approvals == nil || len(d.Approvals.RequiredFor) != 1 {
		t.Fatalf("approvals: %+v", d.Approvals)
	}
	if d.Effects == nil || len(d.Effects.Permit) != 1 || len(d.Effects.PermitWithApproval) != 1 {
		t.Fatalf("effects: %+v", d.Effects)
	}
}

// TestToolPolicyContextualKeywords guards that `tool`/`policy` remain usable in their existing
// roles (a grant path and an agent field) — they are only declarations at the top level.
func TestToolPolicyContextualKeywords(t *testing.T) {
	f, diags := Parse("t.agent", `agent A {
    model mock/gpt-4
    policy guarded
    grants { tool.github.pull_request.get }
}`)
	if diags.HasErrors() {
		t.Fatalf("`policy <name>` field and `tool.x.y` grant must still parse: %v", diags)
	}
	a := f.Decls[0].(*AgentDecl)
	if a.Policy == nil || a.Policy.Name != "guarded" || len(a.Grants) != 1 {
		t.Fatalf("agent policy/grants: %+v", a)
	}
}

// TestParseToolDecl_DuplicateOperationRejected keeps parity with the YAML loader (which rejects a
// duplicate mapping key): a repeated operation name inside one inline tool is a diagnostic, not a
// silent last-wins.
func TestParseToolDecl_DuplicateOperationRejected(t *testing.T) {
	_, diags := Parse("t.agent", `tool ws {
    type native
    operations {
        read_file { effects { workspace.read } }
        read_file { effects { workspace.write } }
    }
}`)
	found := false
	for _, d := range diags {
		if containsStr(d.Msg, "duplicate operation") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a duplicate operation name must be diagnosed, got: %v", diags)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}

// TestPrintRoundTrip_ToolPolicy: parse → Print → re-parse yields the same declarations, and the
// printed form is itself valid .agent (no `;`).
func TestPrintRoundTrip_ToolPolicy(t *testing.T) {
	src := `tool ws {
    type native
    safety {
        trusted true
    }
    operations {
        read_file { effects { workspace.read } }
    }
}

policy p {
    execution {
        maxTotalCostUsd 3
        maxIterations 64
    }
    effects {
        permit { workspace.read }
        permitWithApproval { workspace.write }
    }
}`
	f1, d1 := Parse("t.agent", src)
	if d1.HasErrors() {
		t.Fatalf("parse 1: %v", d1)
	}
	printed := Print(f1)
	f2, d2 := Parse("t.agent", printed)
	if d2.HasErrors() {
		t.Fatalf("printed output must re-parse cleanly, got %v\n---\n%s", d2, printed)
	}
	if Print(f2) != printed {
		t.Fatalf("print not idempotent:\n--- first ---\n%s\n--- second ---\n%s", printed, Print(f2))
	}
}
