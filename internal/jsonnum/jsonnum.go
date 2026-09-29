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
//   - a number whose value is a whole number in [-2^63, 2^63) is an int64, however
//     it was written ("7", "7.0", "7e0" are all int64(7));
//   - every other number is a float64 (fractions, and integers outside int64);
//   - a literal that does not fit float64 is an error, as with encoding/json.
//
// The canonical form is a function of the numeric VALUE, not of the literal or of
// the Go type the value had before it was serialized. That is what makes it stable
// across encode -> decode: encoding/json writes float64(1e6) as `1000000`, and an
// int64(1000000) the same way, so both must decode to the same Go value or a
// resumed run would render/compare them differently from the original. Encoding
// is encoding/json's own (int64 is written as exact digits), so no custom
// marshaler is needed: Marshal(int64) -> Unmarshal returns the same int64.
//
// Integer literals written in decimal/exponent form with a fractional part
// ("9007199254740993.0") are float literals and are rounded to binary64 like any
// other float; only plain integer literals are guaranteed exact.
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
)

// Unmarshal is encoding/json.Unmarshal with the canonical number model: every
// number stored in an `any` (at any depth, including inside typed structs, maps,
// and slices) is an int64 or float64 per the package doc, never a float64
// rounding of a large integer and never a json.Number. Numbers decoded into typed
// numeric fields behave exactly as with encoding/json.
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

// FromNumber converts a JSON number literal to its canonical value (int64 or
// float64).
func FromNumber(n json.Number) (any, error) {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("jsonnum: number %q: %w", s, err)
	}
	return canonicalFloat(f), nil
}

// canonicalFloat maps a whole-valued float64 in the int64 range to int64.
func canonicalFloat(f float64) any {
	if f == math.Trunc(f) && f >= -(1<<63) && f < (1<<63) {
		return int64(f)
	}
	return f
}

// Canonical returns v with every number normalized to the canonical form,
// recursing through map[string]any and []any (copying them; the input is never
// mutated, so it is safe on values shared with callers). It accepts values that
// did not come from JSON: json.Number, float32/float64, and Go integer types are
// all mapped, so a live value and its decoded-from-checkpoint counterpart are the
// same Go value. Any other type is returned unchanged.
func Canonical(v any) any {
	switch x := v.(type) {
	case json.Number:
		if c, err := FromNumber(x); err == nil {
			return c
		}
		return x
	case float64:
		return canonicalFloat(x)
	case float32:
		return canonicalFloat(float64(x))
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Canonical(e)
		}
		return out
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Canonical(e)
		}
		return out
	default:
		return v
	}
}

// CanonicalMap is Canonical for a map[string]any, preserving nil.
func CanonicalMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return Canonical(m).(map[string]any)
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
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				if err := walk(v.Field(i)); err != nil {
					return err
				}
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
