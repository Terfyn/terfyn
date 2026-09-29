package jsonnum

import (
	"encoding/json"
	"math"
	"math/big"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUnmarshal_CanonicalNumbers(t *testing.T) {
	t.Parallel()
	var got map[string]any
	in := `{
		"small": 7, "neg": -3, "zero": 0,
		"two53": 9007199254740992, "two53p1": 9007199254740993, "negTwo53m1": -9007199254740993,
		"max": 9223372036854775807, "min": -9223372036854775808,
		"pastMax": 9223372036854775808, "big": 1e30,
		"frac": 1.5, "tiny": 1e-7,
		"wholeFloat": 7.0, "wholeExp": 1e6,
		"nested": {"xs": [1, 2.5, 9007199254740993]}
	}`
	if err := Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"small": int64(7), "neg": int64(-3), "zero": int64(0),
		"two53": int64(9007199254740992), "two53p1": int64(9007199254740993), "negTwo53m1": int64(-9007199254740993),
		"max": int64(math.MaxInt64), "min": int64(math.MinInt64),
		"pastMax": float64(9223372036854775808), "big": 1e30,
		"frac": 1.5, "tiny": 1e-7,
		"wholeFloat": int64(7), "wholeExp": int64(1000000),
		"nested": map[string]any{"xs": []any{int64(1), 2.5, int64(9007199254740993)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

// The canonical form is a function of the numeric value, so encode -> decode is stable for every
// value class: int64 stays the same int64 (including past 2^53) and float64 the same float64.
func TestRoundTrip_PreservesIdentity(t *testing.T) {
	t.Parallel()
	vals := []any{
		int64(0), int64(1), int64(-1), int64(9007199254740992), int64(9007199254740993),
		int64(math.MaxInt64), int64(math.MinInt64), 1.5, -2.25, 1e-7, 1e30, float64(1 << 63),
	}
	for _, v := range vals {
		b, err := json.Marshal(map[string]any{"v": v})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["v"] != v {
			t.Fatalf("%T(%v) -> %s -> %T(%v)", v, v, b, got["v"], got["v"])
		}
		// A second cycle must be a fixed point.
		b2, _ := json.Marshal(got)
		var again map[string]any
		if err := Unmarshal(b2, &again); err != nil || !reflect.DeepEqual(got, again) {
			t.Fatalf("not a fixed point: %s -> %v (%v)", b2, again, err)
		}
	}
	// A whole-valued float64 has no JSON spelling distinct from an integer, so it decodes to the
	// integer of the same value (and renders identically before and after: see Canonical).
	b, _ := json.Marshal(map[string]any{"v": float64(1e6)})
	var got map[string]any
	if err := Unmarshal(b, &got); err != nil || got["v"] != int64(1000000) {
		t.Fatalf("float64(1e6) -> %s -> %#v (%v)", b, got["v"], err)
	}
}

func TestUnmarshal_TypedTargetsAndNestedStructs(t *testing.T) {
	t.Parallel()
	type step struct {
		Output any
		Meta   map[string]any
	}
	type payload struct {
		Version int              `json:"version"`
		Cost    float64          `json:"cost"`
		Input   map[string]any   `json:"input"`
		Steps   map[string]step  `json:"steps"`
		List    []any            `json:"list"`
		Ptr     *step            `json:"ptr"`
		Memo    map[string]any   `json:"memo"`
		Ctl     map[string]int   `json:"ctl"`
		Raw     json.RawMessage  `json:"raw"`
		Nested  []map[string]any `json:"nested"`
	}
	in := `{"version":1,"cost":2,"input":{"a":9007199254740993},
	"steps":{"s":{"Output":{"n":9007199254740993},"Meta":{"ms":12,"usd":0.25}}},
	"list":[9007199254740993],"ptr":{"Output":9007199254740993},
	"memo":{"k":{"id":9007199254740993}},"ctl":{"x":3},"raw":9007199254740993,
	"nested":[{"n":9007199254740993}]}`
	var p payload
	if err := Unmarshal([]byte(in), &p); err != nil {
		t.Fatal(err)
	}
	const big = int64(9007199254740993)
	checks := map[string]bool{
		"version": p.Version == 1,
		"cost":    p.Cost == 2,
		"input":   p.Input["a"] == big,
		"step":    p.Steps["s"].Output.(map[string]any)["n"] == big,
		"meta":    p.Steps["s"].Meta["ms"] == int64(12) && p.Steps["s"].Meta["usd"] == 0.25,
		"list":    p.List[0] == big,
		"ptr":     p.Ptr.Output == big,
		"memo":    p.Memo["k"].(map[string]any)["id"] == big,
		"ctl":     p.Ctl["x"] == 3,
		"raw":     string(p.Raw) == "9007199254740993",
		"nested":  p.Nested[0]["n"] == big,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s not canonical: %+v", name, p)
		}
	}
}

func TestUnmarshal_ErrorsMatchEncodingJSON(t *testing.T) {
	t.Parallel()
	for _, in := range []string{``, `{"a":`, `{"a":1} x`, `{"a":1}{"b":2}`, `{"a":1e999}`, `{"a":01}`} {
		var got map[string]any
		if err := Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("Unmarshal(%q) = %v, want error", in, got)
		}
	}
	var got map[string]any
	if err := Unmarshal([]byte(`null`), &got); err != nil || got != nil {
		t.Fatalf("null: %v %v", got, err)
	}
	if err := Unmarshal([]byte(` {"a":1} `), &got); err != nil {
		t.Fatalf("surrounding whitespace: %v", err)
	}
	if err := Unmarshal([]byte(`{}`), got); err == nil {
		t.Fatal("non-pointer target must error")
	}
}

func TestCanonical(t *testing.T) {
	t.Parallel()
	src := map[string]any{
		"i": 5, "i64": int64(6), "f": 2.0, "fr": 2.5, "n": json.Number("9007199254740993"),
		"s": "x", "b": true, "nil": nil,
		"l": []any{float64(3), json.Number("1.5")},
		"m": map[string]any{"z": float32(4)},
	}
	got, err := CanonicalMap(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"i": int64(5), "i64": int64(6), "f": int64(2), "fr": 2.5, "n": int64(9007199254740993),
		"s": "x", "b": true, "nil": nil,
		"l": []any{int64(3), 1.5},
		"m": map[string]any{"z": int64(4)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	if _, isNum := src["n"].(json.Number); !isNum || src["i"] != 5 {
		t.Fatal("Canonical must not mutate its input")
	}
	if m, err := CanonicalMap(nil); err != nil || m != nil {
		t.Fatal("nil map must stay nil")
	}
}

// checkpointRoundTrip is the real checkpoint path: the value sits inside a JSON object
// (as step outputs and memo entries do), is encoded with encoding/json, and is
// decoded back with Unmarshal into `any`.
func checkpointRoundTrip(t *testing.T, v any) (any, error) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": v})
	if err != nil {
		return nil, err
	}
	var got map[string]any
	if err := Unmarshal(b, &got); err != nil {
		return nil, err
	}
	return got["v"], nil
}

// exactFastFloats re-specifies, independently of Canonical, the ONE way Canonical(v)
// differs from the checkpoint round trip of the raw v: a whole float64 in (-2^63, 2^63)
// that Canonical reaches directly (bare, or through map[string]any/[]any whose keys are
// valid UTF-8) is taken at its exact value, int64(f), where encoding/json would write its
// shortest digits. It returns v with those leaves replaced by int64(f), and whether any
// replaced leaf's shortest spelling differs from its exact digits (only then do
// Canonical(v) and the raw round trip disagree).
func exactFastFloats(v any) (any, bool) {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && x > -(1<<63) && x < (1<<63) {
			spelled, err := strconv.ParseInt(strconv.FormatFloat(x, 'f', -1, 64), 10, 64)
			return int64(x), err != nil || spelled != int64(x)
		}
	case map[string]any:
		if x == nil {
			return v, false
		}
		for k := range x {
			if !utf8.ValidString(k) {
				return v, false // the whole map takes the round trip
			}
		}
		out, differs := make(map[string]any, len(x)), false
		for k, e := range x {
			var d bool
			out[k], d = exactFastFloats(e)
			differs = differs || d
		}
		return out, differs
	case []any:
		if x == nil {
			return v, false
		}
		out, differs := make([]any, len(x)), false
		for i, e := range x {
			var d bool
			out[i], d = exactFastFloats(e)
			differs = differs || d
		}
		return out, differs
	}
	return v, false
}

// assertCanonical checks the S7 property and pins exactly where Canonical departs from
// the raw round trip:
//
//   - FIXED POINT (what S7 needs, for every v): Canonical(v) survives the checkpoint round
//     trip unchanged and Canonical is idempotent, so a value canonicalized before it is
//     checkpointed is the same Go value live and on resume;
//   - Canonical(v) is the round trip of v with its directly reached whole float64 leaves
//     taken at their exact value (exactFastFloats), so it equals the raw round trip
//     wherever encoding/json's spelling is exact, and differs only where it is not.
func assertCanonical(t *testing.T, name string, v any) {
	t.Helper()
	live, err := Canonical(v)
	if err != nil {
		t.Fatalf("%s: Canonical(%T %#v): %v", name, v, v, err)
	}
	replay2, err := checkpointRoundTrip(t, live)
	if err != nil || !reflect.DeepEqual(replay2, live) {
		t.Fatalf("%s: canonical value not a round-trip fixed point: %#v -> %#v (%v)", name, live, replay2, err)
	}
	again, err := Canonical(live)
	if err != nil || !reflect.DeepEqual(again, live) {
		t.Fatalf("%s: Canonical not idempotent: %#v -> %#v (%v)", name, live, again, err)
	}
	exact, differs := exactFastFloats(v)
	want, err := checkpointRoundTrip(t, exact)
	if err != nil {
		t.Fatalf("%s: roundTrip(%T %#v): %v", name, exact, exact, err)
	}
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("%s: %T %#v\n live   %T %#v\n want   %T %#v", name, v, v, live, live, want, want)
	}
	replay, err := checkpointRoundTrip(t, v)
	if err != nil {
		t.Fatalf("%s: roundTrip(%T %#v): %v", name, v, v, err)
	}
	if got := !reflect.DeepEqual(live, replay); got != differs {
		t.Fatalf("%s: Canonical vs raw round trip differ=%v, want %v\n live   %#v\n replay %#v", name, got, differs, live, replay)
	}
}

type embeddedInner struct {
	X any `json:"x"`
}

type canonNested struct {
	ID    uint64            `json:"id"`
	Score float32           `json:"score"`
	Tags  []string          `json:"tags"`
	Meta  map[string]uint16 `json:"meta"`
	Child *canonNested      `json:"child,omitempty"`
	Any   any               `json:"any"`
	skip  int               //nolint:unused // unexported: never encoded
	embeddedInner
}

func TestCanonical_FixedPointAndRoundTrip(t *testing.T) {
	t.Parallel()
	i64 := int64(9007199254740993)
	f32 := float32(0.1)
	var nilPtr *int
	type named string
	type namedInt int32
	cases := map[string]any{
		// Every Go numeric kind, including boundaries and past-2^53 values.
		"int":          int(-7),
		"intBig":       int(math.MaxInt),
		"int8":         int8(math.MinInt8),
		"int16":        int16(math.MaxInt16),
		"int32":        int32(math.MinInt32),
		"int64":        int64(math.MaxInt64),
		"int64Min":     int64(math.MinInt64),
		"int64Past53":  i64,
		"uint":         uint(5),
		"uint8":        uint8(math.MaxUint8),
		"uint16":       uint16(math.MaxUint16),
		"uint32":       uint32(math.MaxUint32),
		"uint64":       uint64(5),
		"uint64Past53": uint64(9007199254740993),
		"uint64MaxI64": uint64(math.MaxInt64),
		"uint64PastI":  uint64(math.MaxInt64) + 1,
		"uint64Max":    uint64(math.MaxUint64),
		"uintptr":      uintptr(42),
		"float32":      f32,
		"float32Whole": float32(4),
		"float32Big":   float32(math.MaxFloat32),
		"float32Tiny":  float32(math.SmallestNonzeroFloat32),
		"float32Neg0":  float32(math.Copysign(0, -1)),
		"float64":      0.1,
		"float64Whole": float64(1e6),
		"float64Neg0":  math.Copysign(0, -1),
		"float64Two53": float64(1 << 53),
		"float64Two60": float64(1 << 60), // exact int64, NOT encoding/json's ...6847000
		"float64Neg60": -float64(1 << 60),
		"float64Big63": float64(1<<63) - 1024, // largest whole float64 below 2^63
		"float64Min1":  -float64(1<<63) + 2048,
		"float64Min":   float64(math.MinInt64),
		"float64Two63": float64(1 << 63),
		"float64Big":   1e300,
		"float64Max":   math.MaxFloat64,
		"float64Tiny":  math.SmallestNonzeroFloat64,
		"float64Exp":   1e21,
		"float64Small": 1e-7,
		"number":       json.Number("9007199254740993"),
		"numberFloat":  json.Number("7.0"),
		"numberEmpty":  json.Number(""), // encoding/json writes an empty Number as 0
		"namedInt":     namedInt(-3),
		// Scalars, strings (invalid UTF-8 is rewritten by json.Marshal), nil.
		"string":     "héllo <&>",
		"badUTF8":    "aÿbþ",
		"named":      named("n"),
		"bool":       true,
		"nil":        nil,
		"nilPtr":     nilPtr,
		"ptrInt":     &i64,
		"ptrFloat32": &f32,
		"bytes":      []byte("raw"),
		"rawMessage": json.RawMessage(`{"n":9007199254740993,"f":7.0}`),
		// Typed containers (workspace.grep returns []map[string]any).
		"sliceMap":   []map[string]any{{"n": 1.0, "u": uint(2)}},
		"sliceInt":   []int{1, 2, 3},
		"sliceU64":   []uint64{math.MaxUint64, 9007199254740993},
		"sliceF32":   []float32{0.1, 2},
		"sliceStr":   []string{"a"},
		"nilSlice":   []int(nil),
		"array":      [2]uint8{1, 2},
		"mapInt":     map[string]int{"a": 1},
		"mapIntKey":  map[int]float32{1: 0.1},
		"mapStrStr":  map[string]string{"a": "b"},
		"nilMap":     map[string]any(nil),
		"nilMapTyp":  map[string]int(nil),
		"emptySlice": []any{},
		"emptyMap":   map[string]any{},
		"badKey":     map[string]any{"aÿ": 1, "aþ": 2},
		// Structs: nested, pointers, embedded unexported, unexported fields.
		"struct": canonNested{
			ID: math.MaxUint64, Score: 0.1, Tags: []string{"t"}, Meta: map[string]uint16{"m": 9},
			Child: &canonNested{ID: 1, Any: []uint32{7}}, Any: json.Number("9007199254740993"),
			skip: 1, embeddedInner: embeddedInner{X: float32(0.1)},
		},
		"structPtr": &canonNested{ID: 9007199254740993},
		// Whole float64 past 2^53: exact through fast-path containers; encoding/json's
		// shortest spelling inside slow-path types (typed field, []float64,
		// []map[string]any, a map with an invalid UTF-8 key).
		"two60InMap":     map[string]any{"a": float64(1 << 60), "l": []any{float64(1 << 60)}},
		"two60InStruct":  struct{ F float64 }{F: 1 << 60},
		"two60InSlice":   []float64{1 << 60},
		"two60InRows":    []map[string]any{{"n": float64(1 << 60)}},
		"two60InBadKey":  map[string]any{"aÿ": float64(1 << 60)},
		"two60InPtr":     &struct{ Any any }{Any: map[string]any{"n": float64(1 << 60)}},
		"two60F32":       float32(1 << 60),
		"two60MixedRows": map[string]any{"fast": float64(1 << 60), "slow": []float64{1 << 60}},
		// Fast-path containers holding slow-path leaves.
		"mixed": map[string]any{
			"u": uint64(5), "f32": float32(0.1), "rows": []map[string]any{{"id": uint64(1)}},
			"deep": []any{map[string]any{"x": []int8{-1}}}, "n": json.Number("1e3"),
		},
	}
	for name, v := range cases {
		assertCanonical(t, name, v)
	}
}

// Random values of every numeric kind (random float64 bit patterns are mostly whole
// values past 2^53): every Canonical(v) is a round-trip fixed point, and differs from
// the raw round trip exactly where assertCanonical says, not just for hand-picked cases.
func TestCanonical_FixedPointAndRoundTrip_Random(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 5000; i++ {
		bits := r.Uint64()
		f64 := math.Float64frombits(bits)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			f64 = float64(int64(bits)) // keep the case finite but large
		}
		f32 := math.Float32frombits(uint32(bits))
		if math.IsNaN(float64(f32)) || math.IsInf(float64(f32), 0) {
			f32 = float32(int32(bits))
		}
		vals := []any{
			int(bits), int8(bits), int16(bits), int32(bits), int64(bits),
			uint(bits), uint8(bits), uint16(bits), uint32(bits), bits, uintptr(bits),
			f32, f64, float64(int64(bits)), float32(int32(bits)),
			[]any{bits, f32, f64}, map[string]any{"k": []uint64{bits}, "f": f64, "t": []float64{f64}},
		}
		for _, v := range vals {
			assertCanonical(t, "random", v)
		}
	}
}

// A value with no JSON encoding cannot be checkpointed, so it has no replayed
// counterpart: Canonical must refuse it rather than let it flow on live.
func TestCanonical_UnencodableIsError(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]any{
		"nan":        math.NaN(),
		"inf":        math.Inf(1),
		"nanNested":  map[string]any{"a": []any{math.Inf(-1)}},
		"float32NaN": float32(math.NaN()),
		"chan":       make(chan int),
		"func":       func() {},
		"complex":    complex(1, 2),
		"badNumber":  json.Number("0x10"),
		"mapInChan":  map[string]any{"c": make(chan int)},
	} {
		if got, err := Canonical(v); err == nil {
			t.Errorf("%s: Canonical = %#v, want error", name, got)
		}
		if _, err := checkpointRoundTrip(t, v); err == nil {
			t.Errorf("%s: round trip succeeded; Canonical must match it", name)
		}
	}
}

// Integer literals outside int64 are never re-canonicalized to an int64: both
// sides stay float64 (the negative band that binary64-rounds to exactly -2^63
// used to become int64(MinInt64) and compare equal to it), and both are stable
// across another checkpoint round trip.
func TestFromNumber_Int64Boundaries(t *testing.T) {
	t.Parallel()
	cases := map[string]any{
		"9223372036854775807":      int64(math.MaxInt64),
		"9223372036854775808":      float64(1 << 63),
		"9223372036854776832":      float64(1 << 63), // 2^63+1024 ties to even: 2^63
		"9223372036854776833":      float64(1<<63) + 2048,
		"18446744073709551615":     float64(1 << 64),
		"-9223372036854775808":     int64(math.MinInt64),
		"-9223372036854775809":     -float64(1 << 63), // not int64(MinInt64)
		"-9223372036854775810":     -float64(1 << 63),
		"-9223372036854776832":     -float64(1 << 63),
		"-9223372036854776833":     -float64(1<<63) - 2048,
		"-18446744073709551615":    -float64(1 << 64),
		"9223372036854775808.0":    float64(1 << 63),
		"-9223372036854775808.0":   -float64(1 << 63), // float literal: value -2^63 is canonically float64
		"-9.223372036854775808e18": -float64(1 << 63),
		"1152921504606846976.0":    int64(1152921504606846976), // float64(2^60), exactly
		"1152921504606847000.0":    int64(1152921504606846976), // the same binary64 value
		"1152921504606846977.0":    int64(1152921504606846976), // float literal rounds to binary64
		"9007199254740993.0":       int64(9007199254740992),    // float literal rounds; plain integers do not
		"9007199254740993":         int64(9007199254740993),
	}
	for lit, want := range cases {
		got, err := FromNumber(json.Number(lit))
		if err != nil || got != want {
			t.Errorf("FromNumber(%s) = %T(%v), %v; want %T(%v)", lit, got, got, err, want, want)
			continue
		}
		var m map[string]any
		if err := Unmarshal([]byte(`{"v":`+lit+`}`), &m); err != nil || m["v"] != want {
			t.Errorf("Unmarshal(%s) = %#v, %v", lit, m["v"], err)
		}
		// The decoded value is stable across another checkpoint round trip.
		assertCanonical(t, lit, got)
	}
	if _, err := FromNumber(json.Number("1" + strings.Repeat("0", 400))); err == nil {
		t.Error("an integer literal past float64 must be an error, as with encoding/json")
	}
}

// encoding/json fills the promoted exported fields of embedded unexported
// structs (by value and by non-nil pointer); Unmarshal must normalize them too.
func TestUnmarshal_EmbeddedUnexportedStructs(t *testing.T) {
	t.Parallel()
	type inner struct {
		X any `json:"x"`
	}
	type deeper struct {
		Z any `json:"z"`
	}
	type mid struct {
		deeper
		W any `json:"w"`
	}
	type outer struct {
		inner
		*mid
		Y any `json:"y"`
	}
	in := `{"x":9007199254740993,"y":9007199254740993,"w":9007199254740993,"z":{"n":9007199254740993}}`
	o := outer{mid: &mid{}} // encoding/json cannot allocate an unexported embedded pointer
	if err := Unmarshal([]byte(in), &o); err != nil {
		t.Fatal(err)
	}
	const big = int64(9007199254740993)
	if o.X != big || o.Y != big || o.W != big {
		t.Fatalf("promoted fields not canonical: X=%#v Y=%#v W=%#v", o.X, o.Y, o.W)
	}
	if z, _ := o.Z.(map[string]any); z == nil || z["n"] != big {
		t.Fatalf("doubly embedded field not canonical: %#v", o.Z)
	}
	// A nil embedded pointer is left alone (encoding/json would have errored if it
	// needed to allocate it; here no promoted field of it is present).
	var o2 outer
	if err := Unmarshal([]byte(`{"x":1,"y":2}`), &o2); err != nil || o2.X != int64(1) || o2.mid != nil {
		t.Fatalf("nil embedded pointer: %+v %v", o2, err)
	}
}

// A whole float64 keeps its exact VALUE: canonicalization must never hand the exact
// comparator a different integer than the number the input held. Swept around every
// boundary where binary64 spacing or the int64 range changes, plus random bit patterns.
func TestCanonical_WholeFloatKeepsExactValue(t *testing.T) {
	t.Parallel()
	check := func(f float64) {
		t.Helper()
		c, err := Canonical(f)
		if err != nil {
			t.Fatalf("Canonical(%v): %v", f, err)
		}
		switch x := c.(type) {
		case int64:
			if new(big.Float).SetInt64(x).Cmp(big.NewFloat(f)) != 0 {
				t.Fatalf("Canonical(%v) = int64(%d): value changed", f, x)
			}
		case float64:
			if x != f {
				t.Fatalf("Canonical(%v) = float64(%v): value changed", f, x)
			}
			if f == math.Trunc(f) && f > -(1<<63) && f < (1<<63) {
				t.Fatalf("Canonical(%v) = float64, want the exact int64", f)
			}
		default:
			t.Fatalf("Canonical(%v) = %T", f, c)
		}
		assertCanonical(t, "exact", f)
	}
	for _, center := range []float64{
		1 << 53, 1 << 60, 1 << 62, float64(1<<63) - 1024, 1 << 63, 1e21, 1e-6, 0,
		-(1 << 53), -(1 << 60), -(1 << 63),
	} {
		up, down := center, center
		for i := 0; i < 500; i++ {
			check(up)
			check(down)
			up, down = math.Nextafter(up, math.Inf(1)), math.Nextafter(down, math.Inf(-1))
		}
	}
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 5000; i++ {
		f := math.Float64frombits(r.Uint64())
		if !math.IsNaN(f) && !math.IsInf(f, 0) {
			check(f)
		}
	}
	// The review's repro: float64(2^60) is exactly 1152921504606846976, and must not
	// become 1152921504606847000 (encoding/json's shortest spelling of it).
	if c, _ := Canonical(float64(1 << 60)); c != int64(1152921504606846976) {
		t.Fatalf("Canonical(float64(2^60)) = %T(%v)", c, c)
	}
	if c, _ := Canonical(-float64(1 << 63)); c != -float64(1<<63) {
		t.Fatalf("Canonical(float64(-2^63)) = %T(%v), want float64 (like FromNumber)", c, c)
	}
}

// The one raw-vs-round-trip difference, pinned: a whole float64 past 2^53 reached only
// through a type Canonical does not take directly (here []float64) gets encoding/json's
// shortest spelling. That is still deterministic and a fixed point, so live and replayed
// values agree; it is not the exact value, which only the direct path preserves.
func TestCanonical_SlowPathFloatTakesEncodingJSONSpelling(t *testing.T) {
	t.Parallel()
	got, err := Canonical(map[string]any{"fast": float64(1 << 60), "slow": []float64{1 << 60}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"fast": int64(1152921504606846976), "slow": []any{int64(1152921504606847000)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func nestSlices(n int) any {
	var v any = []any{}
	for i := 1; i < n; i++ {
		v = []any{v}
	}
	return v
}

func nestMaps(n int, leaf any) any {
	v := leaf
	for i := 0; i < n; i++ {
		v = map[string]any{"k": v}
	}
	return v
}

// The limits Canonical mirrors: encoding/json decodes objects/arrays nested exactly
// encodingJSONMaxDepth deep and rejects one more level.
func TestEncodingJSONMaxDepth(t *testing.T) {
	t.Parallel()
	doc := func(n int) []byte { return []byte(strings.Repeat("[", n) + strings.Repeat("]", n)) }
	var v any
	if err := Unmarshal(doc(encodingJSONMaxDepth), &v); err != nil {
		t.Fatalf("depth %d: %v", encodingJSONMaxDepth, err)
	}
	if err := Unmarshal(doc(encodingJSONMaxDepth+1), &v); err == nil {
		t.Fatalf("depth %d decoded; encodingJSONMaxDepth is stale", encodingJSONMaxDepth+1)
	}
	if nesting(nestSlices(7)) != 7 || nesting(nestMaps(7, 1)) != 7 {
		t.Fatal("nesting miscounts")
	}
}

// Cyclic map[string]any/[]any used to recurse until the runtime aborted the process
// (unrecoverable stack overflow). It must be an error, like any cycle json.Marshal finds.
func TestCanonical_CyclesAreErrors(t *testing.T) {
	t.Parallel()
	selfMap := map[string]any{}
	selfMap["m"] = selfMap
	viaSlice := []any{nil}
	sliceHolder := map[string]any{"s": viaSlice}
	viaSlice[0] = sliceHolder
	viaTyped := map[string]any{}
	viaTyped["rows"] = []map[string]any{viaTyped}
	for name, v := range map[string]any{"map": selfMap, "sliceViaMap": sliceHolder, "slice": viaSlice, "typed": viaTyped} {
		if got, err := Canonical(v); err == nil {
			t.Errorf("%s: Canonical = %T, want error", name, got)
		}
	}
}

// Canonical accepts exactly MaxDepth levels of nesting, on the direct path, through
// slow-path subtrees, and mixed; a value it accepts round-trips even inside a checkpoint
// envelope (where it sits deeper), and one more level is an error.
func TestCanonical_DepthLimit(t *testing.T) {
	t.Parallel()
	ok := map[string]any{
		"slices":    nestSlices(MaxDepth),
		"maps":      nestMaps(MaxDepth, int64(1)),
		"slowLeaf":  nestMaps(MaxDepth-1, []int{1}),                     // typed slice is the last level
		"slowRows":  nestMaps(MaxDepth-2, []map[string]any{{"n": 1.0}}), // two levels via the round trip
		"slowOuter": []map[string]any{nestMaps(MaxDepth-1, int64(1)).(map[string]any)},
		"badKey":    nestMaps(MaxDepth-2, map[string]any{"aÿ": []any{}}), // round trip adds 2
	}
	for name, v := range ok {
		live, err := Canonical(v)
		if err != nil {
			t.Fatalf("%s at MaxDepth: %v", name, err)
		}
		if nesting(live) != MaxDepth {
			t.Fatalf("%s: nesting %d, want %d", name, nesting(live), MaxDepth)
		}
		// Wrap in a deep checkpoint-like envelope and round trip it.
		env := nestMaps(checkpointEnvelopeReserve-1, live)
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		var back any
		if err := Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, env) {
			t.Fatalf("%s: canonical value at MaxDepth does not survive a checkpoint envelope: %v", name, err)
		}
	}
	tooDeep := map[string]any{
		"slices":    nestSlices(MaxDepth + 1),
		"maps":      nestMaps(MaxDepth+1, int64(1)),
		"slowLeaf":  nestMaps(MaxDepth, []int{1}),
		"slowRows":  nestMaps(MaxDepth-1, []map[string]any{{"n": 1.0}}),
		"slowOuter": []map[string]any{nestMaps(MaxDepth, int64(1)).(map[string]any)},
		"badKey":    nestMaps(MaxDepth-1, map[string]any{"aÿ": []any{}}),
		"pastJSON":  []map[string]any{nestMaps(encodingJSONMaxDepth, int64(1)).(map[string]any)},
	}
	for name, v := range tooDeep {
		if _, err := Canonical(v); err == nil {
			t.Errorf("%s at MaxDepth+1: want error", name)
		}
	}
}
