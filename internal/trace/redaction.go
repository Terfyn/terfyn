package trace

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

// RedactedPlaceholder replaces sensitive values in stored trace payloads (issue #110).
const RedactedPlaceholder = "[REDACTED]"

// Stable JSON field names emitted by the trace redaction pipeline.
const (
	FieldPayloadTruncated = "payload_truncated"
	FieldPayloadPreview   = "preview"
	FieldWrappedValue     = "value"
)

const (
	defaultMaxDepth        = 64
	defaultMaxBinaryBytes  = 1024
	defaultMaxStringChars  = 256
	defaultMaxPayloadBytes = 65536
)

const (
	// DetailMaxStringChars / DetailMaxPayloadBytes are the raised per-field and per-event caps used
	// when a run opts into terfyn run --trace-detail (issue #525). The audit defaults (256 chars,
	// 64 KiB) were tuned for terse digests and would cut an edit's diff or the reasoning text to a
	// stub before it could be shown; detail mode raises the ceilings so the substance actually
	// survives. Only the truncation budget changes — the redaction key set (secret masking) is
	// untouched, and these ceilings still bound the event so the DB cannot balloon.
	DetailMaxStringChars  = 8192
	DetailMaxPayloadBytes = 262144
)

// ApplyDetailBudget raises o's per-field string and per-event payload caps to at least the detail-mode
// floors, never lowering a larger configured value (issue #525). Used only for a --trace-detail run so
// the surfaced substance (diffs, reasoning, output) is not shredded by the terse audit defaults.
func ApplyDetailBudget(o RedactionOptions) RedactionOptions {
	if o.MaxStringChars < DetailMaxStringChars {
		o.MaxStringChars = DetailMaxStringChars
	}
	if o.MaxPayloadBytes < DetailMaxPayloadBytes {
		o.MaxPayloadBytes = DetailMaxPayloadBytes
	}
	return o
}

// DefaultRedactKeys is the built-in case-insensitive key set merged with project/call keys.
var DefaultRedactKeys = []string{
	"password", "secret", "credential", "token", "api_key", "apikey",
	"access_token", "refresh_token", "id_token", "session_token", "auth_token",
	"bearer", "auth", "authorization", "client_secret",
	"access_key", "access_key_id", "secret_access_key",
	"private_key", "privatekey",
}

// RedactionOptions configures sanitize → redact → truncate for trace payloads (issue #110).
type RedactionOptions struct {
	RedactKeys      []string
	MaxDepth        int
	MaxBinaryBytes  int
	MaxStringChars  int
	MaxPayloadBytes int
	// UnsafeRepr enables repr-style placeholders for unknown types (debug only; off in production).
	UnsafeRepr bool
}

// DefaultRedactionOptions returns safe defaults when project config is unset.
func DefaultRedactionOptions() RedactionOptions {
	return RedactionOptions{
		RedactKeys:      append([]string(nil), DefaultRedactKeys...),
		MaxDepth:        defaultMaxDepth,
		MaxBinaryBytes:  defaultMaxBinaryBytes,
		MaxStringChars:  defaultMaxStringChars,
		MaxPayloadBytes: defaultMaxPayloadBytes,
	}
}

// NormalizeRedactionOptions applies defaults and merges redact key lists.
func NormalizeRedactionOptions(o RedactionOptions) RedactionOptions {
	return o.normalized()
}

func (o RedactionOptions) normalized() RedactionOptions {
	d := DefaultRedactionOptions()
	if o.MaxDepth > 0 {
		d.MaxDepth = o.MaxDepth
	}
	if o.MaxBinaryBytes > 0 {
		d.MaxBinaryBytes = o.MaxBinaryBytes
	}
	if o.MaxStringChars > 0 {
		d.MaxStringChars = o.MaxStringChars
	}
	if o.MaxPayloadBytes > 0 {
		d.MaxPayloadBytes = o.MaxPayloadBytes
	}
	d.UnsafeRepr = o.UnsafeRepr
	d.RedactKeys = mergeRedactKeys(d.RedactKeys, o.RedactKeys)
	return d
}

func mergeRedactKeys(base, extra []string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(keys []string) {
		for _, k := range keys {
			k = strings.ToLower(strings.TrimSpace(k))
			if k == "" {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	add(base)
	add(extra)
	return out
}

// PrepareEventData runs sanitize → redact → truncate and returns JSON-safe event data.
// extraRedactKeys are merged with defaults and opts.RedactKeys (per-call / HITL review keys).
func PrepareEventData(data map[string]any, extraRedactKeys []string, opts RedactionOptions) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	o := opts.normalized()
	o.RedactKeys = mergeRedactKeys(o.RedactKeys, extraRedactKeys)
	sanitized := sanitizeValue(data, 0, o)
	redacted := redactValue(sanitized, o.RedactKeys)
	out, ok := redacted.(map[string]any)
	if !ok {
		out = map[string]any{FieldWrappedValue: redacted}
	}
	return truncatePayload(out, o.MaxPayloadBytes)
}

// RedactArgsDiff prepares HITL edit deltas for trace storage: sensitive change paths and
// nested from/to values are masked (from/to keys alone do not imply sensitivity).
func RedactArgsDiff(diff map[string]any, extraRedactKeys []string, opts RedactionOptions) map[string]any {
	if len(diff) == 0 {
		return map[string]any{}
	}
	o := opts.normalized()
	o.RedactKeys = mergeRedactKeys(o.RedactKeys, extraRedactKeys)
	out := make(map[string]any, len(diff))
	for path, entry := range diff {
		m, ok := entry.(map[string]any)
		if !ok {
			out[path] = entry
			continue
		}
		if keyMatchesRedact(path, o.RedactKeys) {
			out[path] = map[string]any{"from": RedactedPlaceholder, "to": RedactedPlaceholder}
			continue
		}
		payload := map[string]any{}
		if v, ok := m["from"]; ok {
			payload["from"] = v
		}
		if v, ok := m["to"]; ok {
			payload["to"] = v
		}
		out[path] = PrepareEventData(payload, nil, o)
	}
	return out
}

func sanitizeValue(v any, depth int, o RedactionOptions) any {
	if depth > o.MaxDepth {
		return fmt.Sprintf("<max depth %d exceeded>", o.MaxDepth)
	}
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = sanitizeValue(val, depth+1, o)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = sanitizeValue(val, depth+1, o)
		}
		return out
	case json.Number:
		return x.String()
	case string:
		return truncateString(x, o.MaxStringChars)
	case []byte:
		return binaryPlaceholder(x, o.MaxBinaryBytes)
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return x
	default:
		// Fall back to reflection so named string types (e.g. spec.HitlDecisionKind), typed slices
		// ([]string, []spec.HitlDecisionKind), and string-keyed maps (map[string]string) serialize to
		// their values instead of a "<type: unserialized>" placeholder (issue #376). Recurse through
		// sanitizeValue so element/value redaction and truncation still apply.
		return sanitizeReflect(x, depth, o)
	}
}

// sanitizeReflect handles values the concrete type switch in sanitizeValue does not: named scalar
// types and typed containers. It normalizes them to the same JSON-safe shapes (string, number,
// bool, []any, map[string]any) the audit chain stores, so a Go-typed payload field is recorded by
// value rather than as an opaque placeholder (issue #376).
func sanitizeReflect(v any, depth int, o RedactionOptions) any {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		// Increment depth on deref so a pointer/interface cycle is bounded by the MaxDepth guard,
		// exactly like the container cases (not reachable via JSON-derived payloads today, but
		// MaxDepth is the anti-cycle guard and this keeps it complete).
		return sanitizeValue(rv.Elem().Interface(), depth+1, o)
	case reflect.String:
		return truncateString(rv.String(), o.MaxStringChars)
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Slice, reflect.Array:
		// A byte slice is binary, not a list of numbers (mirrors the concrete []byte case).
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return binaryPlaceholder(rv.Bytes(), o.MaxBinaryBytes)
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = sanitizeValue(rv.Index(i).Interface(), depth+1, o)
		}
		return out
	case reflect.Map:
		// JSON objects require string keys; a non-string-keyed map has no faithful representation.
		if rv.Type().Key().Kind() != reflect.String {
			return unknownPlaceholder(v, o.UnsafeRepr)
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = sanitizeValue(iter.Value().Interface(), depth+1, o)
		}
		return out
	default:
		return unknownPlaceholder(v, o.UnsafeRepr)
	}
}

// RedactValue masks every sensitive key (opts.RedactKeys, matched case-insensitively as a substring)
// at any depth of v, preserving structure and without truncating or sanitizing. It is the
// display-layer redactor for data that must be stored raw but shown masked — notably a run checkpoint's
// context (the completed steps' outputs and a pending gate's args), which the interpreter needs
// verbatim to resume but which read surfaces (inspect, state show) must not serve in clear (issue #408).
func RedactValue(v any, opts RedactionOptions) any {
	return redactValue(v, opts.normalized().RedactKeys)
}

func redactValue(v any, keys []string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if keyMatchesRedact(k, keys) {
				out[k] = RedactedPlaceholder
				continue
			}
			out[k] = redactValue(val, keys)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactValue(val, keys)
		}
		return out
	default:
		return v
	}
}

func keyMatchesRedact(key string, patterns []string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if k == p || strings.Contains(k, p) {
			return true
		}
	}
	return false
}

func truncateString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return runeSafePrefix(s, max)
	}
	keep := max - 3
	head := keep / 2
	tail := keep - head
	return runeSafePrefix(s, head) + "..." + runeSafeSuffix(s, tail)
}

// runeSafePrefix returns the longest prefix of s within maxBytes that ends on a
// UTF-8 rune boundary, so a stored trace preview is never invalid UTF-8 (#386).
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

// runeSafeSuffix returns the longest suffix of s within maxBytes that starts on a
// UTF-8 rune boundary.
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

func binaryPlaceholder(b []byte, max int) string {
	if max <= 0 {
		max = defaultMaxBinaryBytes
	}
	show := b
	if len(b) > max {
		show = b[:max]
	}
	hexPreview := hex.EncodeToString(show)
	if len(b) > len(show) {
		return fmt.Sprintf("<binary: %d bytes, hex preview %d bytes: %s>", len(b), len(show), hexPreview)
	}
	return fmt.Sprintf("<binary: %d bytes, hex: %s>", len(b), hexPreview)
}

func unknownPlaceholder(v any, unsafeRepr bool) string {
	if unsafeRepr {
		return fmt.Sprintf("%v", v)
	}
	t := reflect.TypeOf(v)
	name := "unknown"
	if t != nil {
		name = t.String()
	}
	return fmt.Sprintf("<%s: unserialized>", name)
}

func truncatePayload(data map[string]any, maxBytes int) map[string]any {
	if maxBytes <= 0 {
		return data
	}
	b, err := json.Marshal(data)
	if err != nil || len(b) <= maxBytes {
		return data
	}
	preview := string(b)
	if len(preview) > maxBytes {
		preview = runeSafePrefix(preview, maxBytes) // rune boundary, not a mid-rune byte cut (#386)
	}
	return map[string]any{
		FieldPayloadTruncated: true,
		FieldPayloadPreview:   preview,
	}
}
