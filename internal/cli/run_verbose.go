package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Terfyn/terfyn/internal/trace"
)

// newVerboseSink returns a [trace.EventSink] that renders each streamed trace event as one human
// line on w — the `terfyn run --verbose` live view (issue #450). It writes to stderr so `-o json`
// stdout stays clean and parseable. Only the events worth watching live are rendered (model
// completions, tool selection/execution, limit hits, approval pauses, errors); the rest are skipped
// to keep the stream focused. noColor swaps the leading glyphs for ASCII.
//
// When detail is true (--trace-detail, issue #525) each rendered event is followed by indented
// sub-lines carrying its substance: the agent's reasoning text, the tool call arguments (an edit as a
// unified diff), and a bounded tool output. The fields are present only because the run was recorded
// with --trace-detail; a run without it streams the terse one-liners regardless.
func newVerboseSink(w io.Writer, noColor, detail bool) trace.EventSink {
	return func(ev trace.StreamEvent) {
		line, ok := formatVerboseEvent(ev, noColor)
		if !ok {
			return
		}
		fmt.Fprintln(w, line)
		if detail {
			for _, sub := range verboseDetailLines(ev) {
				fmt.Fprintln(w, sub)
			}
		}
	}
}

type verboseMark int

const (
	markStep verboseMark = iota
	markWarn
	markErr
	markPause
)

func markGlyph(m verboseMark, noColor bool) string {
	if noColor {
		switch m {
		case markWarn:
			return "*"
		case markErr:
			return "x"
		case markPause:
			return "#"
		default:
			return "-"
		}
	}
	switch m {
	case markWarn:
		return "⚠"
	case markErr:
		return "✗"
	case markPause:
		return "⏸"
	default:
		return "▸"
	}
}

// formatVerboseEvent renders one streamed event, or (,"",false) when the event kind is not part of
// the live view.
func formatVerboseEvent(ev trace.StreamEvent, noColor bool) (string, bool) {
	label := verboseLabel(ev)
	switch ev.Type {
	case trace.EventLLMCompletion:
		return verboseLine(noColor, markStep, label, "llm_completion", verboseCost(ev.Data)), true
	case trace.EventToolSelection:
		return verboseLine(noColor, markStep, label, "tool_selection", stringField(ev.Data, "uses", "tool")), true
	case trace.EventToolExecution:
		status := "ok"
		if ok, _ := ev.Data["success"].(bool); !ok {
			status = "err"
		}
		detail := joinNonEmpty("  ", stringField(ev.Data, "uses", "tool"), status, verboseDuration(ev.Data))
		return verboseLine(noColor, markStep, label, "tool_execution", detail), true
	case trace.EventLimitHit:
		return verboseLine(noColor, markWarn, label, "limit_hit", stringField(ev.Data, "kind")), true
	case trace.EventRunError, trace.EventSystemError:
		// Show the stable denial/failure REASON code when present. Never stream the raw "error"
		// field: run_error/system_error store err.Error() (local finish and runErrorTraceData both
		// do), and key-based redaction does not scrub a value under a key named "error" — so an
		// adapter string with api_key=/a URL would become a stderr/CI line. Fall back to a stable
		// token, mirroring how tool_execution refuses to persist Error() (ToolCallFailedReason).
		detail := stringField(ev.Data, "reason")
		if detail == "" {
			detail = "run_failed"
			if ev.Type == trace.EventSystemError {
				detail = "system_error"
			}
		}
		return verboseLine(noColor, markErr, label, string(ev.Type), detail), true
	case trace.EventHitlRequestCreated:
		uses := stringField(ev.Data, "uses")
		return verboseLine(noColor, markPause, label, "approval", strings.TrimSpace("approval required: "+uses)), true
	default:
		return "", false
	}
}

const (
	// verboseDetailIndent prefixes every substance sub-line so it reads as a child of its event.
	verboseDetailIndent = "      ⤷ "
	// verboseDetailWidth clips each sub-line for terminal readability; the stored value is already
	// bounded by the recorder's per-field truncation, this only keeps the live view tidy.
	verboseDetailWidth = 120
	// verboseDetailMaxLines caps how many lines one field expands to (a big diff / long output),
	// so a single event cannot flood the stream.
	verboseDetailMaxLines = 20
)

// verboseDetailLines renders the substance fields (#525) of one event as indented sub-lines: the
// reasoning text on llm_completion, the arguments (an edit as a unified diff) on tool_selection, and
// the output on tool_execution. It returns nil when the event carries no detail (a run recorded
// without --trace-detail, or a field that was empty).
func verboseDetailLines(ev trace.StreamEvent) []string {
	return detailLinesFor(ev.Type, ev.Data)
}

// detailLinesFor renders the substance sub-lines for one event's data, shared by the live --verbose
// stream and `terfyn logs --detail` (#525). It returns nil when the event carries no detail.
func detailLinesFor(evType trace.EventType, data map[string]any) []string {
	switch evType {
	case trace.EventLLMCompletion:
		if text := stringField(data, trace.FieldCompletionText); text != "" {
			return detailTextLines("", text)
		}
	case trace.EventToolSelection:
		if args, ok := data[trace.FieldToolArgs].(map[string]any); ok {
			return detailArgsLines(stringField(data, "uses", "tool"), args)
		}
	case trace.EventToolExecution:
		if out, ok := data[trace.FieldToolOutput].(map[string]any); ok {
			return detailMapLines(out)
		}
	}
	return nil
}

// detailArgsLines renders a tool call's arguments. An edit (old_string→new_string) is shown as a
// unified diff — the single most useful thing to see (#525) — everything else as key: value lines.
func detailArgsLines(uses string, args map[string]any) []string {
	if old, oOK := args["old_string"].(string); oOK {
		if nw, nOK := args["new_string"].(string); nOK {
			path, _ := args["path"].(string)
			return detailEditDiffLines(path, old, nw)
		}
	}
	return detailMapLines(args)
}

// detailEditDiffLines renders an edit as a compact unified-diff hunk: the path, then old lines
// prefixed '-' and new lines prefixed '+'. Bounded by verboseDetailMaxLines.
func detailEditDiffLines(path, old, nw string) []string {
	var lines []string
	if strings.TrimSpace(path) != "" {
		lines = append(lines, verboseDetailIndent+"edit "+path)
	}
	add := func(sign, s string) {
		for _, ln := range strings.Split(s, "\n") {
			lines = append(lines, verboseDetailIndent+sign+clipDetail(ln))
		}
	}
	add("-", old)
	add("+", nw)
	return capDetailLines(lines)
}

// detailMapLines renders a map as sorted `key: value` sub-lines, each clipped. A multi-line string
// value (e.g. a run_tests stdout tail) is kept across lines under a `key:` header so its structure
// survives, rather than being collapsed onto one line.
func detailMapLines(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.Contains(strings.TrimRight(s, "\n"), "\n") {
			lines = append(lines, verboseDetailIndent+k+":")
			for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
				lines = append(lines, verboseDetailIndent+"  "+clipDetail(ln))
			}
			continue
		}
		lines = append(lines, verboseDetailIndent+k+": "+clipDetail(fmt.Sprintf("%v", m[k])))
	}
	return capDetailLines(lines)
}

// detailTextLines renders a free-text field (reasoning) across up to verboseDetailMaxLines lines,
// each clipped, preserving the model's own line breaks.
func detailTextLines(prefix, text string) []string {
	var lines []string
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, verboseDetailIndent+prefix+clipDetail(ln))
	}
	return capDetailLines(lines)
}

func capDetailLines(lines []string) []string {
	if len(lines) <= verboseDetailMaxLines {
		return lines
	}
	out := append([]string(nil), lines[:verboseDetailMaxLines]...)
	return append(out, verboseDetailIndent+fmt.Sprintf("… (%d more lines)", len(lines)-verboseDetailMaxLines))
}

func clipDetail(s string) string {
	r := []rune(s)
	if len(r) <= verboseDetailWidth {
		return s
	}
	return string(r[:verboseDetailWidth-1]) + "…"
}

// verboseLabel picks a short actor label: the agent name when present, else the step id, else the
// event's actor kind.
func verboseLabel(ev trace.StreamEvent) string {
	if a := stringField(ev.Data, "agent"); a != "" {
		return a
	}
	if ev.StepID != "" {
		return ev.StepID
	}
	return string(ev.Actor)
}

func verboseLine(noColor bool, m verboseMark, label, typ, detail string) string {
	line := fmt.Sprintf("%s %s %s", markGlyph(m, noColor), padRight(truncate(label, 18, noColor), 18), padRight(typ, 16))
	if detail = strings.TrimSpace(detail); detail != "" {
		line += " " + detail
	}
	return strings.TrimRight(line, " ")
}

func verboseCost(data map[string]any) string {
	if c, ok := data["costUsd"].(float64); ok && c > 0 {
		return fmt.Sprintf("$%.4f", c)
	}
	return ""
}

func verboseDuration(data map[string]any) string {
	var ms int64
	switch d := data["durationMs"].(type) {
	case float64:
		ms = int64(d)
	case int64:
		ms = d
	case int:
		ms = int64(d)
	}
	if ms <= 0 {
		return ""
	}
	return fmt.Sprintf("(%dms)", ms)
}

// stringField returns the first present, non-empty string value among keys.
func stringField(data map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func joinNonEmpty(sep string, parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func padRight(s string, n int) string {
	if len([]rune(s)) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len([]rune(s)))
}

// truncate shortens s to at most n runes, marking elision with an ellipsis. Under noColor the marker
// is ASCII "..." so the whole line stays ASCII (the "…" glyph would otherwise leak past --no-color);
// the result is still bounded by n runes in both cases, keeping column alignment.
func truncate(s string, n int, noColor bool) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if noColor {
		if n <= 3 {
			return string(r[:n])
		}
		return string(r[:n-3]) + "..."
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
