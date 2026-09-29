// Package jsonnum is the one canonical, lossless JSON-number model for runtime
// values that flow through the execution IR (docs/SOUNDNESS.md S7: replay must
// be deterministic, so a value must have the same representation on a fresh run,
// after a checkpoint round trip, and after resume).
//
// encoding/json decodes every number into float64 when the target is `any`, which
// silently rounds any integer past 2^53 (9007199254740992 and 9007199254740993
// both become 9007199254740992). Decoding runtime state with [Unmarshal] instead
// keeps the literal (json.Number) and then normalizes it to the canonical form:
//
//   - a plain integer literal ("7", "-9007199254740993") in [-2^63, 2^63) is that
//     exact int64;
//   - a plain integer literal outside int64 is its binary64 rounding, and stays a
//     float64 on both sides: "9223372036854775808" is float64(2^63) and
//     "-9223372036854775809" is float64(-2^63), never int64(MinInt64);
//   - any other literal (with a fraction or exponent) is rounded to binary64 and
//     then canonicalized as a float64 VALUE (below);
//   - a literal that does not fit float64 is an error, as with encoding/json.
//
// The canonical form of a float64 is a function of its exact VALUE, never of its
// decimal spelling: a whole float64 strictly inside (-2^63, 2^63) is exactly
// int64(f) ("7.0", "7e0" and float64(7) are int64(7); float64(2^60) is
// int64(1152921504606846976)), and every other float64 (fractions, ±2^63 and
// beyond) stays float64. Canonicalization therefore never changes a number's
// value, so the exact comparator in internal/execir sees the integer the input
// spelled.
//
// The S7 invariant is that canonical values are checkpoint round-trip FIXED
// POINTS: for every v that [Canonical] accepts,
//
//	Unmarshal(json.Marshal(Canonical(v))) deep-equals Canonical(v)
//
// and Canonical(Canonical(v)) deep-equals Canonical(v). encoding/json writes an
// int64 as its exact digits, which decode back to that int64, and every float64
// Canonical leaves as float64 is either non-whole or outside int64, so it decodes
// back to the same float64. The runtime canonicalizes every value before it is
// checkpointed (the interpreter's input and memo, the engine's step outputs), so
// the live value and the value a resume decodes are the same Go value.
//
// Canonical(v) is NOT in general Unmarshal(json.Marshal(v)) of the RAW value,
// because encoding/json spells a float64 with its shortest round-tripping digits,
// not its exact value: json.Marshal(float64(2^60)) is "1152921504606847000",
// which decodes to a different integer. Canonical takes int, int64, float64,
// bool, valid-UTF-8 strings, map[string]any and []any (with valid-UTF-8 keys)
// directly, preserving values exactly; every other Go type (uint*, float32,
// json.Number, structs, pointers, typed slices and maps such as
// []map[string]any or []float64) goes through that real round trip, so a whole
// float64 past 2^53 reached only through such a type (or a float32 anywhere)
// takes encoding/json's spelling. That is the one place raw and canonical
// values differ, it is deterministic, and it is still a fixed point afterwards.
package jsonnum

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"unicode/utf8"
)

// Unmarshal is encoding/json.Unmarshal with the canonical number model: every
// number stored in an `any` (at any depth, including inside typed structs —
// embedded ones too — maps, and slices) is an int64 or float64 per the package
// doc, never a float64 rounding of an integer that fits int64 and never a
// json.Number. Numbers decoded into typed numeric fields behave exactly as with
// encoding/json.
func Unmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	// json.Unmarshal rejects trailing data; a Decoder does not, so enforce it.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("jsonnum: invalid character after top-level value")
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil // Decode already rejected a non-pointer target
	}
	return walk(rv.Elem())
}

// FromNumber converts a valid JSON number literal to its canonical value (int64
// or float64) per the package doc.
func FromNumber(n json.Number) (any, error) {
	s := n.String()
	i, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return i, nil
	}
	// ErrRange from ParseInt means s is a syntactically valid plain integer that
	// does not fit int64; anything else (fraction, exponent) is a float literal.
	outOfRangeInt := errors.Is(err, strconv.ErrRange)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("jsonnum: number %q: %w", s, err)
	}
	if outOfRangeInt {
		// Never re-canonicalize an integer literal ParseInt rejected: its binary64
		// rounding can land on exactly -2^63, and turning that into
		// int64(MinInt64) would silently clamp the literal. It stays float64 (and
		// float64(-2^63) is canonical, see canonicalFloat), like the positive side.
		return f, nil
	}
	return canonicalFloat(f), nil
}

// canonicalFloat is the canonical form of a float64 value (package doc): a whole
// value strictly inside (-2^63, 2^63) is exactly int64(f), whose JSON spelling
// is its exact digits, so it decodes back to the same int64. Everything else
// stays float64: fractions (and NaN/±Inf, which Canonical rejects), and whole
// values at or past ±2^63. -2^63 itself stays float64 so that FromNumber's
// out-of-range integer literals ("-9223372036854775809" rounds to it) are
// canonical fixed points too; its spelling "-9223372036854776000" is below
// MinInt64, so it also decodes back to float64(-2^63). The value is unchanged
// either way, and the execir comparator equates it with int64(MinInt64).
func canonicalFloat(f float64) any {
	if f != math.Trunc(f) || f <= -(1<<63) || f >= (1<<63) {
		return f
	}
	return int64(f) // exact: f is whole and in range, so no rounding
}

// MaxDepth is the deepest container nesting (JSON objects and arrays on one
// path, the count encoding/json's decoder limits to 10000) that [Canonical]
// accepts. A canonical value is always checkpointed inside an envelope (the
// interpreter memo, an engine step's output map, nested subworkflow frames), so
// the limit leaves encodingJSONMaxDepth-MaxDepth levels of headroom: a value the
// live run accepts must also decode on resume, where it sits deeper than it did
// live. Anything deeper, and any cyclic map[string]any/[]any, is an error.
const MaxDepth = encodingJSONMaxDepth - checkpointEnvelopeReserve

const (
	// encodingJSONMaxDepth is encoding/json's decode nesting limit
	// (maxNestingDepth in encoding/json/scanner.go): a document whose objects
	// and arrays nest 10000 deep decodes, 10001 fails with "exceeded max depth".
	encodingJSONMaxDepth = 10000
	// checkpointEnvelopeReserve is the nesting kept free for checkpoint
	// envelopes around a canonical value: a few levels for the payload itself
	// plus about two per nested subworkflow frame (the default
	// maxWorkflowNesting is 8).
	checkpointEnvelopeReserve = 1000
)

// Canonical returns v in the canonical form (package doc): numbers become
// int64/float64 without changing their value, and every container becomes
// map[string]any / []any. The result is a checkpoint round-trip fixed point
// (json.Marshal, then [Unmarshal] into `any`, returns it unchanged), so a value
// that is canonicalized before it is checkpointed is the same Go value live and
// after resume (S7).
//
// nil, bool, valid-UTF-8 string, int, int64, finite float64, map[string]any and
// []any are copied directly, preserving numeric values exactly. Every other type
// (a []map[string]any, a typed struct, a uint64, a float32, a json.Number, ...)
// becomes exactly what the checkpoint round trip of it yields, including
// encoding/json's shortest spelling of any float inside it (package doc). The
// input is never mutated, so it is safe on values shared with callers.
//
// A value with no JSON encoding (NaN/±Inf, channels, funcs, complex numbers, a
// failing MarshalJSON, cyclic data) or nested deeper than [MaxDepth] is an
// error: it cannot be checkpointed and decoded back, so there is no replayed
// counterpart for it to equal, and letting it flow on live would make a fresh
// run and a resumed run diverge.
func Canonical(v any) (any, error) {
	return canonical(v, 0)
}

// canonical is Canonical for a value enclosed by depth containers.
func canonical(v any, depth int) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case string:
		if utf8.ValidString(x) {
			return x, nil
		}
		// json.Marshal replaces invalid UTF-8 with U+FFFD; mirror it exactly.
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return canonicalFloat(x), nil
		}
		// Non-finite: the round trip reports json.Marshal's error.
	case map[string]any:
		if x == nil {
			return nil, nil // encodes as null, which decodes to an untyped nil
		}
		if depth >= MaxDepth {
			return nil, errTooDeep(v)
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !utf8.ValidString(k) {
				return roundTrip(v, depth) // key rewriting could even merge keys
			}
			c, err := canonical(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		if x == nil {
			return nil, nil
		}
		if depth >= MaxDepth {
			return nil, errTooDeep(v)
		}
		out := make([]any, len(x))
		for i, e := range x {
			c, err := canonical(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return roundTrip(v, depth)
}

func errTooDeep(v any) error {
	return fmt.Errorf("jsonnum: %T value nests deeper than %d levels or is cyclic, so it cannot be checkpointed and replayed", v, MaxDepth)
}

// roundTrip is the checkpoint round trip of a value enclosed by depth
// containers: json.Marshal (which reports cycles through typed values), then
// Unmarshal, rejecting a result that would nest past MaxDepth in place.
func roundTrip(v any, depth int) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jsonnum: %T value has no JSON encoding, so it cannot be checkpointed and replayed identically: %w", v, err)
	}
	var out any
	if err := Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("jsonnum: %T value does not survive a checkpoint round trip: %w", v, err)
	}
	// Nesting n needs at least 2n bytes of brackets, so short encodings cannot
	// exceed the remaining budget and skip the walk.
	if budget := MaxDepth - depth; len(b) > 2*budget && nesting(out) > budget {
		return nil, errTooDeep(v)
	}
	return out, nil
}

// nesting is the container depth of a decoded tree (a scalar is 0, [] is 1).
// Decoding already bounded it by encoding/json's limit, so recursion is bounded.
func nesting(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			n = max(n, nesting(e))
		}
	case []any:
		for _, e := range x {
			n = max(n, nesting(e))
		}
	default:
		return 0
	}
	return n + 1
}

// CanonicalMap is Canonical for a map[string]any, preserving a nil map as nil.
func CanonicalMap(m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	c, err := Canonical(m)
	if err != nil {
		return nil, err
	}
	return c.(map[string]any), nil
}

// walk normalizes json.Number values in place under an addressable value
// produced by Decode. It handles the shapes encoding/json can populate: pointers,
// interfaces, structs, maps, slices and arrays.
func walk(v reflect.Value) error {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		nv, err := canonicalDecoded(v.Elem().Interface())
		if err != nil {
			return err
		}
		if v.CanSet() {
			v.Set(reflect.ValueOf(nv))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			return walk(v.Elem())
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			sf, f := t.Field(i), v.Field(i)
			switch {
			case sf.IsExported():
			case sf.Anonymous:
				// encoding/json fills the promoted exported fields of an embedded
				// UNEXPORTED struct (or *struct), but reflect marks everything
				// reached through it read-only. Re-derive a writable view of the
				// same memory so those promoted fields are normalized too; walk
				// still only descends into exported fields below it.
				ft := sf.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() != reflect.Struct || !f.CanAddr() {
					continue
				}
				f = reflect.NewAt(sf.Type, f.Addr().UnsafePointer()).Elem()
			default:
				continue
			}
			if err := walk(f); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if !mayHoldNumber(v.Type().Elem()) {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := walk(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := walk(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.IsNil() || !mayHoldNumber(v.Type().Elem()) {
			return nil
		}
		iter := v.MapRange()
		for iter.Next() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(iter.Value())
			if err := walk(e); err != nil {
				return err
			}
			v.SetMapIndex(iter.Key(), e)
		}
	}
	return nil
}

// mayHoldNumber reports whether a container element type could contain a
// json.Number, so byte slices, []string and the like are skipped cheaply.
func mayHoldNumber(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Struct, reflect.Slice, reflect.Array, reflect.Map:
		return true
	}
	return false
}

// canonicalDecoded normalizes a tree decoded into `any` (only json.Number, maps,
// and slices need work; strings, bools and nil pass through). It mutates the
// freshly decoded containers in place.
func canonicalDecoded(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		return FromNumber(x)
	case map[string]any:
		for k, e := range x {
			c, err := canonicalDecoded(e)
			if err != nil {
				return nil, err
			}
			x[k] = c
		}
		return x, nil
	case []any:
		for i, e := range x {
			c, err := canonicalDecoded(e)
			if err != nil {
				return nil, err
			}
			x[i] = c
		}
		return x, nil
	default:
		return v, nil
	}
}
