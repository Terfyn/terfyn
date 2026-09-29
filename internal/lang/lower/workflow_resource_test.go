package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Terfyn/terfyn/internal/execir"
	"github.com/Terfyn/terfyn/internal/lang"
	"github.com/Terfyn/terfyn/internal/spec"
)

// lowerYAMLWorkflowOrFatal parses a single YAML Workflow document and lowers it
// to the execution IR.
func lowerYAMLWorkflowOrFatal(t *testing.T, src string) (*execir.Program, lang.Diagnostics) {
	t.Helper()
	dec, err := spec.ParseResourceFromBytes([]byte(src), "test.yaml")
	if err != nil {
		t.Fatalf("parse workflow YAML: %v", err)
	}
	wf, ok := dec.Resource.(*spec.WorkflowResource)
	if !ok {
		t.Fatalf("decoded resource is %T, not a WorkflowResource", dec.Resource)
	}
	return LowerWorkflowResource(wf)
}

// TestLowerWorkflowResource_DifferentialParity is the acceptance bar (issue
// #256): a straight-line YAML workflow and its `.agent` twin lower to identical
// programs — same structure and same digest. The digest is the structural check
// (it excludes source positions, which necessarily differ between the two
// surfaces).
func TestLowerWorkflowResource_DifferentialParity(t *testing.T) {
	t.Parallel()

	yamlProg, ydiags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: W
spec:
  steps:
    - id: fetch_pr
      uses: tool.github.pull_request.fetch
      with:
        pr: ${input.pr}
    - id: review
      agent: reviewer
      with:
        pull_request: ${steps.fetch_pr.output.pull_request}
  output:
    value:
      value: ${steps.review.output}
`)
	if ydiags.HasErrors() {
		t.Fatalf("YAML lowering diagnostics: %v", ydiags)
	}

	agentProg, adiags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    fetch_pr = github.pull_request.fetch(pr: input.pr)
    review = reviewer(pull_request: fetch_pr.pull_request)
    return review
}
`, nil)
	if adiags.HasErrors() {
		t.Fatalf(".agent lowering diagnostics: %v", adiags)
	}

	if yamlProg.Digest() != agentProg.Digest() {
		t.Fatalf("digests differ:\n YAML:  %s\n .agent: %s", yamlProg.Digest(), agentProg.Digest())
	}
	// The digest match is the contract, but pin the shape too so a future digest
	// weakening cannot hide a divergence.
	if len(yamlProg.Body) != 3 {
		t.Fatalf("expected 3 top nodes (2 steps + return), got %d", len(yamlProg.Body))
	}
	if tool, ok := yamlProg.Body[0].(*execir.InvokeTool); !ok || tool.Bind != "fetch_pr" || tool.Uses != "tool.github.pull_request.fetch" {
		t.Fatalf("node 0 should be InvokeTool fetch_pr, got %#v", yamlProg.Body[0])
	}
	if _, ok := yamlProg.Body[2].(*execir.Return); !ok {
		t.Fatalf("node 2 should be Return, got %T", yamlProg.Body[2])
	}
}

// TestLowerWorkflowResource_TemplateParity is the acceptance bar for issue #316:
// an `.agent` argument with an embedded `${binding.field}` token lowers to the same
// program as the YAML twin whose `${steps.<id>.output.field}` interpolation names
// the resolved step directly. The head binding is resolved through the workflow's
// binding environment, so the twins share a digest and the templated step gains the
// referenced step as a predecessor.
//
// The agent argument is the NAMED `arg0:` field on both sides: a positional
// `reviewer(input)` is the whole-document call shape (#550), which a YAML with:
// map cannot express, so it deliberately has a different digest than its
// `arg0:` YAML lookalike.
func TestLowerWorkflowResource_TemplateParity(t *testing.T) {
	t.Parallel()

	yamlProg, ydiags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: W
spec:
  steps:
    - id: review
      agent: reviewer
      with:
        arg0: ${input}
    - id: post
      uses: tool.github.post
      with:
        body: "## Review\n${steps.review.output.summary}\ndone"
  output:
    value:
      value: ${steps.post.output}
`)
	if ydiags.HasErrors() {
		t.Fatalf("YAML lowering diagnostics: %v", ydiags)
	}

	agentProg, adiags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    review = reviewer(arg0: input)
    post = github.post(body: "## Review\n${review.summary}\ndone")
    return post
}
`, nil)
	if adiags.HasErrors() {
		t.Fatalf(".agent lowering diagnostics: %v", adiags)
	}

	if yamlProg.Digest() != agentProg.Digest() {
		t.Fatalf("template twins differ:\n YAML:   %s\n .agent: %s", yamlProg.Digest(), agentProg.Digest())
	}
}

// TestLowerWorkflowResource_LiteralParity guards the twin digest across every
// scalar literal type. yaml.v3 decodes an authored integer as Go `int` while the
// `.agent` parser produces `int64`, and execir.litKey tokenizes those
// differently — so without scalar canonicalization at the lowering boundary the
// two twins would hash differently for a literal that executes identically. This
// is the parity that DifferentialParity (refs only) does not exercise.
func TestLowerWorkflowResource_LiteralParity(t *testing.T) {
	t.Parallel()

	yamlProg, ydiags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: W
spec:
  steps:
    - id: a
      uses: tool.t.op
      with:
        n: 5
        ratio: 1.5
        flag: true
        name: hi
  output:
    value:
      value: ${steps.a.output}
`)
	if ydiags.HasErrors() {
		t.Fatalf("YAML lowering diagnostics: %v", ydiags)
	}
	agentProg, adiags := lowerExecOrFatal(t, `
workflow W(input: PR) {
    a = t.op(n: 5, ratio: 1.5, flag: true, name: "hi")
    return a
}
`, nil)
	if adiags.HasErrors() {
		t.Fatalf(".agent lowering diagnostics: %v", adiags)
	}
	if yamlProg.Digest() != agentProg.Digest() {
		t.Fatalf("literal twin digests differ:\n YAML:   %s\n .agent: %s", yamlProg.Digest(), agentProg.Digest())
	}
	// The integer literal must be the canonical int64, not the yaml.v3 int.
	if n, ok := yamlProg.Body[0].(*execir.InvokeTool).Args["n"].(execir.Lit); !ok {
		t.Fatalf("arg n should be a Lit, got %#v", yamlProg.Body[0].(*execir.InvokeTool).Args["n"])
	} else if _, ok := n.V.(int64); !ok {
		t.Fatalf("integer literal should be canonicalized to int64, got %T", n.V)
	}
}

// TestLowerWorkflowResource_InterpolationRefMapping pins the ${...} → Ref mapping
// in the source binding namespace: input.<field> keeps its path, steps.<id>.
// output.<f> drops output (a step binds its output), and steps.<id>.meta.<f>
// keeps meta as an explicit segment.
func TestLowerWorkflowResource_InterpolationRefMapping(t *testing.T) {
	t.Parallel()
	prog, diags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: refs
spec:
  steps:
    - id: prev
      agent: P
    - id: s1
      uses: tool.t.op
      with:
        a: ${input.repo.name}
        b: ${steps.prev.output.x.y}
        c: ${steps.prev.meta.trace}
`)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	s1, ok := prog.Body[1].(*execir.InvokeTool)
	if !ok {
		t.Fatalf("node 1 should be InvokeTool, got %T", prog.Body[1])
	}
	wantPaths := map[string][]string{
		"a": {"input", "repo", "name"},
		"b": {"prev", "x", "y"},
		"c": {"prev", "meta", "trace"},
	}
	for arg, want := range wantPaths {
		ref, ok := s1.Args[arg].(execir.Ref)
		if !ok {
			t.Fatalf("arg %q should be a Ref, got %#v", arg, s1.Args[arg])
		}
		if !equalStrings(ref.Path, want) {
			t.Fatalf("arg %q path = %v, want %v", arg, ref.Path, want)
		}
	}
}

// TestLowerWorkflowResource_EmbeddedTokenTemplate covers a string that mixes
// prose with tokens: it lowers to a Template whose token parts are Refs, so every
// ${...} still becomes a Ref (not a lost raw string).
func TestLowerWorkflowResource_EmbeddedTokenTemplate(t *testing.T) {
	t.Parallel()
	prog, diags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: tmpl
spec:
  steps:
    - id: post
      uses: tool.gh.comment
      with:
        body: "review: ${steps.post.output.summary} end"
`)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	post := prog.Body[0].(*execir.InvokeTool)
	tmpl, ok := post.Args["body"].(execir.Template)
	if !ok {
		t.Fatalf("body should be a Template, got %#v", post.Args["body"])
	}
	if len(tmpl.Parts) != 3 {
		t.Fatalf("expected 3 template parts (lit, ref, lit), got %d: %#v", len(tmpl.Parts), tmpl.Parts)
	}
	ref, ok := tmpl.Parts[1].(execir.Ref)
	if !ok || !equalStrings(ref.Path, []string{"post", "summary"}) {
		t.Fatalf("middle part should be Ref[post,summary], got %#v", tmpl.Parts[1])
	}
}

// TestLowerWorkflowResource_MultiKeyOutput checks a multi-field output.value
// lowers to a Return of an Object (not the single-key unwrap).
func TestLowerWorkflowResource_MultiKeyOutput(t *testing.T) {
	t.Parallel()
	prog, diags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: out
spec:
  steps:
    - id: a
      agent: A
  output:
    value:
      review: ${steps.a.output}
      ok: true
`)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	ret, ok := prog.Body[len(prog.Body)-1].(*execir.Return)
	if !ok {
		t.Fatalf("last node should be Return, got %T", prog.Body[len(prog.Body)-1])
	}
	obj, ok := ret.Value.(execir.Object)
	if !ok {
		t.Fatalf("return value should be an Object, got %#v", ret.Value)
	}
	if len(obj.Fields) != 2 {
		t.Fatalf("expected 2 output fields, got %d", len(obj.Fields))
	}
}

// TestLowerWorkflowResource_ApprovalNode covers the fourth XOR step kind (#195):
// an approval step lowers to an Approval node carrying its presentation.
func TestLowerWorkflowResource_ApprovalNode(t *testing.T) {
	t.Parallel()
	prog, diags := lowerYAMLWorkflowOrFatal(t, `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: appr
spec:
  steps:
    - id: draft
      agent: drafter
      with:
        topic: ${input.topic}
    - id: gate
      approval:
        description: Please review
        redactKeys: [secret]
  output:
    value:
      value: ${steps.draft.output}
`)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	appr, ok := prog.Body[1].(*execir.Approval)
	if !ok {
		t.Fatalf("node 1 should be Approval, got %T", prog.Body[1])
	}
	if appr.Bind != "gate" {
		t.Fatalf("approval bind = %q, want gate", appr.Bind)
	}
	if appr.Description != "Please review" {
		t.Fatalf("approval description = %q", appr.Description)
	}
	if !equalStrings(appr.RedactKeys, []string{"secret"}) {
		t.Fatalf("approval redactKeys = %v", appr.RedactKeys)
	}
}

// TestLowerWorkflowResource_GeneralDAG exercises design decision 1: a workflow
// that opts into graph mode lowers to one Graph node preserving each step's
// authored dependency set — the case Fork cannot express (A,B roots; C[A];
// D[A,B]; E[C]).
func TestLowerWorkflowResource_GeneralDAG(t *testing.T) {
	t.Parallel()
	prog, diags := lowerYAMLWorkflowOrFatal(t, dagWorkflowYAML)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(prog.Body) != 1 {
		t.Fatalf("expected a single Graph node, got %d nodes", len(prog.Body))
	}
	g, ok := prog.Body[0].(*execir.Graph)
	if !ok {
		t.Fatalf("body[0] should be a Graph, got %T", prog.Body[0])
	}
	needs := map[string][]string{}
	for _, gn := range g.Nodes {
		needs[gn.ID] = gn.Needs
	}
	if len(g.Nodes) != 5 {
		t.Fatalf("expected 5 graph nodes, got %d", len(g.Nodes))
	}
	if !equalStrings(needs["d"], []string{"a", "b"}) {
		t.Fatalf("d needs = %v, want [a b]", needs["d"])
	}
	if !equalStrings(needs["e"], []string{"c"}) {
		t.Fatalf("e needs = %v, want [c]", needs["e"])
	}
	if len(needs["a"]) != 0 || len(needs["b"]) != 0 {
		t.Fatalf("a/b should be roots, got a=%v b=%v", needs["a"], needs["b"])
	}
}

// TestLowerWorkflowResource_DigestStableAcrossStructuring pins that reordering
// independent DAG steps (a semantically-equivalent structuring) does not change
// the digest.
func TestLowerWorkflowResource_DigestStableAcrossStructuring(t *testing.T) {
	t.Parallel()
	p1, d1 := lowerYAMLWorkflowOrFatal(t, dagWorkflowYAML)
	p2, d2 := lowerYAMLWorkflowOrFatal(t, dagWorkflowReorderedYAML)
	if d1.HasErrors() || d2.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v / %v", d1, d2)
	}
	if p1.Digest() != p2.Digest() {
		t.Fatalf("reordered DAG changed digest:\n %s\n %s", p1.Digest(), p2.Digest())
	}
}

// TestLowerWorkflowResource_ExamplesLowerWithoutError is the corpus bar (issue
// #256 AC1): every Workflow resource under examples/ lowers without an error
// diagnostic.
func TestLowerWorkflowResource_ExamplesLowerWithoutError(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "examples"))
	if err != nil {
		t.Fatalf("resolve examples dir: %v", err)
	}
	var files []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		dec, derr := spec.ParseResourceFromBytes(data, path)
		if derr != nil {
			return nil // not a single-doc resource / not our concern here
		}
		if wf, ok := dec.Resource.(*spec.WorkflowResource); ok {
			files = append(files, path)
			if _, diags := LowerWorkflowResource(wf); diags.HasErrors() {
				t.Errorf("%s: lowering produced errors: %v", path, diags)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
	if len(files) == 0 {
		// The examples are now authored entirely in .agent (issue #430/#440), so there are no YAML
		// Workflow resources left to sweep. This is a bonus check over real examples; LowerWorkflowResource
		// stays covered by the inline-fixture tests in this file. Skip rather than fail on the end state.
		t.Skip("no YAML Workflow resources under examples/ (examples are .agent-only, #440)")
	}
}

const dagWorkflowYAML = `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: dag
spec:
  steps:
    - id: a
      agent: A
      needs: []
    - id: b
      agent: B
      needs: []
    - id: c
      agent: C
      needs: [a]
    - id: d
      agent: D
      needs: [a, b]
    - id: e
      agent: E
      needs: [c]
`

// dagWorkflowReorderedYAML is dagWorkflowYAML with independent steps reordered;
// it denotes the same DAG.
const dagWorkflowReorderedYAML = `
apiVersion: agentic.dev/v0
kind: Workflow
metadata:
  name: dag
spec:
  steps:
    - id: b
      agent: B
      needs: []
    - id: d
      agent: D
      needs: [b, a]
    - id: a
      agent: A
      needs: []
    - id: e
      agent: E
      needs: [c]
    - id: c
      agent: C
      needs: [a]
`

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
