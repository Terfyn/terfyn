package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Terfyn/terfyn/internal/engine"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/runtime"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/mattn/go-isatty"
)

// EnvHitlActor overrides the actor recorded on approval trace events.
const EnvHitlActor = "TERFYN_HITL_ACTOR"

// maxDecisionEditJSONBytes caps --decision-edit-json size (well below checkpoint limits).
const maxDecisionEditJSONBytes = 1 << 20

func hitlActorFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(EnvHitlActor)); v != "" {
		return v
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	return policy.DefaultHitlActor
}

func applyHitlInvokeOptions(opts *runtime.InvokeOptions, autoApprove bool) {
	if opts == nil {
		return
	}
	opts.AutoApprove = autoApprove || envAutoApproveEnabled()
	opts.HitlActor = hitlActorFromEnv()
}

func applyHitlResumeOptions(opts *runtime.ResumeOptions, autoApprove bool, decision string, editJSON string, switchTarget string) error {
	if opts == nil {
		return nil
	}
	opts.AutoApprove = autoApprove || envAutoApproveEnabled()
	opts.HitlActor = hitlActorFromEnv()
	decision = strings.TrimSpace(decision)
	editJSON = strings.TrimSpace(editJSON)
	switchTarget = strings.TrimSpace(switchTarget)
	if decision == "" {
		if editJSON != "" || switchTarget != "" {
			return fmt.Errorf("run: --decision-edit-json and --decision-switch-target require --decision")
		}
		return nil
	}
	hd, err := parseHitlDecisionOptions(decision, editJSON, switchTarget)
	if err != nil {
		return err
	}
	opts.HitlDecision = hd
	return nil
}

func parseHitlDecisionOptions(decision, editJSON, switchTarget string) (*runtime.HitlDecisionOptions, error) {
	kind, err := spec.ParseHitlDecisionKind(decision)
	if err != nil {
		return nil, err
	}
	hd := &runtime.HitlDecisionOptions{Kind: kind}
	switch kind {
	case spec.HitlDecisionEdit:
		if editJSON == "" {
			return nil, fmt.Errorf("run: --decision edit requires --decision-edit-json")
		}
		if len(editJSON) > maxDecisionEditJSONBytes {
			return nil, fmt.Errorf("run: --decision-edit-json exceeds %d bytes", maxDecisionEditJSONBytes)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(editJSON), &m); err != nil {
			return nil, fmt.Errorf("run: --decision-edit-json: %w", err)
		}
		if m == nil {
			return nil, fmt.Errorf("run: --decision-edit-json must be a JSON object")
		}
		hd.EditedWith = m
	case spec.HitlDecisionSwitch:
		hd.SwitchTarget = switchTarget
		if hd.SwitchTarget == "" {
			return nil, fmt.Errorf("run: --decision switch requires --decision-switch-target")
		}
	}
	return hd, nil
}

// maxHitlLineBytes bounds a single interactive HITL input line.
// bufio.Scanner measures the raw bytes in the buffer before ScanLines strips
// the line ending, so the cap must accommodate both Unix LF (+1) and Windows
// CRLF (+2) termination of a maximum-length payload.
const maxHitlLineBytes = maxDecisionEditJSONBytes + 2

func maybePromptHitlDecision(sc *bufio.Scanner, out io.Writer, gate policy.HitlGate) (*policy.HitlDecisionInput, error) {
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return nil, nil
	}
	return promptHitlDecision(sc, out, gate)
}

func promptHitlDecision(sc *bufio.Scanner, out io.Writer, gate policy.HitlGate) (*policy.HitlDecisionInput, error) {
	actor := hitlActorFromEnv()
	display := policy.RedactHitlArgs(gate.With, gate.Review.RedactKeys)
	fmt.Fprintf(out, "\n%s\n", gate.Review.Description)
	fmt.Fprintf(out, "Tool: %s\nArguments: %v\n", gate.Uses, display)
	fmt.Fprintf(out, "Allowed decisions: %v\n", gate.Review.AllowedDecisions)
	if len(gate.Review.SwitchTargets) > 0 {
		fmt.Fprintf(out, "Switch targets: %v\n", gate.Review.SwitchTargets)
	}
	if sc == nil {
		sc = newHitlScanner(os.Stdin)
	}
	for {
		fmt.Fprintf(out, "Decision [approve/reject/edit/switch]: ")
		line, err := scanHitlLine(sc)
		if err != nil {
			return nil, err
		}
		kind, err := spec.ParseHitlDecisionKind(line)
		if err != nil {
			fmt.Fprintf(out, "Unknown decision %q\n", line)
			continue
		}
		if !policy.IsDecisionAllowed(kind, gate.Review.AllowedDecisions) {
			fmt.Fprintf(out, "Decision %q is not allowed for this call\n", kind)
			continue
		}
		dec := &policy.HitlDecisionInput{Kind: kind, Actor: actor}
		switch kind {
		case spec.HitlDecisionEdit:
			fmt.Fprintf(out, "Edited args JSON: ")
			editLine, err := scanHitlLine(sc)
			if err != nil {
				return nil, err
			}
			if len(editLine) > maxDecisionEditJSONBytes {
				fmt.Fprintf(out, "Edited args exceed %d bytes\n", maxDecisionEditJSONBytes)
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(editLine), &m); err != nil {
				fmt.Fprintf(out, "Invalid JSON: %v\n", err)
				continue
			}
			if err := policy.ValidateHitlEdit(gate.With, m, gate.Review); err != nil {
				fmt.Fprintf(out, "%v\n", err)
				continue
			}
			dec.EditedWith = m
		case spec.HitlDecisionSwitch:
			fmt.Fprintf(out, "Switch target operation: ")
			target, err := scanHitlLine(sc)
			if err != nil {
				return nil, err
			}
			dec.SwitchTarget = target
		}
		return dec, nil
	}
}

func newHitlScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxHitlLineBytes)
	return sc
}

func scanHitlLine(sc *bufio.Scanner) (string, error) {
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("run: unexpected EOF reading hitl decision")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func readLine(r io.Reader) (string, error) {
	return scanHitlLine(newHitlScanner(r))
}

func hitlGateFromCheckpoint(contextJSON string) (*policy.HitlGate, error) {
	var payload struct {
		PendingHitl *engine.PendingHitlState `json:"pendingHitl,omitempty"`
		Nested      *engine.NestedRunState   `json:"nested,omitempty"`
	}
	if err := json.Unmarshal([]byte(contextJSON), &payload); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint: %w", err)
	}
	// The gate may be at the top level or, when the suspension is inside a
	// subworkflow, under the nested frame — the engine deliberately leaves the
	// top-level pendingHitl nil and stores the gate at nested[.nested…].pendingHitl
	// (suspendExecIR). Walk the nested chain to the innermost pending gate (#381).
	p := payload.PendingHitl
	for frame := payload.Nested; p == nil && frame != nil; frame = frame.Nested {
		p = frame.PendingHitl
	}
	if p == nil {
		return nil, nil
	}
	return &policy.HitlGate{
		Uses:   p.Uses,
		With:   p.With,
		Review: p.Review,
	}, nil
}

// loadPendingHitlGate reads the latest checkpoint for a run awaiting HITL input.
func loadPendingHitlGate(ctx context.Context, st state.RuntimeStore, runID string) (*policy.HitlGate, error) {
	cp, err := st.GetLatestCheckpoint(ctx, runID)
	if err != nil {
		return nil, err
	}
	return hitlGateFromCheckpoint(cp.ContextJSON)
}

// requirePendingHitlGate returns the pending gate or an error when interrupted without one.
func requirePendingHitlGate(ctx context.Context, st state.RuntimeStore, runID string) (*policy.HitlGate, error) {
	gate, err := loadPendingHitlGate(ctx, st, runID)
	if err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, fmt.Errorf("run: run %q is interrupted but checkpoint has no pending approval gate", runID)
	}
	return gate, nil
}
