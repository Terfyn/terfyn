package engine

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/Terfyn/terfyn/internal/jsonnum"
	"github.com/Terfyn/terfyn/internal/render"
	"github.com/Terfyn/terfyn/internal/trace"
)

const (
	maxTruncatePasses   = 256
	minTruncatedRunes   = 8
	truncatedStringMark = "..."
)

// truncateMapInPlace shortens v so its stable JSON encoding fits within maxBytes while
// preserving the original map shape (top-level keys and nesting) where possible.
// Long string values are shortened first; whole entries are dropped only when needed.
// Live tool I/O uses this path; trace storage uses [trace.TruncateMapValue].
func truncateMapInPlace(v map[string]any, maxBytes int, opts trace.RedactionOptions) (map[string]any, int, bool, error) {
	if v == nil {
		v = map[string]any{}
	}
	orig, err := stableJSONLen(v)
	if err != nil {
		return nil, 0, false, fmt.Errorf("engine: measure map bytes: %w", err)
	}
	if maxBytes <= 0 || orig <= maxBytes {
		return cloneMapAny(v), orig, false, nil
	}

	o := trace.NormalizeRedactionOptions(opts)
	out := cloneMapAny(v)
	sanitizeMapStrings(out, o.MaxStringChars)

	truncated := false
	for pass := 0; pass < maxTruncatePasses; pass++ {
		n, err := stableJSONLen(out)
		if err != nil {
			return nil, orig, false, err
		}
		if n <= maxBytes {
			return out, orig, truncated, nil
		}
		truncated = true
		if shrinkLongestString(out) {
			continue
		}
		if dropLargestEntry(out) {
			continue
		}
		if o.MaxStringChars > minTruncatedRunes {
			o.MaxStringChars /= 2
			sanitizeMapStrings(out, o.MaxStringChars)
			continue
		}
		break
	}

	n, err := stableJSONLen(out)
	if err != nil {
		return nil, orig, false, err
	}
	if n > maxBytes {
		return nil, orig, false, fmt.Errorf("engine: cannot truncate map to %d bytes (still %d)", maxBytes, n)
	}
	return out, orig, truncated, nil
}

func stableJSONLen(v any) (int, error) {
	b, err := render.MarshalStableJSON(v)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func cloneMapAny(v map[string]any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		cp := make(map[string]any, len(v))
		for k, val := range v {
			cp[k] = val
		}
		return cp
	}
	var out map[string]any
	if err := jsonnum.Unmarshal(raw, &out); err != nil {
		cp := make(map[string]any, len(v))
		for k, val := range v {
			cp[k] = val
		}
		return cp
	}
	return out
}

func sanitizeMapStrings(v map[string]any, maxChars int) {
	if maxChars <= 0 {
		return
	}
	walkAndTruncateStrings(v, maxChars)
}

func walkAndTruncateStrings(v any, maxChars int) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok {
				x[k] = truncateRunes(s, maxChars)
				continue
			}
			walkAndTruncateStrings(val, maxChars)
		}
	case []any:
		for i, val := range x {
			if s, ok := val.(string); ok {
				x[i] = truncateRunes(s, maxChars)
				continue
			}
			walkAndTruncateStrings(val, maxChars)
		}
	}
}

// truncateRunes shortens s to a byte budget of max, keeping a head and tail around
// a "..." marker. Both cuts land on UTF-8 rune boundaries so the result is always
// valid UTF-8 — this value is dispatched to the tool (inputs) and stored (outputs),
// so a rune split here corrupts the data json.Marshal then patches with U+FFFD (#386).
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= len(truncatedStringMark) {
		return runeSafePrefix(s, max)
	}
	keep := max - len(truncatedStringMark)
	head := keep / 2
	tail := keep - head
	if head+tail > len(s) {
		return s
	}
	return runeSafePrefix(s, head) + truncatedStringMark + runeSafeSuffix(s, tail)
}

// runeSafePrefix returns the longest prefix of s that is at most maxBytes bytes and
// ends on a rune boundary (never mid-rune).
func runeSafePrefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	b := maxBytes
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b]
}

// runeSafeSuffix returns the longest suffix of s that is at most maxBytes bytes and
// starts on a rune boundary (never mid-rune).
func runeSafeSuffix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

type stringRef struct {
	container any
	key       string
	index     int
	value     string
}

func shrinkLongestString(root map[string]any) bool {
	ref, ok := findLongestString(root)
	if !ok || len(ref.value) <= minTruncatedRunes {
		return false
	}
	nextLen := len(ref.value) / 2
	if nextLen < minTruncatedRunes {
		nextLen = minTruncatedRunes
	}
	assignStringRef(ref, truncateRunes(ref.value, nextLen))
	return true
}

func findLongestString(v any) (stringRef, bool) {
	var best stringRef
	found := false
	var walk func(cur any)
	walk = func(cur any) {
		switch x := cur.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok {
					if !found || len(s) > len(best.value) {
						best = stringRef{container: x, key: k, value: s}
						found = true
					}
					continue
				}
				walk(val)
			}
		case []any:
			for i, val := range x {
				if s, ok := val.(string); ok {
					if !found || len(s) > len(best.value) {
						best = stringRef{container: x, index: i, value: s}
						found = true
					}
					continue
				}
				walk(val)
			}
		}
	}
	walk(v)
	return best, found
}

func assignStringRef(ref stringRef, s string) {
	switch c := ref.container.(type) {
	case map[string]any:
		c[ref.key] = s
	case []any:
		c[ref.index] = s
	}
}

func dropLargestEntry(root map[string]any) bool {
	if len(root) == 0 {
		return false
	}
	if len(root) > 1 {
		return dropLargestKey(root)
	}
	for k, val := range root {
		if nested, ok := val.(map[string]any); ok && dropLargestKey(nested) {
			return true
		}
		if arr, ok := val.([]any); ok {
			if shortened, ok := dropLargestSliceElem(arr); ok {
				root[k] = shortened
				return true
			}
		}
	}
	for k := range root {
		delete(root, k)
		return true
	}
	return false
}

func dropLargestKey(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	bestKey := ""
	bestSize := 0
	for k, val := range m {
		n, err := stableJSONLen(val)
		if err != nil {
			continue
		}
		if bestKey == "" || n > bestSize {
			bestKey, bestSize = k, n
		}
	}
	if bestKey == "" {
		return false
	}
	delete(m, bestKey)
	return true
}

func dropLargestSliceElem(arr []any) ([]any, bool) {
	if len(arr) == 0 {
		return arr, false
	}
	bestIdx := -1
	bestSize := 0
	for i, val := range arr {
		n, err := stableJSONLen(val)
		if err != nil {
			continue
		}
		if bestIdx < 0 || n > bestSize {
			bestIdx, bestSize = i, n
		}
	}
	if bestIdx < 0 {
		return arr, false
	}
	return append(append([]any(nil), arr[:bestIdx]...), arr[bestIdx+1:]...), true
}
