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
	if v, ok := trueDoc.Raw.(bool); !ok || !v {
		t.Fatalf("true document Raw = %#v, want bool true", trueDoc.Raw)
	}
	if raw, ok := trueDoc.Schema(); !ok || raw != true {
		t.Fatalf("true document must be a present schema, got %#v, %v", raw, ok)
	}
	if trueDoc.Object() != nil {
		t.Fatalf("boolean schema has no object form, got %+v", trueDoc.Object())
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
	if v, ok := falseDoc.Raw.(bool); !ok || v {
		t.Fatalf("false document Raw = %#v, want bool false", falseDoc.Raw)
	}
	// false is a PRESENT schema (never valid), not an absent one: Raw == nil must not be conflated
	// with it (issue #549 review).
	if raw, ok := falseDoc.Schema(); !ok || raw != false {
		t.Fatalf("false document must be a present schema, got %#v, %v", raw, ok)
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

func TestDocument_SchemaPresence(t *testing.T) {
	var nilDoc *Document
	if _, ok := nilDoc.Schema(); ok {
		t.Fatal("nil document carries no schema")
	}
	if _, ok := (&Document{}).Schema(); ok {
		t.Fatal("zero document carries no schema")
	}
	if _, ok := (&Document{Raw: map[string]any(nil)}).Schema(); ok {
		t.Fatal("typed-nil object map carries no schema")
	}
	if _, ok := (&Document{Raw: map[string]any{}}).Schema(); !ok {
		t.Fatal("empty object schema {} is a present schema")
	}
	if _, ok := (&Document{Raw: false}).Schema(); !ok {
		t.Fatal("false must be a present schema")
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

func TestProducerUnionMustFitConsumer(t *testing.T) {
	producer := TypeSet{TypeString: {}, TypeInteger: {}}
	consumer := TypeSet{TypeString: {}}
	if Compatible(producer, consumer) {
		t.Fatal("producer string|integer is not safely assignable to consumer string")
	}
}

func TestCompatibleUnionMatrix(t *testing.T) {
	str := TypeSet{TypeString: {}}
	num := TypeSet{TypeNumber: {}}
	integ := TypeSet{TypeInteger: {}}
	strInt := TypeSet{TypeString: {}, TypeInteger: {}}
	numStr := TypeSet{TypeNumber: {}, TypeString: {}}
	intNum := TypeSet{TypeInteger: {}, TypeNumber: {}}
	empty := TypeSet{}

	cases := []struct {
		name     string
		producer TypeSet
		consumer TypeSet
		want     bool
	}{
		{"string|integer -> string", strInt, str, false},
		{"string -> string|integer", str, strInt, true},
		{"integer -> number", integ, num, true},
		{"integer|string -> number|string", strInt, numStr, true},
		{"number -> integer|number", num, intNum, true},
		{"number -> integer", num, integ, false},
		{"string|integer -> string|integer", strInt, strInt, true},
		{"integer|number -> number", intNum, num, true},
		{"empty producer gradual", empty, str, true},
		{"empty consumer gradual", str, empty, true},
		{"disjoint unions", strInt, TypeSet{TypeBoolean: {}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Compatible(tc.producer, tc.consumer)
			if got != tc.want {
				t.Fatalf("Compatible(%s, %s) = %v, want %v", tc.producer, tc.consumer, got, tc.want)
			}
		})
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
