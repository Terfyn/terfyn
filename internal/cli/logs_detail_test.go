package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Terfyn/terfyn/internal/state"
)

// `terfyn logs --detail` expands each event's stored substance (issue #525) below its table row,
// reusing the same renderer as the live --verbose stream.
func TestFormatTraceTable_detailExpandsStoredSubstance(t *testing.T) {
	events := []state.TraceEvent{
		{
			Seq: 1, Timestamp: time.Unix(0, 0).UTC(), Type: "tool_selection", ActorType: "agent", StepID: "impl",
			DataJSON: `{"uses":"tool.workspace.edit","tool":"workspace","argumentsDigest":"abc","args":{"path":"foo.go","old_string":"a","new_string":"b"}}`,
		},
	}

	// Default (no detail): the row is present, the diff sub-lines are not.
	plain := formatTraceTable(events, false)
	if !strings.Contains(plain, "tool_selection") {
		t.Fatalf("plain table missing event: %q", plain)
	}
	if strings.Contains(plain, "+b") || strings.Contains(plain, "-a") {
		t.Fatalf("plain table must not expand the diff: %q", plain)
	}

	// --detail: the edit diff is expanded.
	detailed := formatTraceTable(events, true)
	if !strings.Contains(detailed, "foo.go") || !strings.Contains(detailed, "-a") || !strings.Contains(detailed, "+b") {
		t.Fatalf("detail table did not expand the diff: %q", detailed)
	}
}

// An event without stored detail (a run recorded without --trace-detail) expands to nothing.
func TestTraceEventDetailLines_noDetail(t *testing.T) {
	e := state.TraceEvent{Type: "tool_selection", DataJSON: `{"uses":"tool.x.y","argumentsDigest":"abc"}`}
	if got := traceEventDetailLines(e); got != nil {
		t.Fatalf("expected no detail lines, got %v", got)
	}
	// Malformed/empty DataJSON must not panic.
	if got := traceEventDetailLines(state.TraceEvent{Type: "tool_selection", DataJSON: "not json"}); got != nil {
		t.Fatalf("malformed data must yield no lines, got %v", got)
	}
}
