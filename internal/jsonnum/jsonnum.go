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
// The canonical form of a float64 is a function of its VALUE, not of the literal
// or of the Go type the value had before it was serialized, and it is whatever
// encoding/json's spelling of that value decodes to. That is what makes it stable
// across encode -> decode: encoding/json writes float64(1e6) as `1000000`, and an
// int64(1000000) the same way, so both must decode to the same Go value or a
// resumed run would render/compare them differently from the original. So:
//
//   - a whole float64 in the int64 range is the int64 its shortest decimal
//     spelling denotes: exactly int64(f) up to 2^53 ("7.0" and "7e0" are
//     int64(7)); beyond 2^53 encoding/json writes the shortest round-tripping
//     digits, so float64(2^60) is int64(1152921504606847000), not ...6976;
//   - a whole float64 whose spelling falls outside int64 (float64(-2^63) is
//     written "-9223372036854776000") stays float64, as does every fraction.
//
// Encoding is encoding/json's own (int64 is written as exact digits), so no
// custom marshaler is needed: Marshal(int64) -> Unmarshal returns the same int64.
//
// [Canonical] is defined as that checkpoint round trip (json.Marshal, then
// [Unmarshal] into `any`), so a live value and its decoded-from-checkpoint
// counterpart are the same Go value by construction, whatever Go type a producer
// returned.
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

// canonicalFloat is the canonical form of a float64 value: what encoding/json's
// spelling of f decodes to under FromNumber. A whole value in the int64 range is
// written as its shortest round-tripping digits in plain notation (never an
// exponent below 1e21), so it becomes the int64 those digits spell when that
// fits, and stays float64 otherwise. Everything else stays float64.
func canonicalFloat(f float64) any {
	if f != math.Trunc(f) || f < -(1<<63) || f >= (1<<63) {
		return f // fractions, NaN/±Inf, and whole values past int64
	}
	if f >= -(1<<53) && f <= (1<<53) {
		return int64(f) // every integer here is a float64, so the spelling is exact
	}
	if i, err := strconv.ParseInt(strconv.FormatFloat(f, 'f', -1, 64), 10, 64); err == nil {
		return i
	}
	return f // only -2^63, whose spelling "-9223372036854776000" is below MinInt64
}

// Canonical returns v in the canonical form: exactly the value a checkpoint
// round trip (json.Marshal, then [Unmarshal] into `any`) produces, so a live
// value and its decoded-from-checkpoint counterpart are the same Go value (S7).
// Numbers become int64/float64 per the package doc, and every container becomes
// map[string]any / []any: a []map[string]any, a typed struct, a uint64, a float32
// or a json.Number all come out exactly as they would on replay.
//
// The shapes the runtime overwhelmingly carries (nil, bool, valid-UTF-8 string,
// int, int64, finite float64, map[string]any, []any) take a copying fast path
// that computes the same result without encoding; every other type goes through
// the real round trip. The input is never mutated, so it is safe on values
// shared with callers.
//
// A value with no JSON encoding (NaN/±Inf, channels, funcs, complex numbers, a
// failing MarshalJSON, cyclic data) is an error: it cannot be checkpointed, so
// there is no replayed counterpart for it to equal, and letting it flow on live
// would make a fresh run and a resumed run diverge.
func Canonical(v any) (any, error) {
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
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !utf8.ValidString(k) {
				return roundTrip(v) // key rewriting could even merge keys
			}
			c, err := Canonical(e)
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
		out := make([]any, len(x))
		for i, e := range x {
			c, err := Canonical(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return roundTrip(v)
}

// roundTrip is the checkpoint round trip itself: the definition of Canonical.
func roundTrip(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jsonnum: %T value has no JSON encoding, so it cannot be checkpointed and replayed identically: %w", v, err)
	}
	var out any
	if err := Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("jsonnum: %T value does not survive a checkpoint round trip: %w", v, err)
	}
	return out, nil
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
