package jsonnum

import (
	"encoding/json"
	"math"
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
	got := CanonicalMap(src)
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
	if CanonicalMap(nil) != nil {
		t.Fatal("nil map must stay nil")
	}
	if !strings.Contains(reflect.TypeOf(Canonical(uint8(1))).String(), "int64") {
		t.Fatal("uint8 -> int64")
	}
}
