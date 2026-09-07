package trace

import (
	"strings"
	"testing"
)

// The detail budget (#525) must let a substance field survive past the terse audit default so the
// "unified diff"/reasoning this feature surfaces is not shredded to a 256-char stub.
func TestApplyDetailBudget(t *testing.T) {
	long := strings.Repeat("x", 4000)
	data := map[string]any{FieldToolArgs: map[string]any{"old_string": long}}

	// Audit default cuts it to the terse cap.
	def := PrepareEventData(data, nil, DefaultRedactionOptions())
	defArgs := def[FieldToolArgs].(map[string]any)
	if got := defArgs["old_string"].(string); len(got) >= 4000 || !strings.Contains(got, "...") {
		t.Fatalf("default cap should truncate a 4000-char field, got len=%d", len(got))
	}

	// Detail budget preserves it.
	detail := PrepareEventData(data, nil, ApplyDetailBudget(DefaultRedactionOptions()))
	detArgs := detail[FieldToolArgs].(map[string]any)
	if got := detArgs["old_string"].(string); got != long {
		t.Fatalf("detail budget should preserve a 4000-char field, got len=%d", len(got))
	}

	// It never lowers a larger configured cap.
	custom := ApplyDetailBudget(RedactionOptions{MaxStringChars: DetailMaxStringChars * 2, MaxPayloadBytes: DetailMaxPayloadBytes * 2})
	if custom.MaxStringChars != DetailMaxStringChars*2 || custom.MaxPayloadBytes != DetailMaxPayloadBytes*2 {
		t.Fatalf("detail budget must not lower a larger configured cap: %+v", custom)
	}
}

func TestAddDetailFields_skipEmpty(t *testing.T) {
	data := map[string]any{}
	AddCompletionText(data, "   ")
	AddToolArgs(data, nil)
	AddToolArgs(data, map[string]any{})
	AddToolOutput(data, nil)
	if len(data) != 0 {
		t.Fatalf("empty detail must add nothing, got %v", data)
	}

	AddCompletionText(data, "reasoning")
	AddToolArgs(data, map[string]any{"path": "x"})
	AddToolOutput(data, map[string]any{"ok": true})
	if data[FieldCompletionText] != "reasoning" {
		t.Fatalf("text not set: %v", data)
	}
	if _, ok := data[FieldToolArgs].(map[string]any); !ok {
		t.Fatalf("args not set: %v", data)
	}
	if _, ok := data[FieldToolOutput].(map[string]any); !ok {
		t.Fatalf("output not set: %v", data)
	}

	// A nil map is a no-op (no panic).
	AddCompletionText(nil, "x")
	AddToolArgs(nil, map[string]any{"a": 1})
	AddToolOutput(nil, map[string]any{"a": 1})
}
