package local

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/runtime"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
)

// runCmp starts the bigint fixture's `cmp` workflow through the real local runtime with a
// serialized JSON input — the same ingress `terfyn run` uses — and returns the store, run id and
// the Invoke error.
func runCmp(t *testing.T, runID, inputJSON string) (*sqlite.Store, *Runtime, error) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "bigint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt := NewRuntime(st)
	rc := testResolvedConfig(t, filepath.Join("testdata", "bigint"), "")
	_, err = rt.Invoke(ctx, rc, runtime.InvokeOptions{
		RunID: runID, WorkflowName: "cmp", Env: "dev", InputJSON: []byte(inputJSON),
	})
	return st, rt, err
}

// stepOutputs returns step id -> persisted output JSON for every succeeded run step.
func stepOutputs(t *testing.T, st *sqlite.Store, runID string) map[string]string {
	t.Helper()
	steps, err := st.ListRunStepsByRunID(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range steps {
		if s.Status == "succeeded" {
			out[s.StepID] = s.OutputJSON
		}
	}
	return out
}

func resumeCmp(t *testing.T, rt *Runtime, runID string) {
	t.Helper()
	rc := testResolvedConfig(t, filepath.Join("testdata", "bigint"), "")
	if _, err := rt.Resume(context.Background(), rc, runtime.ResumeOptions{
		RunID:        runID,
		HitlActor:    "approver",
		HitlDecision: &runtime.HitlDecisionOptions{Kind: spec.HitlDecisionApprove},
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// TestInvoke_IntegersAboveTwo53StayDistinctFromIngressThroughResume is the S7 regression for
// issue #560 at the RUNTIME boundary: 9007199254740992 and 9007199254740993 arrive as serialized
// JSON, so if ingress decoded them into float64 they would collapse to one value and `a == b`
// would be true. The run takes the not-equal arm, suspends at a HITL gate, persists a real
// checkpoint, and resumes from it — after which the memoized values must still be exactly the
// two distinct integers (a resumed run must take the same branch it would have without the
// interruption, or the interpreter reports "control flow diverged").
func TestInvoke_IntegersAboveTwo53StayDistinctFromIngressThroughResume(t *testing.T) {
	ctx := context.Background()
	const runID = "bigint-1"
	st, rt, err := runCmp(t, runID, `{"a":9007199254740992,"b":9007199254740993}`)
	if err != nil {
		t.Fatalf("invoke should interrupt cleanly at the gate, got %v", err)
	}
	if r, err := st.GetRun(ctx, runID); err != nil || r.Status != state.RunStatusInterrupted {
		t.Fatalf("run should be interrupted at the gate (a==b would have returned equal-input), got status=%q err=%v", r.Status, err)
	}

	// The persisted checkpoint is the durable, serialized state: both integers must be present
	// exactly, in the input and in the memoized pre-gate tool output.
	cp, err := st.GetLatestCheckpoint(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"a":9007199254740992`, `"b":9007199254740993`} {
		if got := strings.Count(cp.ContextJSON, want); got < 2 {
			t.Fatalf("checkpoint has %d copies of %s (want input + memo), context=%s", got, want, cp.ContextJSON)
		}
	}
	// The run row's stored input is what a resume re-reads; it must be lossless too.
	if r, _ := st.GetRun(ctx, runID); !strings.Contains(r.InputJSON, `"b":9007199254740993`) {
		t.Fatalf("stored run input lost precision: %s", r.InputJSON)
	}

	resumeCmp(t, rt, runID)

	got, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.RunStatusSucceeded {
		t.Fatalf("resume status = %q err=%q", got.Status, got.ErrorText)
	}
	steps := stepOutputs(t, st, runID)
	for _, id := range []string{"equalInput", "equalMemo", "unordered"} {
		if _, ok := steps[id]; ok {
			t.Fatalf("wrong arm taken after resume: step %q ran (steps=%v)", id, steps)
		}
	}
	// The post-resume comparison of the MEMOIZED values took the `<` arm, and the values it
	// forwarded are still the two exact integers.
	ordered, ok := steps["ordered"]
	if !ok {
		t.Fatalf("the ordered arm did not run after resume (steps=%v)", steps)
	}
	for _, want := range []string{`"a":9007199254740992`, `"b":9007199254740993`} {
		if !strings.Contains(ordered, want) {
			t.Fatalf("ordered step output missing %s: %s", want, ordered)
		}
	}
}

// TestInvoke_EqualLargeIntegersStillEqual guards the other direction: identical integers above
// 2^53 compare equal and take the equal arm (no gate, no suspend).
func TestInvoke_EqualLargeIntegersStillEqual(t *testing.T) {
	st, _, err := runCmp(t, "bigint-eq", `{"a":9007199254740993,"b":9007199254740993}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(context.Background(), "bigint-eq")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stepOutputs(t, st, "bigint-eq")["equalInput"]; !ok || got.Status != state.RunStatusSucceeded {
		t.Fatalf("status=%q steps=%v err=%q", got.Status, stepOutputs(t, st, "bigint-eq"), got.ErrorText)
	}
}

// TestInvoke_SmallIntegersAndFloatsUnaffected: 1 == 1.0 stays true, and an ordinary float pair
// round-trips through a real checkpoint/resume unchanged (fractions are float64, not truncated).
func TestInvoke_SmallIntegersAndFloatsUnaffected(t *testing.T) {
	ctx := context.Background()

	st, _, err := runCmp(t, "num-eq", `{"a":1,"b":1.0}`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRun(ctx, "num-eq")
	if _, ok := stepOutputs(t, st, "num-eq")["equalInput"]; !ok || got.Status != state.RunStatusSucceeded {
		t.Fatalf("1 == 1.0: status=%q steps=%v err=%q", got.Status, stepOutputs(t, st, "num-eq"), got.ErrorText)
	}

	st, rt, err := runCmp(t, "num-frac", `{"a":1.5,"b":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetRun(ctx, "num-frac"); r.Status != state.RunStatusInterrupted {
		t.Fatalf("status = %q", r.Status)
	}
	resumeCmp(t, rt, "num-frac")
	got, _ = st.GetRun(ctx, "num-frac")
	if got.Status != state.RunStatusSucceeded {
		t.Fatalf("resume status = %q err=%q", got.Status, got.ErrorText)
	}
	ordered, ok := stepOutputs(t, st, "num-frac")["ordered"]
	if !ok {
		t.Fatalf("ordered arm did not run: %v", stepOutputs(t, st, "num-frac"))
	}
	for _, want := range []string{`"a":1.5`, `"b":2`} {
		if !strings.Contains(ordered, want) {
			t.Fatalf("ordered output missing %s: %s", want, ordered)
		}
	}
}
