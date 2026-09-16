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
	// The scanner buffer is maxDecisionEditJSONBytes+2 to accommodate both LF (+1)
	// and CRLF (+2) endings on a maximum-size payload. Content above
	// maxDecisionEditJSONBytes is still rejected by the application-layer check in
	// parseHitlDecisionOptions / promptHitlDecision; the scanner's contract is that
	// it does NOT truncate a valid-sized payload regardless of line-ending style.
	tests := []struct {
		name    string
		size    int
		lineEnd string
		wantErr bool
	}{
		{
			name:    "64 KiB payload LF — well below limit",
			size:    64 * 1024,
			lineEnd: "\n",
			wantErr: false,
		},
		{
			name:    "64 KiB payload CRLF — well below limit",
			size:    64 * 1024,
			lineEnd: "\r\n",
			wantErr: false,
		},
		{
			name:    "exact application limit LF",
			size:    maxDecisionEditJSONBytes,
			lineEnd: "\n",
			wantErr: false,
		},
		{
			name:    "exact application limit CRLF",
			size:    maxDecisionEditJSONBytes,
			lineEnd: "\r\n",
			wantErr: false,
		},
		{
			// maxDecisionEditJSONBytes+1 bytes + "\r\n" = maxDecisionEditJSONBytes+3
			// raw bytes, which exceeds the scanner cap of maxDecisionEditJSONBytes+2.
			name:    "one byte above limit CRLF",
			size:    maxDecisionEditJSONBytes + 1,
			lineEnd: "\r\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Repeat("a", tt.size) + tt.lineEnd
			got, err := readLine(strings.NewReader(input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readLine accepted %d bytes (%q ending), want error", tt.size, tt.lineEnd)
				}
				if !errors.Is(err, bytes.ErrTooLarge) && !strings.Contains(err.Error(), "token too long") {
					t.Fatalf("readLine returned unexpected error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readLine failed on %d bytes (%q ending): %v", tt.size, tt.lineEnd, err)
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
