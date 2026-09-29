package inspect

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/trace"
)

// TestCheckpointsToRecords_redactsContext is the read-layer half of the #408 fix: a run checkpoint is
// stored raw (the interpreter needs the real args to dispatch on resume), but the inspect API must not
// serve a token/password/authorization value in clear. checkpointsToRecords redacts the completed
// steps' outputs and the pending gate's args before serving, preserving structure and non-sensitive
// values.
func TestCheckpointsToRecords_redactsContext(t *testing.T) {
	ctx := `{
      "steps": {"prep": {"Output": {"echo": {"token": "sekret-123", "topic": "hi"}}}},
      "pendingHitl": {"stepId": "publish", "with": {"body": {"authorization": "Bearer abc", "note": "ok"}}}
    }`
	recs := checkpointsToRecords(
		[]state.RunCheckpoint{{Seq: 1, StepID: "publish", Status: "interrupted", ContextJSON: ctx}},
		trace.NormalizeRedactionOptions(trace.DefaultRedactionOptions()),
	)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	got := string(recs[0].Context)

	// Sensitive values are masked...
	if strings.Contains(got, "sekret-123") || strings.Contains(got, "Bearer abc") {
		t.Fatalf("checkpoint context served a sensitive value in clear:\n%s", got)
	}
	if !strings.Contains(got, trace.RedactedPlaceholder) {
		t.Fatalf("expected the redaction placeholder in the served context:\n%s", got)
	}
	// ...while non-sensitive values and structure survive.
	var m map[string]any
	if err := json.Unmarshal(recs[0].Context, &m); err != nil {
		t.Fatalf("redacted context is not valid JSON: %v\n%s", err, got)
	}
	steps := m["steps"].(map[string]any)
	prep := steps["prep"].(map[string]any)
	echo := prep["Output"].(map[string]any)["echo"].(map[string]any)
	if echo["topic"] != "hi" {
		t.Fatalf("non-sensitive step output value must survive: %+v", echo)
	}
	if echo["token"] != trace.RedactedPlaceholder {
		t.Fatalf("step output token must be redacted: %+v", echo)
	}
	pend := m["pendingHitl"].(map[string]any)["with"].(map[string]any)["body"].(map[string]any)
	if pend["note"] != "ok" || pend["authorization"] != trace.RedactedPlaceholder {
		t.Fatalf("pending gate args not redacted correctly: %+v", pend)
	}
}

// TestCheckpointsToRecords_malformedContextPassesThrough: a non-JSON context is served unchanged
// (defensive — checkpoint context is always valid JSON we wrote).
func TestCheckpointsToRecords_malformedContextPassesThrough(t *testing.T) {
	recs := checkpointsToRecords(
		[]state.RunCheckpoint{{Seq: 1, ContextJSON: "not json"}},
		trace.NormalizeRedactionOptions(trace.DefaultRedactionOptions()),
	)
	if string(recs[0].Context) != "not json" {
		t.Fatalf("malformed context should pass through, got %s", recs[0].Context)
	}
}

// TestCheckpointsToRecords_redactsWholeDocumentNestedInput: a nested frame of a whole-document
// subworkflow call (#552) stores the single argument's VALUE as its input — a scalar or array with no
// key of its own — plus the parameter name as inputParam. The served context must mask that value
// when the parameter name is sensitive (#408), at every nesting level, and leave a non-sensitive
// parameter's document alone.
func TestCheckpointsToRecords_redactsWholeDocumentNestedInput(t *testing.T) {
	ctx := `{
      "input": {"doc": "d"},
      "nested": {
        "stepId": "a", "workflow": "Deploy", "inputParam": "token", "input": "s3cr3t-value",
        "nested": {
          "stepId": "b", "workflow": "Deploy2", "inputParam": "password", "input": ["p1", "p2"],
          "nested": {"stepId": "c", "workflow": "Echo", "inputParam": "value", "input": {"note": "ok", "apiKey": "k-1"}}
        }
      }
    }`
	recs := checkpointsToRecords(
		[]state.RunCheckpoint{{Seq: 1, StepID: "a", Status: "interrupted", ContextJSON: ctx}},
		trace.NormalizeRedactionOptions(trace.DefaultRedactionOptions()),
	)
	got := string(recs[0].Context)
	for _, secret := range []string{"s3cr3t-value", "p1", "p2", "k-1"} {
		if strings.Contains(got, secret) {
			t.Fatalf("checkpoint context served %q in clear:\n%s", secret, got)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(recs[0].Context, &m); err != nil {
		t.Fatal(err)
	}
	a := m["nested"].(map[string]any)
	b := a["nested"].(map[string]any)
	c := b["nested"].(map[string]any)
	if a["input"] != trace.RedactedPlaceholder || b["input"] != trace.RedactedPlaceholder {
		t.Fatalf("whole-document inputs under a sensitive parameter must be masked: a=%v b=%v", a["input"], b["input"])
	}
	cin := c["input"].(map[string]any)
	if cin["note"] != "ok" || cin["apiKey"] != trace.RedactedPlaceholder {
		t.Fatalf("a non-sensitive parameter's document keeps key-based redaction only: %+v", cin)
	}
}
