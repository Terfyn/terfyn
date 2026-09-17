package execir

import (
	"math"
	"testing"
)

func TestLargeIntegersRemainDistinct(t *testing.T) {
	t.Parallel()
	const a int64 = 9007199254740992
	const b int64 = 9007199254740993
	if valuesEqual(a, b) {
		t.Fatalf("valuesEqual(%d, %d) = true", a, b)
	}
	less, err := compareOrdered("<", a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !less {
		t.Fatalf("compareOrdered(<, %d, %d) = false", a, b)
	}
}

func TestNumericCompare_ExactIntegersAndMixtures(t *testing.T) {
	t.Parallel()
	const two53 int64 = 1 << 53
	cases := []struct {
		name string
		a, b any
		eq   bool
		lt   bool
	}{
		{name: "ordinary ints", a: int64(1), b: int64(2), eq: false, lt: true},
		{name: "int equals float", a: int64(1), b: float64(1), eq: true, lt: false},
		{name: "int vs fractional float", a: int64(1), b: 1.5, eq: false, lt: true},
		{name: "negative ints", a: int64(-3), b: int64(-1), eq: false, lt: true},
		{name: "negative int equals float", a: int64(-8), b: float64(-8), eq: true},
		{name: "±2^53 equal across types", a: two53, b: float64(two53), eq: true},
		{name: "2^53 vs 2^53+1", a: two53, b: two53 + 1, eq: false, lt: true},
		{name: "-(2^53+1) vs -2^53", a: -(two53 + 1), b: -two53, eq: false, lt: true},
		{name: "2^53+1 vs float 2^53", a: two53 + 1, b: float64(two53), eq: false, lt: false},
		{name: "MaxInt64 distinct from MaxInt64-1", a: int64(math.MaxInt64 - 1), b: int64(math.MaxInt64), eq: false, lt: true},
		{name: "MaxInt64 vs +Inf", a: int64(math.MaxInt64), b: math.Inf(1), eq: false, lt: true},
		{name: "MinInt64 vs -Inf", a: math.Inf(-1), b: int64(math.MinInt64), eq: false, lt: true},
		{name: "MinInt64 equals -2^63 float", a: int64(math.MinInt64), b: float64(math.MinInt64), eq: true},
		{name: "ordinary floats", a: 1.25, b: 1.5, eq: false, lt: true},
		{name: "equal floats", a: 2.5, b: 2.5, eq: true},
		{name: "int vs int", a: 7, b: int64(7), eq: true},
		{name: "negative fractional", a: int64(-2), b: -1.5, eq: false, lt: true},
		{name: "int above fraction", a: int64(-1), b: -1.5, eq: false, lt: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := valuesEqual(tc.a, tc.b); got != tc.eq {
				t.Fatalf("valuesEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.eq)
			}
			if got := valuesEqual(tc.b, tc.a); got != tc.eq {
				t.Fatalf("valuesEqual(%v, %v) = %v, want %v", tc.b, tc.a, got, tc.eq)
			}
			lt, err := compareOrdered("<", tc.a, tc.b)
			if err != nil {
				t.Fatalf("< err: %v", err)
			}
			if lt != tc.lt {
				t.Fatalf("compareOrdered(<, %v, %v) = %v, want %v", tc.a, tc.b, lt, tc.lt)
			}
			le, err := compareOrdered("<=", tc.a, tc.b)
			if err != nil {
				t.Fatalf("<= err: %v", err)
			}
			if le != (tc.lt || tc.eq) {
				t.Fatalf("compareOrdered(<=, %v, %v) = %v, want %v", tc.a, tc.b, le, tc.lt || tc.eq)
			}
			gt, err := compareOrdered(">", tc.a, tc.b)
			if err != nil {
				t.Fatalf("> err: %v", err)
			}
			if gt != (!tc.lt && !tc.eq) {
				t.Fatalf("compareOrdered(>, %v, %v) = %v, want %v", tc.a, tc.b, gt, !tc.lt && !tc.eq)
			}
		})
	}
}

func TestNumericCompare_NaNUnordered(t *testing.T) {
	t.Parallel()
	if valuesEqual(math.NaN(), math.NaN()) {
		t.Fatal("NaN == NaN")
	}
	if valuesEqual(int64(1), math.NaN()) {
		t.Fatal("1 == NaN")
	}
	for _, op := range []string{"<", "<=", ">", ">="} {
		got, err := compareOrdered(op, int64(1), math.NaN())
		if err != nil {
			t.Fatalf("%s NaN err: %v", op, err)
		}
		if got {
			t.Fatalf("%s with NaN = true", op)
		}
	}
}

func TestNumericCompare_NonNumericError(t *testing.T) {
	t.Parallel()
	_, err := compareOrdered("<", "a", int64(1))
	if err == nil {
		t.Fatal("expected error for non-numeric operand")
	}
}
