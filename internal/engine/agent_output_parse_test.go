package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/models"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
)

func TestRecoverAgentJSONObject(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// Clean input is returned verbatim (fast path).
		{"clean object unchanged", `{"x":"ok"}`, `{"x":"ok"}`, false},
		{"leading and trailing space", "  {\"x\":\"ok\"}\n", `{"x":"ok"}`, false},
		// Exactly one recoverable object → recovered.
		{"prose preamble", "Perfect, here is the state:\n" + `{"x":"ok"}`, `{"x":"ok"}`, false},
		{"json code fence", "```json\n{\"x\":\"ok\"}\n```", `{"x":"ok"}`, false},
		{"bare code fence", "```\n{\"x\":\"ok\"}\n```", `{"x":"ok"}`, false},
		{"preamble and fence", "Sure!\n```json\n{\"x\":\"ok\"}\n```\nDone.", `{"x":"ok"}`, false},
		{"nested object counts once", `noise {"a":{"b":1},"c":"}"}` + " trailing", `{"a":{"b":1},"c":"}"}`, false},
		{"brace inside string is not a close", `text {"path":"a}b","x":1} end`, `{"path":"a}b","x":1}`, false},
		{"skips an unbalanced prose brace", "use {x} format:\n" + `{"x":"ok"}`, `{"x":"ok"}`, false},
		{"skips an unclosed prose brace", "note: use { to open\n" + `{"x":"ok"}`, `{"x":"ok"}`, false},
		// Zero recoverable objects → trimmed unchanged (caller reports the original parse error).
		{"no object at all", "just prose, no json", "just prose, no json", false},
		{"unbalanced object", `{"x":`, `{"x":`, false},
		{"empty", "   ", "", false},
		// Two or more candidate objects → fail closed rather than guess which is the state.
		{"two valid objects", `first {"x":"a"} then {"x":"b"}`, "", true},
		{"empty-object decoy before answer", "use {} format:\n" + `{"x":"ok"}`, "", true},
		{"example object before answer", "I considered " + `{"x":"wrong"}` + " but decided:\n" + `{"x":"ok"}`, "", true},
		{"array of examples then answer", `Examples: [{"x":"no"}]` + "\n" + `{"x":"ok"}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := recoverAgentJSONObject(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("recoverAgentJSONObject(%q) = %q, want ambiguity error", tc.in, got)
				}
				if !strings.Contains(err.Error(), "ambiguous") {
					t.Fatalf("error = %v, want an ambiguity error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("recoverAgentJSONObject(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("recoverAgentJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// completeAgentOutput must recover a schema-valid object from a completion that carries a prose
// preamble or a single fenced object, instead of failing on the first non-JSON character (issue #510).
func TestCompleteAgentOutput_recoversPreambleAndFence(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "a"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./out.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./out.json": strictInputSchema}}
	step := spec.WorkflowStep{ID: "s1"}

	cases := []struct {
		name    string
		content string
	}{
		{"clean", `{"x":"ok"}`},
		{"preamble", "Perfect. Here is the state:\n{\"x\":\"ok\"}"},
		{"json fence", "```json\n{\"x\":\"ok\"}\n```"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := e.completeAgentOutput(context.Background(), policy.NewEvaluator(nil, nil), agent, step, tc.content, models.GenerateMeta{})
			if err != nil {
				t.Fatalf("completeAgentOutput(%q) unexpected error: %v", tc.content, err)
			}
			if out["x"] != "ok" {
				t.Fatalf("completeAgentOutput(%q) = %v, want x=ok", tc.content, out)
			}
		})
	}
}

// A preamble around output that does not satisfy the schema fails with the SCHEMA violation on the
// recovered object — not a stray-character parse error on the raw completion.
func TestCompleteAgentOutput_recoveredButSchemaInvalid(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "a"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./out.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./out.json": strictInputSchema}}
	step := spec.WorkflowStep{ID: "s1"}
	_, _, err := e.completeAgentOutput(context.Background(), policy.NewEvaluator(nil, nil), agent, step, "Here:\n{\"y\":1}", models.GenerateMeta{})
	if err == nil {
		t.Fatal("expected schema violation on recovered object")
	}
	if !strings.Contains(err.Error(), "agent output") {
		t.Fatalf("error = %v, want the agent-output schema-validation path", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("error = %v, want a schema violation, not a stray-character parse error", err)
	}
}

// Two candidate objects fail the step closed, before schema validation, rather than shipping the
// leftmost span as state (issue #510).
func TestCompleteAgentOutput_ambiguousFailsClosed(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "a"},
		Spec:     spec.AgentSpec{Output: &spec.AgentIO{Schema: "./out.json"}},
	}
	e := &Executor{PinnedGraph: true, Schemas: map[string]string{"./out.json": strictInputSchema}}
	step := spec.WorkflowStep{ID: "s1"}
	// Both objects satisfy the schema; the old first-wins would have shipped {"x":"a"}.
	out, _, err := e.completeAgentOutput(context.Background(), policy.NewEvaluator(nil, nil), agent, step, "first {\"x\":\"a\"} then {\"x\":\"b\"}", models.GenerateMeta{})
	if err == nil {
		t.Fatalf("expected ambiguity error, got output %v", out)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want an ambiguity error", err)
	}
}

func TestParseAgentJSONObject(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		want       map[string]any
		wantErrSub string
	}{
		{
			name:    "valid object",
			content: `{"key":"val"}`,
			want:    map[string]any{"key": "val"},
		},
		{
			name:    "empty object",
			content: `{}`,
			want:    map[string]any{},
		},
		{
			name:    "nested object",
			content: `{"nested":{"a":1}}`,
			want:    map[string]any{"nested": map[string]any{"a": float64(1)}},
		},
		{
			name:    "whitespace-wrapped valid object",
			content: "  \n\t{\"x\":\"ok\"}  \n",
			want:    map[string]any{"x": "ok"},
		},
		{
			name:       "null rejected",
			content:    "null",
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "whitespace-wrapped null rejected",
			content:    "  \n null \t \n",
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "array rejected",
			content:    `[1, 2]`,
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "string scalar rejected",
			content:    `"hello"`,
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "number scalar rejected",
			content:    `42`,
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "boolean true rejected",
			content:    `true`,
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "boolean false rejected",
			content:    `false`,
			wantErrSub: "engine: agent response is not a JSON object",
		},
		{
			name:       "empty string rejected",
			content:    "",
			wantErrSub: "engine: empty agent response",
		},
		{
			name:       "whitespace-only rejected",
			content:    "   \n\t  ",
			wantErrSub: "engine: empty agent response",
		},
		{
			name:       "malformed JSON rejected",
			content:    `{"unclosed":`,
			wantErrSub: "engine: agent response is not a JSON object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgentJSONObject(tc.content)
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("parseAgentJSONObject(%q) = %#v, want error containing %q", tc.content, got, tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("parseAgentJSONObject(%q) error = %v, want substring %q", tc.content, err, tc.wantErrSub)
				}
				if got != nil {
					t.Fatalf("parseAgentJSONObject(%q) unexpected non-nil result on error: %#v", tc.content, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAgentJSONObject(%q) unexpected error: %v", tc.content, err)
			}
			if got == nil {
				t.Fatalf("parseAgentJSONObject(%q) returned nil map, want non-nil map", tc.content)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseAgentJSONObject(%q) = %#v, want %#v", tc.content, got, tc.want)
			}
		})
	}
}

func TestCompleteAgentOutput_nullRejectedWithoutSchema(t *testing.T) {
	agent := &spec.AgentResource{
		Metadata: spec.Metadata{Name: "a"},
		Spec:     spec.AgentSpec{},
	}
	e := &Executor{}
	step := spec.WorkflowStep{ID: "s1"}

	for _, content := range []string{"null", "  null\n"} {
		t.Run(content, func(t *testing.T) {
			out, _, err := e.completeAgentOutput(context.Background(), policy.NewEvaluator(nil, nil), agent, step, content, models.GenerateMeta{})
			if err == nil {
				t.Fatalf("completeAgentOutput(%q) succeeded with %#v, want rejection of null", content, out)
			}
			if !strings.Contains(err.Error(), "engine: agent response is not a JSON object") {
				t.Fatalf("completeAgentOutput(%q) error = %v, want object error", content, err)
			}
			if out != nil {
				t.Fatalf("completeAgentOutput(%q) returned non-nil output %#v on error", content, out)
			}
		})
	}
}
