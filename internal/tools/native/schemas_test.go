package native

import (
	"encoding/json"
	"testing"
)

func TestOperationInputSchema(t *testing.T) {
	// Each declared schema must be valid JSON describing an object, and the ones the
	// native handlers require arguments for must mark them required.
	wantRequired := map[string][]string{
		"read_file":                 {"path"},
		"write_file":                {"path", "content"},
		"edit":                      {"path", "old_string", "new_string"},
		"pull_request.get":          {"owner", "repo", "number"},
		"pull_request.diff":         {"owner", "repo", "number"},
		"pull_request.post_comment": {"owner", "repo", "number", "body"},
		"check_runs.list":           {"owner", "repo", "ref"},
		"create_branch":             {"name"},
		"push_branch":               {"branch"},
	}
	for op, req := range wantRequired {
		raw, ok := OperationInputSchema(op)
		if !ok {
			t.Errorf("OperationInputSchema(%q) not found", op)
			continue
		}
		var doc struct {
			Type       string          `json:"type"`
			Properties map[string]any  `json:"properties"`
			Required   []string        `json:"required"`
			_          json.RawMessage `json:"-"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s: invalid JSON schema: %v", op, err)
			continue
		}
		if doc.Type != "object" {
			t.Errorf("%s: type = %q, want object", op, doc.Type)
		}
		got := map[string]bool{}
		for _, r := range doc.Required {
			got[r] = true
		}
		for _, r := range req {
			if !got[r] {
				t.Errorf("%s: missing required arg %q (required=%v)", op, r, doc.Required)
			}
			if _, ok := doc.Properties[r]; !ok {
				t.Errorf("%s: required arg %q not in properties", op, r)
			}
		}
	}

	// run_tests is a known op with no required args.
	if _, ok := OperationInputSchema("run_tests"); !ok {
		t.Errorf("run_tests should have a (no-arg) schema")
	}
	for _, op := range []string{"diff", "status"} {
		if _, ok := OperationInputSchema(op); !ok {
			t.Errorf("%s should have a (no-required-arg) schema", op)
		}
	}
	// An unknown op has no schema (caller advertises the permissive default).
	if _, ok := OperationInputSchema("definitely_not_an_op"); ok {
		t.Errorf("unknown op should return ok=false")
	}
}
