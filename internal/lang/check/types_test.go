package check

import (
	"testing"

	"github.com/Terfyn/terfyn/internal/lang"
)

func TestCheckTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, diags lang.Diagnostics)
	}{
		{
			name: "matching agent invocation arg type passes",
			src: `
agent A {
    input  ReviewRequest
    output Review
}

workflow W(input: PullRequest, note: Count) -> Review
{
    r = A(input)
    return r
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if diags.HasErrors() {
					t.Fatalf("expected no type errors, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "mismatched scalar argument type errors",
			src: `
workflow Sub(n: Count) -> Review
{
    github.get_pr()
}

workflow W(input: PullRequest) -> Review
{
    x = Sub(input.repo)
    return x
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if !diags.HasErrors() {
					t.Fatalf("expected a type error, got %v", diags)
				}
				if !hasSeverity(diags, lang.SeverityError, "not compatible") {
					t.Fatalf("expected a not-compatible message, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "value flow through a binding is checked",
			src: `
agent A {
    input  ReviewRequest
    output Review
}

workflow Sub(n: Count) -> Review
{
    github.get_pr()
}

workflow W(input: PullRequest, note: Count) -> Review
{
    r = A(input)
    x = Sub(r)
    return x
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if !diags.HasErrors() {
					t.Fatalf("expected a type error flowing through binding r, got %v", diags)
				}
				if !hasSeverity(diags, lang.SeverityError, "not compatible") {
					t.Fatalf("expected a not-compatible message, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "unresolved type name stays gradual (no schema file)",
			src: `
agent B {
    input  Nonexistent
    output AlsoNonexistent
}

workflow W(input: PullRequest, note: Count) -> Review
{
    y = B(input)
    return y
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if diags.HasErrors() {
					t.Fatalf("expected no errors for an unresolved type name, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "member access past the declared shape errors",
			src: `
agent A {
    input  ReviewRequest
    output Review
}

workflow W(input: PullRequest, note: Count) -> Review
{
    r = A(input)
    github.post_comment(body: r.nonexistent_field)
    return r
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if !diags.HasErrors() {
					t.Fatalf("expected an error for an undeclared field, got %v", diags)
				}
				if !hasSeverity(diags, lang.SeverityError, "nonexistent_field") {
					t.Fatalf("expected a message naming the field, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "boolean true schema is unconstrained",
			src: `
workflow W(input: Count) -> Any
{
    return input
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if diags.HasErrors() {
					t.Fatalf("true schema must accept any producer, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "boolean false schema rejects typed producer",
			src: `
workflow W(input: Count) -> Never
{
    return input
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if !diags.HasErrors() {
					t.Fatalf("false schema must not accept a typed producer, got %v", diags)
				}
				if !hasSeverity(diags, lang.SeverityError, "not compatible") {
					t.Fatalf("expected a not-compatible message, got %v", diagMessages(diags))
				}
			},
		},
		{
			name: "boolean false schema matches false producer",
			src: `
agent A {
    model mock/default
    instructions "test"
    output Never
}

workflow W(input: Count) -> Never
{
    return A(input)
}
`,
			check: func(t *testing.T, diags lang.Diagnostics) {
				if diags.HasErrors() {
					t.Fatalf("never→never must be compatible, got %v", diagMessages(diags))
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := parseOrFatal(t, tc.src)
			_, diags := Check(f, Options{
				Project:   projectWith(githubTool()),
				SchemaDir: "testdata",
			})
			tc.check(t, diags)
		})
	}
}

func TestResolveTypesGradualOnMissingSchema(t *testing.T) {
	t.Parallel()
	f := parseOrFatal(t, `
agent A {
    input  DoesNotExist
}

workflow W() {
    github.get_pr()
}
`)
	tu, diags := resolveTypes(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("expected no errors for a missing schema file, got %v", diagMessages(diags))
	}
	info, ok := tu.agents["A"]
	if !ok {
		t.Fatalf("expected agent A to be indexed")
	}
	if info.Input != nil {
		t.Fatalf("expected a nil (untyped) Input for an unresolved type name, got %+v", info.Input)
	}
}

func TestResolveTypesLoadsSchema(t *testing.T) {
	t.Parallel()
	f := parseOrFatal(t, `
agent A {
    input  ReviewRequest
    output Review
}
`)
	tu, diags := resolveTypes(f, Options{SchemaDir: "testdata"})
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got %v", diagMessages(diags))
	}
	info := tu.agents["A"]
	if info.Input == nil || info.Output == nil {
		t.Fatalf("expected resolved schema documents, got %+v", info)
	}
	res := info.Output.Lookup([]string{"summary"})
	if !res.Known {
		t.Fatalf("expected summary to resolve to a known type, got %+v", res)
	}
}
