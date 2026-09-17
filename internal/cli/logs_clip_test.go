package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogClipKeepsUTF8(t *testing.T) {
	got := clipJSONForTable(strings.Repeat("🙂", 30), 96)
	if !utf8.ValidString(got) {
		t.Fatalf("clipJSONForTable returned invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected ellipsis on a clipped emoji run, got %q", got)
	}
	if len(got) > 96 {
		t.Fatalf("clipped length %d exceeds byte budget 96", len(got))
	}
}

func TestClipJSONForTableUTF8Boundaries(t *testing.T) {
	cases := []struct {
		name string
		ch   string
	}{
		{"2-byte", "é"}, // U+00E9
		{"3-byte", "あ"}, // U+3042
		{"4-byte", "🙂"}, // U+1F642
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := strings.Repeat(tc.ch, 8)
			width := utf8.RuneLen([]rune(tc.ch)[0])
			if width < 0 {
				t.Fatal("invalid rune")
			}
			// Cut on each byte of a rune in the middle of the string.
			for off := 0; off < width; off++ {
				max := 3*width + off // 3 full runes + partial next, plus room for "..."
				if max <= 3 {
					max = width + off + 3
				}
				got := clipJSONForTable(s, max)
				if !utf8.ValidString(got) {
					t.Fatalf("max=%d off=%d invalid UTF-8: %q bytes=%v", max, off, got, []byte(got))
				}
				if len(got) > max {
					t.Fatalf("max=%d off=%d length %d exceeds budget", max, off, len(got))
				}
			}
		})
	}
}
