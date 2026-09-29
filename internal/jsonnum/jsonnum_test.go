package jsonnum

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
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

// assertCanonicalIsRoundTrip is the S7 property: the live canonical value and the
// value a resume decodes from the checkpoint are the same Go value, and the
// canonical form is a fixed point of both Canonical and the round trip.
func assertCanonicalIsRoundTrip(t *testing.T, name string, v any) {
	t.Helper()
	live, err := Canonical(v)
	if err != nil {
		t.Fatalf("%s: Canonical(%T %#v): %v", name, v, v, err)
	}
	replay, err := checkpointRoundTrip(t, v)
	if err != nil {
		t.Fatalf("%s: roundTrip(%T %#v): %v", name, v, v, err)
	}
	if !reflect.DeepEqual(live, replay) {
		t.Fatalf("%s: %T %#v\n live   %T %#v\n replay %T %#v", name, v, v, live, live, replay, replay)
	}
	again, err := Canonical(live)
	if err != nil || !reflect.DeepEqual(again, live) {
		t.Fatalf("%s: Canonical not idempotent: %#v -> %#v (%v)", name, live, again, err)
	}
	replay2, err := checkpointRoundTrip(t, live)
	if err != nil || !reflect.DeepEqual(replay2, live) {
		t.Fatalf("%s: canonical value not a round-trip fixed point: %#v -> %#v (%v)", name, live, replay2, err)
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

func TestCanonical_EqualsCheckpointRoundTrip(t *testing.T) {
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
		// Fast-path containers holding slow-path leaves.
		"mixed": map[string]any{
			"u": uint64(5), "f32": float32(0.1), "rows": []map[string]any{{"id": uint64(1)}},
			"deep": []any{map[string]any{"x": []int8{-1}}}, "n": json.Number("1e3"),
		},
	}
	for name, v := range cases {
		assertCanonicalIsRoundTrip(t, name, v)
	}
}

// Random values of every numeric kind: Canonical(v) must deep-equal the checkpoint
// round trip for all of them, not just hand-picked ones.
func TestCanonical_EqualsCheckpointRoundTrip_Random(t *testing.T) {
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
			[]any{bits, f32}, map[string]any{"k": []uint64{bits}},
		}
		for _, v := range vals {
			assertCanonicalIsRoundTrip(t, "random", v)
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
		"1152921504606846976.0":    int64(1152921504606847000), // float64(2^60), as encoding/json spells it
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
		assertCanonicalIsRoundTrip(t, lit, got)
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
