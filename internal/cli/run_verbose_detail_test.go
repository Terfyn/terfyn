package cli

import (
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/trace"
)

// verboseDetailLines renders the substance of an event as indented sub-lines only under --trace-detail
// (issue #525): reasoning text, tool arguments (an edit as a unified diff), and tool output.
func TestVerboseDetailLines(t *testing.T) {
	t.Run("llm_completion reasoning text", func(t *testing.T) {
		ev := trace.StreamEvent{Type: trace.EventLLMCompletion, Data: map[string]any{
			"agent":                   "Implementer",
			trace.FieldCompletionText: "I'll add the ablation benchmark\nthen run tests",
		}}
		lines := verboseDetailLines(ev)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "ablation benchmark") || !strings.Contains(joined, "run tests") {
			t.Fatalf("reasoning not rendered: %q", joined)
		}
		if len(lines) != 2 {
			t.Fatalf("expected 2 reasoning lines, got %d: %q", len(lines), joined)
		}
	})

	t.Run("edit args render as a unified diff", func(t *testing.T) {
		ev := trace.StreamEvent{Type: trace.EventToolSelection, Data: map[string]any{
			"uses": "tool.workspace.edit",
			trace.FieldToolArgs: map[string]any{
				"path":       "framework/foo.go",
				"old_string": "old line",
				"new_string": "new line",
			},
		}}
		joined := strings.Join(verboseDetailLines(ev), "\n")
		if !strings.Contains(joined, "framework/foo.go") {
			t.Fatalf("diff missing path: %q", joined)
		}
		if !strings.Contains(joined, "-old line") || !strings.Contains(joined, "+new line") {
			t.Fatalf("diff missing +/- lines: %q", joined)
		}
	})

	t.Run("non-edit args render as key: value", func(t *testing.T) {
		ev := trace.StreamEvent{Type: trace.EventToolSelection, Data: map[string]any{
			"uses":              "tool.workspace.run_tests",
			trace.FieldToolArgs: map[string]any{"command": "go test ./..."},
		}}
		joined := strings.Join(verboseDetailLines(ev), "\n")
		if !strings.Contains(joined, "command: go test ./...") {
			t.Fatalf("run_tests command not rendered: %q", joined)
		}
	})

	t.Run("tool_execution output", func(t *testing.T) {
		ev := trace.StreamEvent{Type: trace.EventToolExecution, Data: map[string]any{
			"uses":                "tool.workspace.run_tests",
			trace.FieldToolOutput: map[string]any{"passed": true, "tail": "ok  ./..."},
		}}
		joined := strings.Join(verboseDetailLines(ev), "\n")
		if !strings.Contains(joined, "passed: true") || !strings.Contains(joined, "tail: ok  ./...") {
			t.Fatalf("output not rendered: %q", joined)
		}
	})

	t.Run("no detail fields returns nil", func(t *testing.T) {
		ev := trace.StreamEvent{Type: trace.EventToolSelection, Data: map[string]any{"uses": "tool.x.y"}}
		if got := verboseDetailLines(ev); got != nil {
			t.Fatalf("expected no detail lines, got %v", got)
		}
	})
}

// A big diff/output must not flood the stream: the renderer caps expanded lines.
func TestVerboseDetailLines_capped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line\n")
	}
	ev := trace.StreamEvent{Type: trace.EventLLMCompletion, Data: map[string]any{
		trace.FieldCompletionText: b.String(),
	}}
	lines := verboseDetailLines(ev)
	if len(lines) > verboseDetailMaxLines+1 {
		t.Fatalf("expected capped at %d(+summary), got %d", verboseDetailMaxLines, len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "more lines") {
		t.Fatalf("expected a truncation summary line, got %q", lines[len(lines)-1])
	}
}
