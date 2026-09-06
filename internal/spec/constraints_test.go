package spec

import "testing"

func TestResolveMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   *AgentConstraints
		want int
	}{
		{"nil constraints -> default", nil, DefaultAgentMaxTokens},
		{"unset -> default", &AgentConstraints{}, DefaultAgentMaxTokens},
		{"zero -> default", &AgentConstraints{MaxTokens: 0}, DefaultAgentMaxTokens},
		{"positive used verbatim", &AgentConstraints{MaxTokens: 32000}, 32000},
		// No hard clamp: a large author-set value is passed through (the provider enforces its own).
		{"large value not clamped", &AgentConstraints{MaxTokens: 120000}, 120000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMaxTokens(tc.in); got != tc.want {
				t.Fatalf("ResolveMaxTokens(%+v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
	// The default replaces the old chat-era 4096; it must be a realistic agent budget.
	if DefaultAgentMaxTokens <= 4096 {
		t.Fatalf("DefaultAgentMaxTokens = %d, want a raised agent default (> 4096)", DefaultAgentMaxTokens)
	}
}

func TestResolveMaxIterations_policyCeiling(t *testing.T) {
	cases := []struct {
		name       string
		c          *AgentConstraints
		policyCeil int
		want       int
	}{
		{"nil + no ceiling -> default", nil, 0, DefaultAgentMaxIterations},
		{"unset + no ceiling -> default", &AgentConstraints{}, 0, DefaultAgentMaxIterations},
		{"explicit under default hard cap", &AgentConstraints{MaxIterations: 20}, 0, 20},
		{"explicit above default hard cap -> clamped to 32", &AgentConstraints{MaxIterations: 99}, 0, HardAgentMaxIterations},
		{"policy raises the ceiling -> 64 honored", &AgentConstraints{MaxIterations: 64}, 128, 64},
		{"policy ceiling clamps above it", &AgentConstraints{MaxIterations: 200}, 128, 128},
		{"policy ceiling below default bounds even the default", &AgentConstraints{}, 4, 4},
		{"policy ceiling below an explicit value clamps it", &AgentConstraints{MaxIterations: 10}, 6, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMaxIterations(tc.c, tc.policyCeil); got != tc.want {
				t.Fatalf("ResolveMaxIterations(%+v, %d) = %d, want %d", tc.c, tc.policyCeil, got, tc.want)
			}
		})
	}
}

func TestPolicyMaxIterations(t *testing.T) {
	if got := PolicyMaxIterations(nil); got != 0 {
		t.Fatalf("nil exec = %d, want 0", got)
	}
	if got := PolicyMaxIterations(&PolicyExecution{}); got != 0 {
		t.Fatalf("unset = %d, want 0", got)
	}
	if got := PolicyMaxIterations(&PolicyExecution{MaxIterations: 64}); got != 64 {
		t.Fatalf("set = %d, want 64", got)
	}
}

func TestEffectiveMaxIterationsCeiling(t *testing.T) {
	if got := EffectiveMaxIterationsCeiling(0); got != HardAgentMaxIterations {
		t.Fatalf("0 must map to the default %d, got %d", HardAgentMaxIterations, got)
	}
	if got := EffectiveMaxIterationsCeiling(64); got != 64 {
		t.Fatalf("positive value used verbatim, got %d", got)
	}
	if got := EffectiveMaxIterationsCeiling(4); got != 4 {
		t.Fatalf("a value below the default is honored (a tighter ceiling), got %d", got)
	}
}
