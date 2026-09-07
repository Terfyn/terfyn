package trace

import "testing"

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
