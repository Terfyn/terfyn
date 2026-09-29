package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Terfyn/terfyn/internal/spec"
	"github.com/Terfyn/terfyn/internal/state"
)

func graphWithInputSchema(ref string) *spec.ProjectGraph {
	return &spec.ProjectGraph{
		Workflows: map[string]*spec.WorkflowResource{
			"wf": {Metadata: spec.Metadata{Name: "wf"}, Spec: spec.WorkflowSpec{Input: &spec.WorkflowInput{Schema: ref}}},
		},
	}
}

func TestCollectSchemas_readsReferencedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.json"), []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	schemas, warnings, err := CollectSchemas(graphWithInputSchema("./in.json"), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if schemas["./in.json"] != `{"type":"object"}` {
		t.Fatalf("schema content not captured: %v", schemas)
	}
}

func TestCollectSchemas_capturesToolOperationSchemas(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "op.json"), []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &spec.ProjectGraph{Tools: map[string]*spec.ToolResource{
		"github": {Metadata: spec.Metadata{Name: "github"}, Spec: spec.ToolSpec{
			Type:       "native",
			Operations: map[string]spec.ToolOperation{"read_pr": {Schema: "./op.json"}},
		}},
	}}
	schemas, warnings, err := CollectSchemas(g, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if schemas["./op.json"] != `{"type":"object"}` {
		t.Fatalf("tool operation schema not captured: %v", schemas)
	}
}

func TestCollectSchemas_missingFileWarnsNotFatal(t *testing.T) {
	root := t.TempDir()
	schemas, warnings, err := CollectSchemas(graphWithInputSchema("./gone.json"), root)
	if err != nil {
		t.Fatalf("missing schema must not be fatal: %v", err)
	}
	if len(schemas) != 0 {
		t.Fatalf("missing schema must not be captured: %v", schemas)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "gone.json") {
		t.Fatalf("expected a skip warning naming the file: %v", warnings)
	}
}

func TestSchemaBundle_capturedAndHydrated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.json"), []byte(`{"type":"object","required":["x"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	g := graphWithInputSchema("./in.json")

	digest, warnings, err := BuildAndPersist(ctx, store, g, "local", "v1", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	// A schema bundle artifact was stored alongside graph + manifest.
	snap, err := store.GetSnapshot(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SchemaBundleDigest == "" {
		t.Fatal("snapshot must reference a captured schema bundle")
	}

	h, err := HydrateGraph(ctx, store, digest)
	if err != nil {
		t.Fatal(err)
	}
	if h.Schemas["./in.json"] != `{"type":"object","required":["x"]}` {
		t.Fatalf("hydrated schema bundle missing content: %v", h.Schemas)
	}
}

func TestSchemaBundle_absentWhenNoSchemasAndDigestUnchanged(t *testing.T) {
	// A schema-less project keeps the same snapshot digest it had before schema capture (the
	// identity field is omitempty), so existing snapshots are unaffected.
	withEmptyRoot, err := Build(graphWithPolicy([]string{"github.read"}), "local", "v1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withEmptyMap, err := Build(graphWithPolicy([]string{"github.read"}), "local", "v1", map[string]string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withEmptyRoot.Snapshot.Digest != withEmptyMap.Snapshot.Digest {
		t.Fatal("no captured schemas must not change the snapshot digest")
	}
	for _, a := range withEmptyRoot.Artifacts {
		if a.Kind == state.ArtifactKindSchemaBundle {
			t.Fatal("no schema bundle artifact should be produced when there are no schemas")
		}
	}
}

func TestHydrateGraph_unsupportedSchemaBundleFormatRefuses(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	graphPayload, _ := MarshalGraph(graphWithPolicy([]string{"github.read"}))
	graphDigest := contentDigest(graphPayload)
	_ = store.PutArtifact(ctx, state.DeploymentArtifact{Digest: graphDigest, Kind: state.ArtifactKindResolvedGraph, FormatVersion: FormatGraphV1, Payload: graphPayload})
	_ = store.PutArtifact(ctx, state.DeploymentArtifact{Digest: "bundle1", Kind: state.ArtifactKindSchemaBundle, FormatVersion: "agentic.dev/schemabundle/v99", Payload: []byte("{}")})
	_ = store.PutSnapshot(ctx, state.DeploymentSnapshot{Digest: "snap1", FormatVersion: FormatSnapshotV1, GraphDigest: graphDigest, SchemaBundleDigest: "bundle1"})

	_, err := HydrateGraph(ctx, store, "snap1")
	if err == nil || !strings.Contains(err.Error(), "v99") || !strings.Contains(err.Error(), FormatSchemaBundleV1) {
		t.Fatalf("expected refusal naming both schema bundle versions, got %v", err)
	}
}

// A Draft 2020-12 boolean root schema (issue #549) is a real schema: it is captured into the schema
// bundle, hydrated verbatim, and — because the bundle digest is part of the snapshot identity — a
// project whose only difference is `true` vs `false` gets a different snapshot digest. Dropping a
// boolean schema from capture (treating it as "no schema") would make a pinned resume validate
// against nothing, and would make widening false→true invisible to the digest.
func TestSchemaBundle_capturesBooleanRootSchemas(t *testing.T) {
	ctx := context.Background()
	build := func(body string) (string, *Hydrated, map[string]string) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "in.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		schemas, warnings, err := CollectSchemas(graphWithInputSchema("./in.json"), root)
		if err != nil || len(warnings) != 0 {
			t.Fatalf("CollectSchemas(%q): warnings=%v err=%v", body, warnings, err)
		}
		if schemas["./in.json"] != body {
			t.Fatalf("boolean schema %q not captured verbatim: %v", body, schemas)
		}
		store := newMemStore()
		digest, _, err := BuildAndPersist(ctx, store, graphWithInputSchema("./in.json"), "local", "v1", root, nil)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := store.GetSnapshot(ctx, digest)
		if err != nil {
			t.Fatal(err)
		}
		if snap.SchemaBundleDigest == "" {
			t.Fatalf("snapshot for %q must reference a schema bundle", body)
		}
		h, err := HydrateGraph(ctx, store, digest)
		if err != nil {
			t.Fatal(err)
		}
		return digest, h, schemas
	}

	trueDigest, trueHyd, _ := build("true")
	falseDigest, falseHyd, _ := build("false")
	if trueHyd.Schemas["./in.json"] != "true" || falseHyd.Schemas["./in.json"] != "false" {
		t.Fatalf("hydrated bundles = %v / %v", trueHyd.Schemas, falseHyd.Schemas)
	}
	if trueDigest == falseDigest {
		t.Fatal("true vs false schema must change the snapshot digest")
	}
}
