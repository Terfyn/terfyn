package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
)

func TestReadLineHonorsDecisionEditLimit(t *testing.T) {
	// 70 KiB line well below 1 MiB maxDecisionEditJSONBytes (#556).
	want := strings.Repeat("x", 70<<10)
	got, err := readLine(strings.NewReader(want + "\n"))
	if err != nil {
		t.Fatalf("readLine rejected %d bytes below maxDecisionEditJSONBytes: %v", len(want), err)
	}
	if got != want {
		t.Fatalf("readLine returned %d bytes, want %d", len(got), len(want))
	}
}

func TestReadLine_Boundaries(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{
			name:    "scanner default 64 KiB",
			size:    64 * 1024,
			wantErr: false,
		},
		{
			name:    "application limit 1 MiB",
			size:    maxDecisionEditJSONBytes,
			wantErr: false,
		},
		{
			name:    "one byte above application limit",
			size:    maxDecisionEditJSONBytes + 1,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Repeat("a", tt.size) + "\n"
			got, err := readLine(strings.NewReader(input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readLine accepted %d bytes, want error", tt.size)
				}
				if !errors.Is(err, bytes.ErrTooLarge) && !strings.Contains(err.Error(), "token too long") {
					t.Fatalf("readLine returned unexpected error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readLine failed on %d bytes: %v", tt.size, err)
			}
			if len(got) != tt.size {
				t.Fatalf("readLine returned %d bytes, want %d", len(got), tt.size)
			}
		})
	}
}

func TestReadLine_UnexpectedEOF(t *testing.T) {
	_, err := readLine(strings.NewReader(""))
	if err == nil {
		t.Fatal("readLine accepted empty input without error")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("readLine returned error %v, want unexpected EOF", err)
	}
}

func TestPromptHitlDecision_MultiLineWithLargeEdit(t *testing.T) {
	gate := policy.HitlGate{
		Uses: "tool.demo.action",
		With: map[string]any{"orig": "val"},
		Review: policy.ResolvedHitlReview{
			Description:      "Approve action",
			AllowedDecisions: []spec.HitlDecisionKind{spec.HitlDecisionApprove, spec.HitlDecisionEdit},
			AllowedEditPaths: []string{"orig"},
		},
	}

	largeVal := strings.Repeat("v", 70<<10)
	editPayload, err := json.Marshal(map[string]any{
		"orig": largeVal,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two consecutive lines: decision "edit" followed by 70+ KiB JSON.
	input := "edit\n" + string(editPayload) + "\n"
	var out bytes.Buffer

	dec, err := promptHitlDecision(strings.NewReader(input), &out, gate)
	if err != nil {
		t.Fatalf("promptHitlDecision failed on multi-line large edit: %v", err)
	}
	if dec == nil {
		t.Fatal("promptHitlDecision returned nil decision")
	}
	if dec.Kind != spec.HitlDecisionEdit {
		t.Fatalf("decision kind = %v, want %v", dec.Kind, spec.HitlDecisionEdit)
	}
	if gotVal, ok := dec.EditedWith["orig"].(string); !ok || gotVal != largeVal {
		t.Fatalf("edited value mismatch: got len %d, want len %d", len(gotVal), len(largeVal))
	}
}
