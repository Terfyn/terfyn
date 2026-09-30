package deploy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/plan"
	"github.com/Terfyn/terfyn/internal/policy"
	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
)

func graphWithPolicy(permit []string) *spec.ProjectGraph {
	return &spec.ProjectGraph{
		Meta: spec.Metadata{Name: "demo"},
		Tools: map[string]*spec.ToolResource{
			"github": {
				Metadata: spec.Metadata{Name: "github"},
				Spec: spec.ToolSpec{
					Type:               "native",
					OperationsDeclared: true,
					Operations:         map[string]spec.ToolOperation{"read_pr": {Effects: []string{"github.read"}}},
				},
			},
		},
		Policies: map[string]*spec.PolicyResource{
			"default": {
				Metadata: spec.Metadata{Name: "default"},
				Spec:     spec.PolicySpec{Effects: &spec.PolicyEffects{Permit: permit}},
			},
		},
	}
}

func TestBuild_snapshotDigestStableAndManifestDigestPopulated(t *testing.T) {
	g := graphWithPolicy([]string{"github.read"})
	a, err := Build(g, "local", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(g, "local", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Snapshot.Digest != b.Snapshot.Digest {
		t.Fatal("identical graph must produce identical snapshot digest")
	}
	if a.Snapshot.CapabilityManifestDigest == "" {
		t.Fatal("snapshot must record the #204 capability manifest digest")
	}
	if a.Snapshot.GraphDigest == "" || a.Snapshot.FormatVersion != FormatSnapshotV1 {
		t.Fatalf("snapshot fields = %+v", a.Snapshot)
	}
	// Two artifacts: resolved graph + capability manifest.
	kinds := map[string]bool{}
	for _, art := range a.Artifacts {
		kinds[art.Kind] = true
	}
	if !kinds[state.ArtifactKindResolvedGraph] || !kinds[state.ArtifactKindCapabilityManifest] {
		t.Fatalf("expected graph+manifest artifacts, got %v", kinds)
	}
}

func TestBuild_digestChangesWhenPolicyWidens(t *testing.T) {
	narrow, _ := Build(graphWithPolicy([]string{"github.read"}), "local", "v1", nil, nil)
	wide, _ := Build(graphWithPolicy([]string{"github.read", "github.write"}), "local", "v1", nil, nil)
	if narrow.Snapshot.GraphDigest == wide.Snapshot.GraphDigest {
		t.Fatal("widening policy must change the graph digest")
	}
	if narrow.Snapshot.Digest == wide.Snapshot.Digest {
		t.Fatal("widening policy must change the snapshot digest")
	}
}

func TestBuild_snapshotDigestIndependentOfCompilerVersionExcludedFields(t *testing.T) {
	// compiler_version is part of identity (provenance), but the graph payload/digest is not.
	g := graphWithPolicy([]string{"github.read"})
	a, _ := Build(g, "local", "v1", nil, nil)
	b, _ := Build(g, "local", "v2", nil, nil)
	if a.Snapshot.GraphDigest != b.Snapshot.GraphDigest {
		t.Fatal("graph digest must not depend on compiler version")
	}
}

func TestGraphRoundTrip_preservesPolicyAndOperations(t *testing.T) {
	g := graphWithPolicy([]string{"github.read"})
	payload, err := MarshalGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalGraph(payload)
	if err != nil {
		t.Fatal(err)
	}
	pol := got.Policies["default"]
	if pol == nil || pol.Spec.Effects == nil || len(pol.Spec.Effects.Permit) != 1 || pol.Spec.Effects.Permit[0] != "github.read" {
		t.Fatalf("policy not preserved: %+v", got.Policies["default"])
	}
	if tr := got.Tools["github"]; tr == nil || !tr.Spec.OperationsDeclared {
		t.Fatalf("tool operations not preserved: %+v", got.Tools["github"])
	}
}

func TestMarshalGraph_ignoresSourcePositions(t *testing.T) {
	// Positions are diagnostic-only (json:"-"); two serializations of the same semantic graph with
	// different source positions must be byte-identical, or content addressing breaks (#207 / #187).
	a := graphWithPolicy([]string{"github.read"})
	a.Tools["github"].Pos = spec.Pos{File: "a.yaml", Line: 1, Column: 1}
	b := graphWithPolicy([]string{"github.read"})
	b.Tools["github"].Pos = spec.Pos{File: "elsewhere/b.yaml", Line: 99, Column: 42}

	pa, err := MarshalGraph(a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := MarshalGraph(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(pa) != string(pb) {
		t.Fatalf("source positions leaked into the canonical graph payload:\n%s\n%s", pa, pb)
	}
}

// A pinned run compiles its policy from the hydrated graph (not the on-disk snapshot). Preset
// policies must survive that round-trip: ResolvedPreset is json:"-" and dropped, but the `preset`
// string is preserved and policy.Compile re-resolves it, so approvals/presets/safety are faithful.
func TestGraphRoundTrip_presetPolicyCompilesIdentically(t *testing.T) {
	g := &spec.ProjectGraph{
		Spec:      spec.ProjectSpec{Defaults: &spec.ProjectDefaults{Policy: "default"}},
		Tools:     map[string]*spec.ToolResource{"sh": {Metadata: spec.Metadata{Name: "sh"}, Spec: spec.ToolSpec{Type: "native"}}},
		Policies:  map[string]*spec.PolicyResource{"default": {Metadata: spec.Metadata{Name: "default"}, Spec: spec.PolicySpec{Preset: "shell_safe"}}},
		Workflows: map[string]*spec.WorkflowResource{"wf": {Metadata: spec.Metadata{Name: "wf"}, Spec: spec.WorkflowSpec{Policy: "default"}}},
	}
	spec.NormalizeProjectGraph(g)
	before, err := policy.Compile(g, "default")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalGraph(payload)
	if err != nil {
		t.Fatal(err)
	}
	after, err := policy.Compile(got, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Residual, after.Residual) {
		t.Fatalf("preset residual not preserved: before=%+v after=%+v", before.Residual, after.Residual)
	}
	if !after.Residual.ShellSafe {
		t.Fatal("shell_safe preset lost through the snapshot round-trip")
	}
}

// A workflow whose ONLY graph-mode signal is an empty declared `needs:` (a parallel-only workflow,
// or an .agent `parallel { }` where the lowerer sets NeedsDeclared with empty Needs) must reproduce
// graph mode after hydration — otherwise resume silently switches concurrent roots to a sequential
// chain, changing side-effect order and fan-out. Needs is omitempty, so this depends on
// NeedsDeclared being JSON identity (#207), the same fix #204 applied to OperationsDeclared.
func TestGraphRoundTrip_parallelOnlyWorkflowKeepsGraphMode(t *testing.T) {
	g := &spec.ProjectGraph{
		Workflows: map[string]*spec.WorkflowResource{
			"wf": {Metadata: spec.Metadata{Name: "wf"}, Spec: spec.WorkflowSpec{Steps: []spec.WorkflowStep{
				{ID: "a", Uses: "tool.x.y", NeedsDeclared: true}, // empty needs, declared -> a root in graph mode
				{ID: "b", Uses: "tool.x.y", NeedsDeclared: true},
			}}},
		},
	}
	if !spec.WorkflowUsesExplicitNeeds(g.Workflows["wf"].Spec.Steps) {
		t.Fatal("precondition: source workflow should be in graph mode")
	}
	payload, err := MarshalGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalGraph(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.WorkflowUsesExplicitNeeds(got.Workflows["wf"].Spec.Steps) {
		t.Fatal("hydrated workflow lost graph mode: NeedsDeclared did not survive the snapshot round-trip")
	}
}

// A whole-document agent call is a call SHAPE (#550): the with: map alone
// ({"arg0": v}) is identical for `Reviewer(v)` and `Reviewer(arg0: v)`. The shape
// bit must survive the snapshot round-trip or a pinned resume validates/executes
// the step under the wrong ABI, and it must be part of the graph digest.
func TestGraphRoundTrip_wholeDocumentAgentCallShape(t *testing.T) {
	build := func(whole bool) *spec.ProjectGraph {
		return &spec.ProjectGraph{
			Workflows: map[string]*spec.WorkflowResource{
				"wf": {Metadata: spec.Metadata{Name: "wf"}, Spec: spec.WorkflowSpec{Steps: []spec.WorkflowStep{
					{ID: "a", Agent: "reviewer", With: map[string]any{"arg0": "x"}, WholeDocument: whole, NeedsDeclared: true},
				}}},
			},
		}
	}
	for _, whole := range []bool{true, false} {
		payload, err := MarshalGraph(build(whole))
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalGraph(payload)
		if err != nil {
			t.Fatal(err)
		}
		if h := got.Workflows["wf"].Spec.Steps[0].WholeDocument; h != whole {
			t.Fatalf("WholeDocument = %v after snapshot round-trip, want %v", h, whole)
		}
	}
	dTrue, err := plan.ResolvedGraphDigest(build(true))
	if err != nil {
		t.Fatal(err)
	}
	dFalse, err := plan.ResolvedGraphDigest(build(false))
	if err != nil {
		t.Fatal(err)
	}
	if dTrue == dFalse {
		t.Fatal("call shape must change the resolved graph digest")
	}
}

func TestScanLiteralSecrets(t *testing.T) {
	g := &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{
		"api": {Metadata: spec.Metadata{Name: "api"}, Spec: spec.ToolSpec{Type: "http", HTTP: &spec.ToolHTTP{
			Headers: map[string]string{"Authorization": "ghp_literalsecret", "Content-Type": "application/json"},
		}}},
		"ref": {Metadata: spec.Metadata{Name: "ref"}, Spec: spec.ToolSpec{Type: "http", HTTP: &spec.ToolHTTP{
			Headers: map[string]string{"Authorization": "env:GH_TOKEN"},
		}}},
	}}
	warnings := ScanLiteralSecrets(g)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one literal-secret warning, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "api") || !strings.Contains(warnings[0], "Authorization") {
		t.Fatalf("warning should name the tool and header: %q", warnings[0])
	}
	// The env: reference is preserved verbatim in the payload — never redacted.
	payload, _ := MarshalGraph(g)
	if !strings.Contains(string(payload), "env:GH_TOKEN") {
		t.Fatal("env: reference must be persisted verbatim")
	}
}

type memArtifactStore struct {
	artifacts map[string]state.DeploymentArtifact
	snapshots map[string]state.DeploymentSnapshot
}

func newMemStore() *memArtifactStore {
	return &memArtifactStore{
		artifacts: map[string]state.DeploymentArtifact{},
		snapshots: map[string]state.DeploymentSnapshot{},
	}
}

func (m *memArtifactStore) PutArtifact(_ context.Context, a state.DeploymentArtifact) error {
	if _, ok := m.artifacts[a.Digest]; !ok {
		m.artifacts[a.Digest] = a
	}
	return nil
}
func (m *memArtifactStore) GetArtifact(_ context.Context, digest string) (*state.DeploymentArtifact, error) {
	a, ok := m.artifacts[digest]
	if !ok {
		return nil, errNoRows
	}
	return &a, nil
}
func (m *memArtifactStore) PutSnapshot(_ context.Context, s state.DeploymentSnapshot) error {
	if _, ok := m.snapshots[s.Digest]; !ok {
		m.snapshots[s.Digest] = s
	}
	return nil
}
func (m *memArtifactStore) GetSnapshot(_ context.Context, digest string) (*state.DeploymentSnapshot, error) {
	s, ok := m.snapshots[digest]
	if !ok {
		return nil, errNoRows
	}
	return &s, nil
}
func (m *memArtifactStore) SetCurrentSnapshot(context.Context, string, string) error { return nil }
func (m *memArtifactStore) CurrentSnapshotDigestForEnv(context.Context, string) (string, error) {
	return "", errNoRows
}
func (m *memArtifactStore) PruneUnreferencedArtifacts(context.Context) (int64, error) { return 0, nil }

var errNoRows = errNoRowsT{}

type errNoRowsT struct{}

func (errNoRowsT) Error() string { return "no rows" }

func TestHydrateGraph_roundTripThroughStore(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	g := graphWithPolicy([]string{"github.read"})
	digest, _, err := BuildAndPersist(ctx, store, g, "local", "v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := HydrateGraph(ctx, store, digest)
	if err != nil {
		t.Fatal(err)
	}
	pol := h.Graph.Policies["default"]
	if pol == nil || len(pol.Spec.Effects.Permit) != 1 {
		t.Fatalf("hydrated policy wrong: %+v", pol)
	}
}

func TestHydrateGraph_unsupportedFormatRefusesLoudly(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	// A snapshot from a newer/unknown format must never be reinterpreted.
	_ = store.PutSnapshot(ctx, state.DeploymentSnapshot{
		Digest:        "snap1",
		FormatVersion: "agentic.dev/snapshot/v99",
		GraphDigest:   "g1",
	})
	_, err := HydrateGraph(ctx, store, "snap1")
	if err == nil || !strings.Contains(err.Error(), "v99") || !strings.Contains(err.Error(), FormatSnapshotV1) {
		t.Fatalf("expected refusal naming both versions, got %v", err)
	}
}

func TestBuildAndPersist_dedupes(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	g := graphWithPolicy([]string{"github.read"})
	d1, _, _ := BuildAndPersist(ctx, store, g, "local", "v1", "", nil)
	d2, _, _ := BuildAndPersist(ctx, store, g, "local", "v1", "", nil)
	if d1 != d2 {
		t.Fatal("identical build must produce identical digest")
	}
	if len(store.snapshots) != 1 || len(store.artifacts) != 2 {
		t.Fatalf("expected dedupe to 1 snapshot + 2 artifacts, got %d snapshots %d artifacts", len(store.snapshots), len(store.artifacts))
	}
}
