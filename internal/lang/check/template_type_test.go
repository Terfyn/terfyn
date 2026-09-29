package check

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/spec"
)

// templateCallSrc places `r = reviewer(<args>)` straight-line, in a while body, or
// in an if arm. producer: String -> <producerOut>; reviewer: <reviewerIn> -> String.
// The while/if steps are Synthetic, which graph validation skips, so the checker
// alone must refuse there what graph validation refuses straight-line (#550).
func templateCallSrc(producerOut, reviewerIn, args, shape string) string {
	call := "r = reviewer(" + args + ")"
	var body string
	switch shape {
	case "straight":
		body = "    " + call
	case "while":
		body = "    while input != \"done\" limit 2 {\n        " + call + "\n    }"
	case "if":
		body = "    if input != \"done\" {\n        " + call + "\n    }"
	default:
		panic("unknown shape " + shape)
	}
	return `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output ` + producerOut + `
}

agent reviewer {
    model mock/default
    instructions "review"
    input ` + reviewerIn + `
    output String
}

workflow demo(input: String) -> String {
    v = producer(input)
` + body + `
    return input
}
`
}

var templateShapes = []string{"straight", "while", "if"}

// An interpolated string literal is typed by graph validation's rule: a string that
// is exactly one ${…} token has the referenced binding's type, any other string
// containing a token is a string. The checker applies the same rule in every
// position — straight-line and inside control-flow bodies — and straight-line
// graph validation rejects the same calls, so the two layers agree.
func TestCheck_StringTemplateTypedLikeValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, producerOut, reviewerIn, args string
		checkerWant, validatorWant          string
	}{
		{
			name: "whole-token field takes the binding type", producerOut: "Count", reviewerIn: "ReviewRequest",
			args: `{repo: "${v}", number: 1}`, checkerWant: `field "repo": type integer`, validatorWant: `input "repo" (string)`,
		},
		{
			name: "mixed template field is a string", producerOut: "String", reviewerIn: "ReviewRequest",
			args: `{repo: input, number: "n${v}"}`, checkerWant: `field "number": type string`, validatorWant: `input "number" (integer)`,
		},
		{
			name: "scalar mixed template document is a string", producerOut: "Count", reviewerIn: "Count",
			args: `"n${v}"`, checkerWant: `input of reviewer: type string`, validatorWant: `input "input" (integer)`,
		},
	}
	for _, tc := range cases {
		for _, shape := range templateShapes {
			t.Run(tc.name+"/"+shape, func(t *testing.T) {
				t.Parallel()
				prog, diags := Check(parseOrFatal(t, templateCallSrc(tc.producerOut, tc.reviewerIn, tc.args, shape)), Options{SchemaDir: "testdata"})
				msgs := strings.Join(diagMessages(diags), "\n")
				if !diags.HasErrors() || !strings.Contains(msgs, tc.checkerWant) {
					t.Fatalf("want checker error containing %q, got %v", tc.checkerWant, msgs)
				}
				if shape != "straight" {
					return
				}
				errs := spec.ValidateProjectGraph(prog.Graph, "testdata")
				if errs == nil || !strings.Contains(errs.Error(), tc.validatorWant) {
					t.Fatalf("straight-line graph validation must reject the same call (%q), got %v", tc.validatorWant, errs)
				}
			})
		}
	}
}

// Well-typed templates check clean in every position and validate straight-line: a
// whole token of a String binding into a string field, a mixed template of an
// integer binding into a string field or document, a whole token of an integer
// binding into an integer field, and a plain literal (untyped) wherever it sits.
func TestCheck_StringTemplateWellTypedAccepted(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, producerOut, reviewerIn, args string }{
		{"whole-token string field", "String", "ReviewRequest", `{repo: "${v}", number: 1}`},
		{"mixed template into string field", "Count", "ReviewRequest", `{repo: "org/${v}", number: 1}`},
		{"whole-token integer field", "Count", "ReviewRequest", `{repo: input, number: "${v}"}`},
		{"mixed template document into string", "Count", "String", `"n${v}"`},
		{"plain literal", "Count", "Count", `"not a template"`},
	}
	for _, tc := range cases {
		for _, shape := range templateShapes {
			t.Run(tc.name+"/"+shape, func(t *testing.T) {
				t.Parallel()
				prog, diags := Check(parseOrFatal(t, templateCallSrc(tc.producerOut, tc.reviewerIn, tc.args, shape)), Options{SchemaDir: "testdata"})
				if diags.HasErrors() {
					t.Fatalf("well-typed template must check clean, got %v", diagMessages(diags))
				}
				if shape != "straight" {
					return
				}
				if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
					t.Fatalf("well-typed template must validate, got %v", errs)
				}
			})
		}
	}
}

// A template token is a reference: an undeclared member path, or a head that is not
// definitely assigned (bound in one if arm only), is a checker error, reported once.
func TestCheck_StringTemplateTokenResolvedAsReference(t *testing.T) {
	t.Parallel()
	undeclared := templateCallSrc("ReviewRequest", "ReviewRequest", `{repo: "${v.missing}", number: 1}`, "while")
	_, diags := Check(parseOrFatal(t, undeclared), Options{SchemaDir: "testdata"})
	if !diags.HasErrors() || !strings.Contains(strings.Join(diagMessages(diags), "\n"), `"missing" is not declared in the schema for "v"`) {
		t.Fatalf("undeclared template path must be a checker error, got %v", diagMessages(diags))
	}

	src := `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output String
}

workflow demo(input: String) -> String {
    if input != "done" {
        w = producer(input)
    }
    r = producer("x${w}")
    return input
}
`
	_, diags = Check(parseOrFatal(t, src), Options{SchemaDir: "testdata"})
	msgs := diagMessages(diags)
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, `unresolved reference "w" in interpolation`) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a one-arm binding in a template must be reported exactly once as unresolved, got %v", msgs)
	}
}

// A return value is a value position the execution IR interpolates too
// (lowerValue/stringTemplateValue), so a whole-token return has the binding's type.
func TestCheck_StringTemplateReturnTyped(t *testing.T) {
	t.Parallel()
	src := func(ret string) string {
		return `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output Count
}

workflow demo(input: String) -> String {
    v = producer(input)
    return ` + ret + `
}
`
	}
	_, diags := Check(parseOrFatal(t, src(`"${v}"`)), Options{SchemaDir: "testdata"})
	if !diags.HasErrors() || !strings.Contains(strings.Join(diagMessages(diags), "\n"), "return value: type integer") {
		t.Fatalf("an integer whole-token return into a String result must be an error, got %v", diagMessages(diags))
	}
	if _, diags := Check(parseOrFatal(t, src(`"n=${v}"`)), Options{SchemaDir: "testdata"}); diags.HasErrors() {
		t.Fatalf("a mixed template return is a string, got %v", diagMessages(diags))
	}
}

// forCollectionSrc is a workflow whose (parallel) for loop iterates coll; s is a
// CodingState whose feedback field is an array of strings.
func forCollectionSrc(coll string, parallel bool) string {
	kw := "for"
	if parallel {
		kw = "parallel for"
	}
	return `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output CodingState
}

agent echo {
    model mock/default
    instructions "echo"
    input String
    output String
}

workflow demo(input: String) -> String {
    s = producer(input)
    ` + kw + ` x in ` + coll + ` {
        r = echo(x)
    }
    return input
}
`
}

// A for / parallel for collection is a value position: the execution IR lowers it
// with lowerValue, so `for x in "${xs}"` iterates the binding xs. The checker must
// resolve its tokens (graph validation never sees the collection), so an unresolved
// token is a checker error reported exactly once rather than a validate-clean
// program that fails at run time with `execir: unresolved reference`.
func TestCheck_ForCollectionTemplateResolved(t *testing.T) {
	t.Parallel()
	for _, parallel := range []bool{false, true} {
		name := "for"
		if parallel {
			name = "parallel for"
		}
		t.Run(name+"/unresolved", func(t *testing.T) {
			t.Parallel()
			_, diags := Check(parseOrFatal(t, forCollectionSrc(`"${zz}"`, parallel)), Options{SchemaDir: "testdata"})
			n := 0
			for _, m := range diagMessages(diags) {
				if strings.Contains(m, `unresolved reference "zz"`) {
					n++
				}
			}
			if !diags.HasErrors() || n != 1 {
				t.Fatalf("an unresolved collection token must be reported exactly once, got %v", diagMessages(diags))
			}
		})
		t.Run(name+"/undeclared path", func(t *testing.T) {
			t.Parallel()
			_, diags := Check(parseOrFatal(t, forCollectionSrc(`"${s.nope}"`, parallel)), Options{SchemaDir: "testdata"})
			if !diags.HasErrors() || !strings.Contains(strings.Join(diagMessages(diags), "\n"), `"nope" is not declared in the schema for "s"`) {
				t.Fatalf("an undeclared collection token path must be a checker error, got %v", diagMessages(diags))
			}
		})
		t.Run(name+"/resolved list", func(t *testing.T) {
			t.Parallel()
			prog, diags := Check(parseOrFatal(t, forCollectionSrc(`"${s.feedback}"`, parallel)), Options{SchemaDir: "testdata"})
			if diags.HasErrors() {
				t.Fatalf("a collection token naming a list binding must check clean, got %v", diagMessages(diags))
			}
			if errs := spec.ValidateProjectGraph(prog.Graph, "testdata"); errs != nil {
				t.Fatalf("must validate, got %v", errs)
			}
			ex := prog.Executables["demo"]
			if ex == nil {
				t.Fatalf("expected execution IR for demo")
			}
			var loop *execir.Loop
			for _, n := range ex.Body {
				if l, ok := n.(*execir.Loop); ok {
					loop = l
				}
			}
			if loop == nil || loop.Parallel != parallel {
				t.Fatalf("expected a Loop (parallel=%v), got %#v", parallel, ex.Body)
			}
			ref, ok := loop.Collection.(execir.Ref)
			if !ok || !reflect.DeepEqual(ref.Path, []string{"s", "feedback"}) {
				t.Fatalf("the collection template must lower to Ref[s feedback], got %#v", loop.Collection)
			}
		})
	}
}

// An empty token path (`${}`, `${ . }`, also embedded in text) names nothing: the
// execution IR would lower it to an empty Ref that fails at run time with
// `execir: empty reference path`. The checker reports it in every value position —
// a for / parallel for collection and a return, which the resource projection never
// interpolates — and, for a call argument, the resource projection's identical
// diagnostic collapses with the checker's so it is reported exactly once.
func TestCheck_EmptyTemplateTokenRejected(t *testing.T) {
	t.Parallel()
	const want = `unresolved reference "" in interpolation`
	returnSrc := func(ret string) string {
		return `
agent producer {
    model mock/default
    instructions "return a value"
    input String
    output String
}

workflow demo(input: String) -> String {
    v = producer(input)
    return ` + ret + `
}
`
	}
	argSrc := func(arg string) string {
		return `
agent echo {
    model mock/default
    instructions "echo"
    input String
    output String
}

workflow demo(input: String) -> String {
    r = echo(` + arg + `)
    return input
}
`
	}
	for _, tok := range []string{`"${}"`, `"${ . }"`, `"a${}b"`} {
		srcs := map[string]string{
			"for":          forCollectionSrc(tok, false),
			"parallel for": forCollectionSrc(tok, true),
			"return":       returnSrc(tok),
			"argument":     argSrc(tok),
		}
		for pos, src := range srcs {
			t.Run(pos+"/"+tok, func(t *testing.T) {
				t.Parallel()
				_, diags := Check(parseOrFatal(t, src), Options{SchemaDir: "testdata"})
				n := 0
				for _, m := range diagMessages(diags) {
					if strings.Contains(m, want) {
						n++
					}
				}
				if !diags.HasErrors() || n != 1 {
					t.Fatalf("an empty token must be reported exactly once as %q, got %v", want, diagMessages(diags))
				}
			})
		}
	}
}
