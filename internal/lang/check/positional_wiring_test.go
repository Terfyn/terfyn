package check

import (
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
)

// TestPositionalAgentArgs_LoweredGraphValidates lowers .agent source and runs the lowered graph
// through spec.ValidateProjectGraph (#575 review). A single positional agent argument lowers to
// with: {arg0: ...} with the explicit WholeDocument bit; graph validation must check it against
// the consumer's whole input, never as an input field named "arg0". Every shape — a literal, the
// workflow input, a step output — into a closed-object or scalar input must validate when well
// typed, and the checker and the graph validator must agree on every row, including the rejects
// into a false (never) input and a literal field (top-level, nested, or beside a typed token)
// that the consumer input does not declare.
func TestPositionalAgentArgs_LoweredGraphValidates(t *testing.T) {
	t.Parallel()
	schemas := map[string]string{
		"PR.json":    `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}},"additionalProperties":false}`,
		"Str.json":   `{"type":"string"}`,
		"Int.json":   `{"type":"integer"}`,
		"Never.json": `false`,
		"Nested.json": `{"type":"object","properties":{` +
			`"meta":{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}` +
			`},"additionalProperties":false}`,
	}
	const agents = `
agent reviewer {
    model mock/default
    instructions "review"
    input PR
    output PR
}

agent sstr {
    model mock/default
    instructions "string in"
    input Str
    output Str
}

agent mkint {
    model mock/default
    instructions "integer out"
    input Str
    output Int
}

agent nested {
    model mock/default
    instructions "nested in"
    input Nested
    output Str
}

agent never {
    model mock/default
    instructions "unreachable"
    input Never
}
`
	cases := []struct {
		name string
		body string // workflow W declaration
		ok   bool
		// wantErr, when set, must appear in the graph validation error.
		wantErr string
	}{
		{name: "input into closed object", body: `workflow W(input: PR) { r = reviewer(input) }`, ok: true},
		{name: "input into scalar", body: `workflow W(input: Str) { r = sstr(input) }`, ok: true},
		{name: "string literal into scalar", body: `workflow W(input: Str) { r = sstr("hi") }`, ok: true},
		{name: "number literal into scalar", body: `workflow W(input: Str) { r = sstr(42) }`, ok: true},
		{name: "object literal into closed object", body: `workflow W(input: Str) { r = reviewer({title: "hi"}) }`, ok: true},
		{name: "step output into closed object", body: `workflow W(input: PR) { a = reviewer(input)  b = reviewer(a) }`, ok: true},
		{name: "step output into scalar", body: `workflow W(input: Str) { a = sstr(input)  b = sstr(a) }`, ok: true},
		{name: "ill-typed step output into scalar", body: `workflow W(input: Str) { a = mkint(input)  b = sstr(a) }`, wantErr: `does not match Agent/sstr input "input"`},
		{name: "nested literals into closed objects", body: `workflow W(input: Str) { r = nested({meta: {x: "a"}}) }`, ok: true},
		{name: "undeclared literal field", body: `workflow W(input: Str) { r = reviewer({title: "hi", bogus: 1}) }`, wantErr: `input field "bogus" is not declared in Agent/reviewer input schema`},
		{name: "undeclared object literal field", body: `workflow W(input: Str) { r = reviewer({title: "hi", bogus: {y: true}}) }`, wantErr: `input field "bogus" is not declared in Agent/reviewer input schema`},
		{name: "undeclared literal field beside a typed token", body: `workflow W(input: PR) { a = reviewer(input)  b = reviewer({title: a.title, bogus: null}) }`, wantErr: `input field "bogus" is not declared in Agent/reviewer input schema`},
		{name: "nested undeclared literal field", body: `workflow W(input: Str) { r = nested({meta: {x: "a", bogus: 1}}) }`, wantErr: `input field "meta.bogus" is not declared in Agent/nested input schema`},
		{name: "object literal into scalar", body: `workflow W(input: Str) { r = sstr({q: "hi"}) }`, wantErr: `input field "q" is not declared in Agent/sstr input schema`},
		{name: "string literal into never", body: `workflow W(input: Str) { never("hi") }`, wantErr: "literal value (any) does not match Agent/never input"},
		{name: "empty object literal into never", body: `workflow W(input: Str) { never({}) }`, wantErr: "literal value (any) does not match Agent/never input"},
		{name: "object literal into never", body: `workflow W(input: Str) { never({q: "hi"}) }`},
		{name: "input into never", body: `workflow W(input: Str) { never(input) }`},
		{name: "step output into never", body: `workflow W(input: Str) { a = sstr(input)  never(a) }`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for name, body := range schemas {
				writeParitySchema(t, root, name, body)
			}
			prog, diags := Check(parseOrFatal(t, agents+tc.body+"\n"), Options{SchemaDir: root})
			if prog == nil || prog.Graph == nil {
				t.Fatalf("no lowered graph (diags %v)", diagMessages(diags))
			}
			wholeDocumentSteps := 0
			for _, st := range prog.Graph.Workflows["W"].Spec.Steps {
				if st.WholeDocument {
					wholeDocumentSteps++
				}
			}
			if wholeDocumentSteps == 0 {
				t.Fatalf("expected lowering to mark the positional call WholeDocument")
			}
			errs := spec.ValidateProjectGraph(prog.Graph, root)
			checkOK, graphOK := !diags.HasErrors(), errs == nil
			if graphOK != tc.ok {
				t.Fatalf("ValidateProjectGraph accepted=%v, want %v (errs %v)", graphOK, tc.ok, errs)
			}
			if checkOK != graphOK {
				t.Fatalf(".agent checker accepted=%v (%v) but ValidateProjectGraph accepted=%v (%v)",
					checkOK, diagMessages(diags), graphOK, errs)
			}
			if errs != nil {
				if strings.Contains(errs.Error(), `"arg0"`) {
					t.Fatalf("a whole-document argument must never be reported as field arg0: %v", errs)
				}
				if tc.wantErr != "" && !strings.Contains(errs.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, errs)
				}
			}
		})
	}
}
