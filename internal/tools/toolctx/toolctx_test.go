package toolctx

import (
	"context"
	"testing"
)

func TestOutputBudget(t *testing.T) {
	if _, ok := OutputBudget(context.Background()); ok {
		t.Fatal("budget reported on a bare context")
	}
	for _, n := range []int{128 << 10, 0, -1} {
		got, ok := OutputBudget(WithOutputBudget(context.Background(), n))
		if !ok || got != n {
			t.Fatalf("WithOutputBudget(%d): got (%d, %v)", n, got, ok)
		}
	}
	inner := WithOutputBudget(WithOutputBudget(context.Background(), 1), 2)
	if got, _ := OutputBudget(inner); got != 2 {
		t.Fatalf("innermost budget should win, got %d", got)
	}
}
