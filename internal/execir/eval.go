package execir

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// evalArgs resolves each argument value against scope.
func evalArgs(scope map[string]any, args map[string]Value) (map[string]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		val, err := evalValue(scope, v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

// evalValue resolves a Ref against the scope or returns a Lit's Go value. The
// composite forms (Object/List/Template) evaluate their members recursively —
// they are pure over already-resolved refs, so a straight-line program that
// returns an object or interpolates a string is fully executable in isolation
// (control-flow nodes Graph/Approval are the parts deferred to later phases).
func evalValue(scope map[string]any, v Value) (any, error) {
	switch x := v.(type) {
	case Lit:
		return x.V, nil
	case Ref:
		return resolvePath(scope, x.Path)
	case Object:
		out := make(map[string]any, len(x.Fields))
		for _, f := range x.Fields {
			fv, err := evalValue(scope, f.Val)
			if err != nil {
				return nil, err
			}
			out[f.Key] = fv
		}
		return out, nil
	case List:
		out := make([]any, len(x.Elems))
		for i, e := range x.Elems {
			ev, err := evalValue(scope, e)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	case Template:
		var sb strings.Builder
		for _, p := range x.Parts {
			pv, err := evalValue(scope, p)
			if err != nil {
				return nil, err
			}
			sb.WriteString(stringify(pv))
		}
		return sb.String(), nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("execir: unknown value %T", v)
	}
}

// stringify renders a resolved value for embedding in an interpolated Template:
// scalars print directly, and composites JSON-encode (mirroring how the engine's
// string interpolation embeds objects/arrays), so a Template part that resolves
// to a map/list does not print as a Go %v map.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		if b, err := json.Marshal(x); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", x)
	}
}

// resolvePath resolves a dotted path against scope. The head must be a bound
// name (an unbound head is a programming error the checker/lowering should have
// caught, so it is surfaced loudly); a missing NESTED field resolves to nil
// (gradual — agent and tool outputs are dynamically shaped).
func resolvePath(scope map[string]any, path []string) (any, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("execir: empty reference path")
	}
	cur, ok := scope[path[0]]
	if !ok {
		return nil, fmt.Errorf("execir: unresolved reference %q", path[0])
	}
	for _, seg := range path[1:] {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, nil
		}
		cur = m[seg]
	}
	return cur, nil
}

// evalCollection resolves a value expected to be an iterable and returns its
// elements. A JSON array ([]any) iterates its elements; nil is an empty
// collection (an absent field yields zero iterations rather than an error). Any
// other concrete type is a runtime error — a loop needs a collection.
func evalCollection(scope map[string]any, v Value) ([]any, error) {
	val, err := evalValue(scope, v)
	if err != nil {
		return nil, err
	}
	switch xs := val.(type) {
	case nil:
		return nil, nil
	case []any:
		return xs, nil
	default:
		return nil, fmt.Errorf("execir: loop collection is %T, not a list", val)
	}
}

// evalExpr evaluates a boolean condition tree.
func evalExpr(scope map[string]any, e Expr) (bool, error) {
	switch x := e.(type) {
	case nil:
		return false, fmt.Errorf("execir: nil condition")
	case Leaf:
		val, err := evalValue(scope, x.V)
		if err != nil {
			return false, err
		}
		return truthy(val), nil
	case Not:
		b, err := evalExpr(scope, x.X)
		if err != nil {
			return false, err
		}
		return !b, nil
	case BinOp:
		return evalBinOp(scope, x)
	default:
		return false, fmt.Errorf("execir: unknown condition %T", e)
	}
}

func evalBinOp(scope map[string]any, x BinOp) (bool, error) {
	// Logical connectives short-circuit and operate on boolean sub-conditions.
	switch x.Op {
	case "&&":
		l, err := evalExpr(scope, x.X)
		if err != nil || !l {
			return false, err
		}
		return evalExpr(scope, x.Y)
	case "||":
		l, err := evalExpr(scope, x.X)
		if err != nil {
			return false, err
		}
		if l {
			return true, nil
		}
		return evalExpr(scope, x.Y)
	}
	// Comparisons operate on values.
	lv, err := leafValue(scope, x.X)
	if err != nil {
		return false, err
	}
	rv, err := leafValue(scope, x.Y)
	if err != nil {
		return false, err
	}
	switch x.Op {
	case "==":
		return valuesEqual(lv, rv), nil
	case "!=":
		return !valuesEqual(lv, rv), nil
	case "<", "<=", ">", ">=":
		return compareOrdered(x.Op, lv, rv)
	default:
		return false, fmt.Errorf("execir: unknown operator %q", x.Op)
	}
}

// leafValue evaluates an expression that must be a value leaf (the operand of a
// comparison). The parser only ever places Leaf nodes under a comparison, so a
// non-leaf here is an internal lowering error.
func leafValue(scope map[string]any, e Expr) (any, error) {
	leaf, ok := e.(Leaf)
	if !ok {
		return nil, fmt.Errorf("execir: comparison operand is not a value")
	}
	return evalValue(scope, leaf.V)
}

// truthy defines the boolean coercion for a bare condition leaf (`if flag`):
// booleans are themselves, null/absent is false, everything present is true.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	default:
		return true
	}
}

// valuesEqual is a total, panic-free equality over every value this IR can
// carry, so `==` is defined on the JSON objects and arrays a workflow input
// holds — a bare `a == b` on `any` would panic ("comparing uncomparable type")
// for a map or slice operand. Numbers compare numerically across int64/float64
// (1 == 1.0) without rounding integers through float64, so values above 2^53
// stay distinct. strings/bools compare by value, arrays and objects structurally
// (element- and key-wise, recursively, with the same numeric normalization),
// and anything else via reflect.DeepEqual, which never panics. A type mismatch
// is unequal.
func valuesEqual(a, b any) bool {
	if cmp, ok := tryCompareNumeric(a, b); ok {
		return cmp == 0
	}
	if isNumeric(a) || isNumeric(b) {
		return false
	}
	switch av := a.(type) {
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, present := bv[k]
			if !present || !valuesEqual(va, vb) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// compareOrdered evaluates <, <=, >, >= over numeric operands. A non-numeric
// operand is an error — ordering strings or booleans is not defined in the
// surface. Integer operands are compared exactly; mixed integer/float
// comparisons do not round the integer through float64.
func compareOrdered(op string, a, b any) (bool, error) {
	cmp, ok := tryCompareNumeric(a, b)
	if !ok {
		if isNumeric(a) && isNumeric(b) {
			// NaN is unordered: every relational operator is false.
			return false, nil
		}
		return false, fmt.Errorf("execir: operator %q needs numeric operands, got %T and %T", op, a, b)
	}
	switch op {
	case "<":
		return cmp < 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">":
		return cmp > 0, nil
	case ">=":
		return cmp >= 0, nil
	}
	return false, fmt.Errorf("execir: unknown operator %q", op)
}

func isNumeric(v any) bool {
	_, i := asInt64(v)
	_, f := asFloat64(v)
	return i || f
}

func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	default:
		return 0, false
	}
}

func asFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	default:
		return 0, false
	}
}

// maxExactInt is the largest integer magnitude float64 can represent exactly
// (the IEEE-754 binary64 significand is 53 bits, including the implicit bit).
const maxExactInt int64 = 1 << 53

// floatMinInt64 is MinInt64 as float64 (−2^63), which is exact.
// floatMaxInt64Exclusive is 2^63: MaxInt64 is not a float64, and every finite
// float64 ≥ 2^63 is strictly greater than any int64.
const (
	floatMinInt64          = -9223372036854775808.0
	floatMaxInt64Exclusive = 9223372036854775808.0
)

// tryCompareNumeric compares two numeric values. ok is false when either
// operand is non-numeric, or when a float operand is NaN (unordered).
func tryCompareNumeric(a, b any) (int, bool) {
	ai, aInt := asInt64(a)
	bi, bInt := asInt64(b)
	af, aFlt := asFloat64(a)
	bf, bFlt := asFloat64(b)
	switch {
	case aInt && bInt:
		return cmpInt64(ai, bi), true
	case aInt && bFlt:
		return compareIntFloat(ai, bf)
	case aFlt && bInt:
		c, ok := compareIntFloat(bi, af)
		return -c, ok
	case aFlt && bFlt:
		switch {
		case math.IsNaN(af) || math.IsNaN(bf):
			return 0, false
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	default:
		return 0, false
	}
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareIntFloat compares an integer to a float without rounding the integer
// through float64 when |i| > 2^53. ok is false when f is NaN.
func compareIntFloat(i int64, f float64) (int, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	if math.IsInf(f, 1) {
		return -1, true
	}
	if math.IsInf(f, -1) {
		return 1, true
	}
	if i >= -maxExactInt && i <= maxExactInt {
		fi := float64(i)
		switch {
		case fi < f:
			return -1, true
		case fi > f:
			return 1, true
		default:
			return 0, true
		}
	}
	if f >= floatMaxInt64Exclusive {
		return -1, true
	}
	if f < floatMinInt64 {
		return 1, true
	}
	trunc := math.Trunc(f)
	ti := int64(trunc)
	if f == trunc {
		return cmpInt64(i, ti), true
	}
	if f > 0 {
		if i <= ti {
			return -1, true
		}
		return 1, true
	}
	if i < ti {
		return -1, true
	}
	return 1, true
}
