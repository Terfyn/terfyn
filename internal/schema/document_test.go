package schema

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDocument_andLookup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	body := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"properties": {
			"summary": { "type": "string" },
			"count": { "type": "integer" },
			"findings": {
				"type": "array",
				"items": { "type": "object" }
			}
		},
		"additionalProperties": false
	}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadDocument(p)
	if err != nil {
		t.Fatal(err)
	}
	root := doc.Lookup(nil)
	if !root.Known || !root.Types.Has(TypeObject) || root.Missing {
		t.Fatalf("root = %+v", root)
	}
	sum := doc.Lookup([]string{"summary"})
	if !sum.Known || !sum.Types.Has(TypeString) {
		t.Fatalf("summary = %+v", sum)
	}
	cnt := doc.Lookup([]string{"count"})
	if !cnt.Known || !cnt.Types.Has(TypeInteger) {
		t.Fatalf("count = %+v", cnt)
	}
	miss := doc.Lookup([]string{"nope"})
	if !miss.Missing {
		t.Fatalf("undeclared property should be missing, got %+v", miss)
	}
	find := doc.Lookup([]string{"findings"})
	if !find.Known || !find.Types.Has(TypeArray) {
		t.Fatalf("findings = %+v", find)
	}
}

func TestLoadDocument_invalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDocument(p)
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CompileError, got %T: %v", err, err)
	}
}

func TestLoadDocument_booleanSchemas(t *testing.T) {
	dir := t.TempDir()

	truePath := filepath.Join(dir, "Any.json")
	if err := os.WriteFile(truePath, []byte("true"), 0o644); err != nil {
		t.Fatal(err)
	}
	trueDoc, err := LoadDocument(truePath)
	if err != nil {
		t.Fatalf("true schema must load: %v", err)
	}
	if trueDoc.Boolean == nil || !*trueDoc.Boolean {
		t.Fatalf("true document Boolean = %+v", trueDoc.Boolean)
	}
	if trueDoc.Raw != nil {
		t.Fatalf("boolean schema must not populate Raw, got %+v", trueDoc.Raw)
	}
	root := trueDoc.Lookup(nil)
	if root.Impossible || root.Missing || root.Known {
		t.Fatalf("true root must be unconstrained, got %+v", root)
	}
	nested := trueDoc.Lookup([]string{"anything"})
	if nested.Impossible || nested.Missing || nested.Known {
		t.Fatalf("true nested path must be unconstrained, got %+v", nested)
	}

	falsePath := filepath.Join(dir, "Never.json")
	if err := os.WriteFile(falsePath, []byte("false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	falseDoc, err := LoadDocument(falsePath)
	if err != nil {
		t.Fatalf("false schema must load: %v", err)
	}
	if falseDoc.Boolean == nil || *falseDoc.Boolean {
		t.Fatalf("false document Boolean = %+v", falseDoc.Boolean)
	}
	never := falseDoc.Lookup(nil)
	if !never.Impossible || never.Known || never.Missing {
		t.Fatalf("false root must be impossible, got %+v", never)
	}
	neverChild := falseDoc.Lookup([]string{"x"})
	if !neverChild.Impossible {
		t.Fatalf("descent through false must stay impossible, got %+v", neverChild)
	}

	str := TypeSet{TypeString: {}}
	if CompatibleLookup(LookupResult{Types: str, Known: true}, never) {
		t.Fatal("string must not flow into false")
	}
	if CompatibleLookup(LookupResult{}, never) {
		t.Fatal("untyped must not flow into false (not gradual)")
	}
	if !CompatibleLookup(never, never) {
		t.Fatal("false must flow into false")
	}
	if !CompatibleLookup(LookupResult{Types: str, Known: true}, root) {
		t.Fatal("string may flow into true (unconstrained)")
	}
	if CompatibleLookup(never, LookupResult{Types: str, Known: true}) {
		t.Fatal("false must not flow into string")
	}
}

func TestLoadDocument_rejectsNonSchemaJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "arr.json")
	if err := os.WriteFile(p, []byte(`[1,2]`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDocument(p)
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CompileError, got %T: %v", err, err)
	}
}

func TestCompatible(t *testing.T) {
	str := TypeSet{TypeString: {}}
	obj := TypeSet{TypeObject: {}}
	num := TypeSet{TypeNumber: {}}
	integ := TypeSet{TypeInteger: {}}
	if !Compatible(str, str) {
		t.Fatal("string→string")
	}
	if Compatible(str, obj) {
		t.Fatal("string↛object")
	}
	if !Compatible(integ, num) {
		t.Fatal("integer→number")
	}
	if Compatible(num, integ) {
		t.Fatal("number↛integer")
	}
	if !Compatible(nil, str) || !Compatible(str, nil) {
		t.Fatal("untyped is gradual")
	}
}

func TestLookup_additionalPropertiesOpen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "open.json")
	if err := os.WriteFile(p, []byte(`{"type":"object","properties":{"a":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadDocument(p)
	if err != nil {
		t.Fatal(err)
	}
	got := doc.Lookup([]string{"extra"})
	if got.Missing || got.Known {
		t.Fatalf("open additionalProperties should be untyped, got %+v", got)
	}
}
