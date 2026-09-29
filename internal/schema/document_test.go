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
	if _, isObject := trueDoc.Raw.(map[string]any); isObject {
		t.Fatalf("boolean schema has no object form, got %#v", trueDoc.Raw)
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
	// never is the bottom type: a false producer flows into every consumer (issue #549 review).
	if !CompatibleLookup(never, LookupResult{Types: str, Known: true}) {
		t.Fatal("false must flow into string (bottom type)")
	}
	if !CompatibleLookup(never, LookupResult{}) {
		t.Fatal("false must flow into an untyped consumer")
	}
	if !CompatibleLookup(never, root) {
		t.Fatal("false must flow into true")
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

// TestLookup_booleanSubschemas covers Draft 2020-12 boolean schemas below the root (issue #549
// review): true is unconstrained wherever a subschema may appear, false reached by a key/index
// descent forbids that key (Missing), and false as the whole value — directly or through a local
// $ref — is never (Impossible).
func TestLookup_booleanSubschemas(t *testing.T) {
	type want struct {
		impossible, missing, known bool
		types                      string
	}
	unconstrained := want{types: "any"}
	forbidden := want{missing: true, types: "any"}
	never := want{impossible: true, types: "any"}
	str := want{known: true, types: "string"}
	cases := []struct {
		name   string
		schema string
		path   []string
		want   want
	}{
		{"properties false forbids key", `{"type":"object","properties":{"body":false,"s":{"type":"string"}}}`, []string{"body"}, forbidden},
		{"properties false forbids descent", `{"type":"object","properties":{"body":false}}`, []string{"body", "x"}, forbidden},
		{"sibling of false property", `{"type":"object","properties":{"body":false,"s":{"type":"string"}}}`, []string{"s"}, str},
		{"properties true is declared", `{"type":"object","properties":{"body":true},"additionalProperties":false}`, []string{"body"}, unconstrained},
		{"descent into true property", `{"type":"object","properties":{"body":true},"additionalProperties":false}`, []string{"body", "x"}, unconstrained},
		{"undeclared beside true property", `{"type":"object","properties":{"body":true},"additionalProperties":false}`, []string{"other"}, forbidden},
		{"items false forbids index", `{"type":"array","items":false}`, []string{"0"}, forbidden},
		{"items true", `{"type":"array","items":true}`, []string{"0"}, unconstrained},
		{"prefixItems before items false", `{"type":"array","prefixItems":[{"type":"string"}],"items":false}`, []string{"0"}, str},
		{"items false after prefixItems", `{"type":"array","prefixItems":[{"type":"string"}],"items":false}`, []string{"1"}, forbidden},
		{"additionalProperties true", `{"type":"object","additionalProperties":true}`, []string{"x"}, unconstrained},
		{"additionalProperties ref false", `{"type":"object","additionalProperties":{"$ref":"#/$defs/n"},"$defs":{"n":false}}`, []string{"x"}, forbidden},
		{"root ref false", `{"$ref":"#/$defs/n","$defs":{"n":false}}`, nil, never},
		{"descent through root ref false", `{"$ref":"#/$defs/n","$defs":{"n":false}}`, []string{"x"}, never},
		{"root ref chain to false", `{"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":false}}`, nil, never},
		{"root ref false with siblings", `{"type":"string","$ref":"#/$defs/n","$defs":{"n":false}}`, nil, never},
		{"root ref true keeps siblings", `{"type":"string","$ref":"#/$defs/y","$defs":{"y":true}}`, nil, str},
		{"property ref false", `{"type":"object","properties":{"body":{"$ref":"#/$defs/n"}},"$defs":{"n":false}}`, []string{"body"}, forbidden},
		{"property ref true", `{"type":"object","properties":{"body":{"$ref":"#/$defs/y"}},"additionalProperties":false,"$defs":{"y":true}}`, []string{"body"}, unconstrained},
		{"ref into prefixItems array", `{"type":"object","properties":{"body":{"$ref":"#/$defs/t/prefixItems/0"}},"$defs":{"t":{"prefixItems":[false]}}}`, []string{"body"}, forbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "s.json")
			if err := os.WriteFile(p, []byte(tc.schema), 0o644); err != nil {
				t.Fatal(err)
			}
			doc, err := LoadDocument(p)
			if err != nil {
				t.Fatalf("schema must compile: %v", err)
			}
			got := doc.Lookup(tc.path)
			gotW := want{impossible: got.Impossible, missing: got.Missing, known: got.Known, types: got.Types.String()}
			if gotW != tc.want {
				t.Fatalf("Lookup(%v) = %+v, want %+v", tc.path, gotW, tc.want)
			}
		})
	}
}

// TestLookup_patternProperties covers Draft 2020-12 §10.3.2.2–§10.3.2.3 (issue #549 review):
// every matching patternProperties subschema applies to a key alongside its properties entry, and
// additionalProperties applies only to a key neither properties nor any pattern matched.
func TestLookup_patternProperties(t *testing.T) {
	type want struct {
		missing, known bool
		types          string
	}
	unconstrained := want{types: "any"}
	forbidden := want{missing: true, types: "any"}
	str := want{known: true, types: "string"}
	integer := want{known: true, types: "integer"}
	cases := []struct {
		name   string
		schema string
		path   []string
		want   want
	}{
		{"true pattern escapes additionalProperties false", `{"type":"object","patternProperties":{"^body$":true},"additionalProperties":false}`, []string{"body"}, unconstrained},
		{"unmatched key still hits additionalProperties false", `{"type":"object","patternProperties":{"^body$":true},"additionalProperties":false}`, []string{"other"}, forbidden},
		{"false pattern forbids key", `{"type":"object","patternProperties":{"^body$":false}}`, []string{"body"}, forbidden},
		{"false pattern forbids descent", `{"type":"object","patternProperties":{"^body$":false}}`, []string{"body", "x"}, forbidden},
		{"typed pattern", `{"type":"object","patternProperties":{"^n_":{"type":"integer"}}}`, []string{"n_count"}, integer},
		{"pattern is unanchored", `{"type":"object","patternProperties":{"_id":{"type":"string"}},"additionalProperties":false}`, []string{"user_id_x"}, str},
		{"pattern does not shadow additionalProperties for other keys", `{"type":"object","patternProperties":{"^n_":{"type":"integer"}},"additionalProperties":{"type":"string"}}`, []string{"s"}, str},
		{"matched key skips additionalProperties", `{"type":"object","patternProperties":{"^n_":{"type":"integer"}},"additionalProperties":{"type":"string"}}`, []string{"n_x"}, integer},
		{"properties and true pattern", `{"type":"object","properties":{"body":{"type":"string"}},"patternProperties":{"^b":true}}`, []string{"body"}, str},
		{"properties and false pattern", `{"type":"object","properties":{"body":{"type":"string"}},"patternProperties":{"^b":false}}`, []string{"body"}, forbidden},
		{"true property and false pattern", `{"type":"object","properties":{"body":true},"patternProperties":{"^b":false}}`, []string{"body"}, forbidden},
		{"two typed conjuncts are gradual", `{"type":"object","properties":{"body":{"type":"string"}},"patternProperties":{"^b":{"type":"integer"}}}`, []string{"body"}, unconstrained},
		{"ref false pattern forbids key", `{"type":"object","patternProperties":{"^body$":{"$ref":"#/$defs/n"}},"$defs":{"n":false}}`, []string{"body"}, forbidden},
		{"pattern ignored for array-only type", `{"type":"array","patternProperties":{"^0$":false},"items":{"type":"string"}}`, []string{"0"}, str},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "s.json")
			if err := os.WriteFile(p, []byte(tc.schema), 0o644); err != nil {
				t.Fatal(err)
			}
			doc, err := LoadDocument(p)
			if err != nil {
				t.Fatalf("schema must compile: %v", err)
			}
			got := doc.Lookup(tc.path)
			gotW := want{missing: got.Missing, known: got.Known, types: got.Types.String()}
			if gotW != tc.want || got.Impossible {
				t.Fatalf("Lookup(%v) = %+v (impossible=%v), want %+v", tc.path, gotW, got.Impossible, tc.want)
			}
		})
	}
}

// TestLookup_patternPropertiesUncompilable: a pattern Go's regexp cannot compile (ECMA-262-only
// lookahead) makes its match unknown. LoadDocument rejects such a schema (the runtime compiler uses
// the same engine), so this builds the Document directly: the key must look up as unconstrained —
// never a false "not declared" from additionalProperties — unless a definite conjunct forbids it.
func TestLookup_patternPropertiesUncompilable(t *testing.T) {
	doc := &Document{Raw: map[string]any{
		"type":                 "object",
		"patternProperties":    map[string]any{"^(?=b)": false, "^x$": false},
		"properties":           map[string]any{"s": map[string]any{"type": "string"}},
		"additionalProperties": false,
	}}
	if got := doc.Lookup([]string{"body"}); got.Missing || got.Known || got.Impossible {
		t.Fatalf("unknown pattern match must be gradual, got %+v", got)
	}
	if got := doc.Lookup([]string{"x"}); !got.Missing {
		t.Fatalf("a definite false pattern still forbids the key, got %+v", got)
	}
	if got := doc.Lookup([]string{"s"}); !got.Known || !got.Types.Has(TypeString) {
		t.Fatalf("a definite properties conjunct still types the key, got %+v", got)
	}
}

func TestLookupResult_String(t *testing.T) {
	if got := (LookupResult{Impossible: true}).String(); got != "never" {
		t.Fatalf("impossible = %q", got)
	}
	if got := (LookupResult{}).String(); got != "any" {
		t.Fatalf("unconstrained = %q", got)
	}
	if got := (LookupResult{Types: TypeSet{TypeString: {}, TypeInteger: {}}, Known: true}).String(); got != "integer|string" {
		t.Fatalf("union = %q", got)
	}
}
