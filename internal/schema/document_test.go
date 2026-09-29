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
