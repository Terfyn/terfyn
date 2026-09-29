package inspect

import (
	"encoding/json"
	"time"

	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/trace"
)

func stepsToRecords(steps []state.RunStep) []StepRecord {
	out := make([]StepRecord, 0, len(steps))
	for _, s := range steps {
		rec := StepRecord{
			StepID:  s.StepID,
			Status:  s.Status,
			CostUsd: s.CostUSD,
		}
		if s.StartedAt != nil {
			rec.StartedAt = s.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		if s.FinishedAt != nil {
			rec.FinishedAt = s.FinishedAt.UTC().Format(time.RFC3339Nano)
		}
		if s.InputJSON != "" {
			rec.Input = json.RawMessage(s.InputJSON)
		}
		if s.OutputJSON != "" {
			rec.Output = json.RawMessage(s.OutputJSON)
		}
		if s.ErrorText != "" {
			rec.Error = s.ErrorText
		}
		out = append(out, rec)
	}
	return out
}

func checkpointsToRecords(cps []state.RunCheckpoint, redaction trace.RedactionOptions) []CheckpointRecord {
	out := make([]CheckpointRecord, 0, len(cps))
	for _, cp := range cps {
		ctxJ := cp.ContextJSON
		if ctxJ == "" {
			ctxJ = "{}"
		}
		// The checkpoint is stored raw so the interpreter can dispatch the pending call on resume, but a
		// read surface must not serve a token/password/authorization in clear — the trace masks the same
		// values (issue #408). Redact at display: every completed step's Output and the pending gate's args.
		ctxJ = redactCheckpointContext(ctxJ, redaction)
		out = append(out, CheckpointRecord{
			Seq:       cp.Seq,
			StepIndex: cp.StepIndex,
			StepID:    cp.StepID,
			Status:    cp.Status,
			CreatedAt: cp.CreatedAt.UTC().Format(time.RFC3339Nano),
			Context:   json.RawMessage(ctxJ),
		})
	}
	return out
}

// redactCheckpointContext masks sensitive values in a checkpoint context JSON for display, preserving
// structure. Best-effort: malformed JSON is returned unchanged (checkpoint context is always valid
// JSON we wrote, so this is a defensive fallback).
func redactCheckpointContext(ctxJSON string, redaction trace.RedactionOptions) string {
	var v any
	if err := json.Unmarshal([]byte(ctxJSON), &v); err != nil {
		return ctxJSON
	}
	redactWholeDocumentFrames(v, redaction)
	b, err := json.Marshal(trace.RedactValue(v, redaction))
	if err != nil {
		return ctxJSON
	}
	return string(b)
}

// redactWholeDocumentFrames masks the input of every nested subworkflow frame that
// records a whole-document call (issue #552). Such a frame's `input` is the single
// argument's VALUE — possibly a scalar or array with no key for RedactValue to
// match — so it is redacted as {inputParam: input}, the argument map the callee
// was called with: a sensitive parameter name masks the whole value, exactly as the
// run_steps input row of the same call is masked (issue #408). Frames without
// `inputParam` (argument-map inputs) are left to the key-based pass.
func redactWholeDocumentFrames(ctx any, redaction trace.RedactionOptions) {
	m, _ := ctx.(map[string]any)
	for frame, _ := m["nested"].(map[string]any); frame != nil; frame, _ = frame["nested"].(map[string]any) {
		param, _ := frame["inputParam"].(string)
		if param == "" {
			continue
		}
		if in, ok := frame["input"]; ok {
			masked, _ := trace.RedactValue(map[string]any{param: in}, redaction).(map[string]any)
			frame["input"] = masked[param]
		}
	}
}
