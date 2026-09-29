package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// JSONType is a JSON Schema instance type (draft 2020-12).
type JSONType string

const (
	TypeNull    JSONType = "null"
	TypeBoolean JSONType = "boolean"
	TypeObject  JSONType = "object"
	TypeArray   JSONType = "array"
	TypeNumber  JSONType = "number"
	TypeInteger JSONType = "integer"
	TypeString  JSONType = "string"
)

func isJSONType(s string) bool {
	switch JSONType(s) {
	case TypeNull, TypeBoolean, TypeObject, TypeArray, TypeNumber, TypeInteger, TypeString:
		return true
	default:
		return false
	}
}

// TypeSet is a set of JSON Schema types. Empty means the schema does not constrain type.
type TypeSet map[JSONType]struct{}

// Has reports whether t is in the set.
func (s TypeSet) Has(t JSONType) bool {
	if s == nil {
		return false
	}
	_, ok := s[t]
	return ok
}

func (s TypeSet) String() string {
	if len(s) == 0 {
		return "any"
	}
	names := make([]string, 0, len(s))
	for t := range s {
		names = append(names, string(t))
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// Document is a loaded JSON Schema held on the project graph after validate (issue #193).
// Path is the absolute file used to compile.
//
// Raw is the parsed root schema and is exactly one of two Draft 2020-12 forms: a
// map[string]any (object schema) or a bool (boolean schema: true accepts every instance, false
// rejects every instance). It is nil only for a Document that carries no schema content (never
// produced by [LoadDocument]); nil means "unresolved / gradual", never "boolean". Because a
// boolean schema is a non-nil interface value, every consumer that asks "is there a schema?"
// (project export, lookup) gets the right answer for true and false without a side field — use
// [Document.Schema] rather than comparing Raw to nil so the invariant lives in one place.
type Document struct {
	Path string
	Raw  any
}

// Schema returns the root schema (map[string]any or bool) and whether the document carries one.
// It is the single presence test: a boolean false schema is present (ok is true), a nil Document or
// a Document without content is not. A typed-nil object map is treated as absent.
func (d *Document) Schema() (raw any, ok bool) {
	if d == nil {
		return nil, false
	}
	switch v := d.Raw.(type) {
	case bool:
		return v, true
	case map[string]any:
		if v == nil {
			return nil, false
		}
		return v, true
	default:
		return nil, false
	}
}

// LookupResult is the static type of a JSON Schema path.
//
// Boolean schemas (Draft 2020-12 §4.3.2) are decoded in every subschema position Lookup descends
// through — the root, properties and patternProperties values, prefixItems/items,
// additionalProperties, and local "$ref" targets such as "$defs" entries. Applicators Lookup does
// not interpret (allOf/anyOf/oneOf/not, if/then/else, dependentSchemas, unevaluated*, …) are not
// consulted at all, boolean or not. A true subschema is unconstrained (the zero LookupResult). A
// false subschema becomes Impossible when it is the schema of the whole value being looked up, and
// Missing when it is reached by descending a key or index: {"properties":{"x":false}} is how
// Draft 2020-12 forbids a key, so the enclosing instance can exist and x must simply be absent —
// the same state as an undeclared key under additionalProperties: false. See
// [lookupNamedProperty] for how properties, patternProperties and additionalProperties combine.
type LookupResult struct {
	Types TypeSet
	// Known is true when the schema names at least one instance type at this path.
	Known bool
	// Missing is true when the path is forbidden: an undeclared property with
	// additionalProperties: false, a property/item whose subschema (under properties,
	// patternProperties, prefixItems/items or additionalProperties) is (or $refs to) boolean false,
	// or a descent through a non-object/array.
	Missing bool
	// Impossible is true when no value can exist at this path: the root schema is boolean false
	// or a local $ref to one (e.g. {"$ref":"#/$defs/n","$defs":{"n":false}}), or the path descends
	// through such a root. It is distinct from unconstrained (empty Types) so gradual typing
	// cannot treat never as any (issue #549); see [CompatibleLookup] for how it flows.
	// Only boolean false (directly or by local $ref) is detected — an unsatisfiable object schema
	// such as {"not":{}} or {"allOf":[false]} still looks up as its declared types.
	Impossible bool
}

// String renders the lookup as a diagnostic type name: "never", "any", or the type union.
func (r LookupResult) String() string {
	if r.Impossible {
		return "never"
	}
	return r.Types.String()
}

const maxSchemaDepth = 32

// LoadDocument reads, compiles, and returns a JSON Schema document.
// schemaPath is cleaned and passed through filepath.Abs (same as Validate).
func LoadDocument(schemaPath string) (*Document, error) {
	abs, err := filepath.Abs(filepath.Clean(schemaPath))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, &FileError{Path: abs, Op: "stat schema", Err: err}
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, &FileError{Path: abs, Op: "read schema", Err: err}
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil, &CompileError{Path: abs, Err: fmt.Errorf("schema must be a JSON Schema (object or boolean): %w", err)}
	}
	if _, err := defaultReg.getOrCompile(abs); err != nil {
		return nil, &CompileError{Path: abs, Err: err}
	}
	switch v := decoded.(type) {
	case bool, map[string]any:
		return &Document{Path: abs, Raw: v}, nil
	default:
		return nil, &CompileError{Path: abs, Err: fmt.Errorf("schema must be a JSON object or boolean, got %T", decoded)}
	}
}

// Lookup returns the schema constraint at a dotted property path from the document root.
// An empty path is the root schema (typically the whole output/input object).
func (d *Document) Lookup(path []string) LookupResult {
	if d == nil {
		return LookupResult{}
	}
	return lookupSchema(d, d.Raw, path, 0)
}

// Compatible reports whether every concrete producer type is accepted by the consumer.
// Untyped (empty) sides are compatible (gradual typing). integer may flow into number.
// Union producers are subtypes of the consumer: overlapping sets are not enough
// (string|integer is not assignable to string).
func Compatible(producer, consumer TypeSet) bool {
	if len(producer) == 0 || len(consumer) == 0 {
		return true
	}
	for t := range producer {
		if consumer.Has(t) {
			continue
		}
		if t == TypeInteger && consumer.Has(TypeNumber) {
			continue
		}
		return false
	}
	return true
}

// CompatibleLookup reports whether a producing lookup can flow into a consuming lookup. It is the
// one flow rule shared by the .agent checker and YAML step wiring.
//
// never (Impossible) is the bottom type. As a producer it is compatible with every consumer: a
// never-producing source cannot yield a value (an agent's output is always validated against its
// schema, so a false-output step cannot complete; a single-parameter workflow's input is validated
// at run start, so a false-typed parameter refuses the run), the downstream flow is dead, and there
// is no value that could violate the consumer — the same answer true / an untyped consumer gives,
// which Draft 2020-12 defines as accepting everything. As a consumer it accepts only another
// never: a false consumer is never gradual, so neither a typed nor an untyped producer may flow
// into it. Everything else is [Compatible].
//
// The producer half is sound only where the producer's schema is enforced at its source; this rule
// assumes that and does not check it. Known gap (pre-existing, not specific to never): a
// .agent workflow with more than one parameter gets no runtime input schema
// (check.wireWorkflowSchemas wires one only for a single parameter, mirroring lower.newEnv), so
// a multi-parameter workflow's parameter typed Never can in fact carry a value at run time, and
// this rule — like every other static flow check on such a parameter — does not stop it.
func CompatibleLookup(producer, consumer LookupResult) bool {
	if producer.Impossible {
		return true
	}
	if consumer.Impossible {
		return false
	}
	return Compatible(producer.Types, consumer.Types)
}

// lookupSchema looks up path in node, a Draft 2020-12 schema in either form: bool or object.
func lookupSchema(d *Document, node any, path []string, depth int) LookupResult {
	if depth > maxSchemaDepth {
		return LookupResult{}
	}
	switch v := node.(type) {
	case bool:
		if v {
			return LookupResult{}
		}
		// Descending through a never value stays never.
		return LookupResult{Impossible: true}
	case map[string]any:
		if v == nil {
			return LookupResult{}
		}
		return lookupObject(d, v, path, depth)
	default:
		return LookupResult{}
	}
}

func lookupObject(d *Document, node map[string]any, path []string, depth int) LookupResult {
	if target, ok := resolveLocalRef(d, node); ok {
		switch t := target.(type) {
		case bool:
			if !t {
				// $ref is a conjunct of node: anything and false is false.
				return LookupResult{Impossible: true}
			}
			// $ref to true adds no constraint; node's sibling keywords still apply.
		case map[string]any:
			return lookupSchema(d, t, path, depth+1)
		}
	}
	types := extractTypes(node)
	if len(path) == 0 {
		return LookupResult{Types: types, Known: len(types) > 0}
	}
	key := path[0]

	if res, ok := lookupNamedProperty(d, node, types, key, path[1:], depth); ok {
		return res
	}
	if (len(types) == 0 || types.Has(TypeArray)) && isJSONIndex(key) {
		idx, _ := strconv.Atoi(key)
		if prefix, ok := node["prefixItems"].([]any); ok && idx >= 0 && idx < len(prefix) {
			return lookupDescent(d, prefix[idx], path[1:], depth)
		}
		if items, ok := node["items"]; ok {
			return lookupDescent(d, items, path[1:], depth)
		}
	}
	if len(types) == 0 || types.Has(TypeObject) {
		if ap, ok := node["additionalProperties"]; ok {
			return lookupDescent(d, ap, path[1:], depth)
		}
		return LookupResult{}
	}
	return LookupResult{Missing: true}
}

// lookupNamedProperty resolves key against the keywords that govern a named property before the
// additionalProperties fallback (Draft 2020-12 §10.3.2): the "properties" entry for key, and every
// "patternProperties" entry whose regular expression matches key. All of them apply to the value
// at key at once (they are conjuncts), and additionalProperties applies only when none of them
// matched (§10.3.2.3). ok is false when no properties/patternProperties entry governs key, so the
// caller falls through to items / additionalProperties.
//
// The conjuncts are combined conservatively, since a LookupResult cannot represent an
// intersection of two constrained subschemas:
//   - any conjunct that forbids key (a false subschema, or a Missing deeper path) makes the key
//     Missing — anything and false is false, the same as properties: {x: false};
//   - conjuncts that are unconstrained (true, {}) add nothing and are dropped;
//   - one remaining constrained conjunct is the result;
//   - two or more constrained conjuncts look up as unconstrained (gradual), never as a guess at
//     their intersection.
//
// Patterns are compiled with Go's regexp (RE2) and matched unanchored, as Draft 2020-12 requires.
// JSON Schema specifies ECMA-262 regular expressions; the runtime validator
// (santhosh-tekuri/jsonschema's default engine) also compiles them with Go's regexp and rejects a
// pattern RE2 cannot compile at load time, so for a document from [LoadDocument] both sides agree
// on which keys match. A pattern that still fails to compile here (e.g. a hand-built Document, or
// ECMA-only syntax such as lookahead) makes its match unknown, and is not an error: the definite
// conjuncts above still apply, and when there are none the key looks up as unconstrained without
// consulting additionalProperties, so an unknown match never produces a false "not declared".
func lookupNamedProperty(d *Document, node map[string]any, types TypeSet, key string, rest []string, depth int) (LookupResult, bool) {
	var conjuncts []LookupResult
	unknown := false
	if props, ok := asObject(node["properties"]); ok {
		if sub, ok := props[key]; ok {
			conjuncts = append(conjuncts, lookupDescent(d, sub, rest, depth))
		}
	}
	if pats, ok := asObject(node["patternProperties"]); ok && (len(types) == 0 || types.Has(TypeObject)) {
		// Sorted so the combination is deterministic regardless of map order.
		names := make([]string, 0, len(pats))
		for p := range pats {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			re, err := compilePattern(p)
			if err != nil {
				unknown = true
				continue
			}
			if re.MatchString(key) {
				conjuncts = append(conjuncts, lookupDescent(d, pats[p], rest, depth))
			}
		}
	}
	if len(conjuncts) == 0 && !unknown {
		return LookupResult{}, false
	}
	var constrained []LookupResult
	for _, c := range conjuncts {
		if c.Missing {
			return LookupResult{Missing: true}, true
		}
		if c.Known || len(c.Types) > 0 {
			constrained = append(constrained, c)
		}
	}
	if len(constrained) != 1 {
		return LookupResult{}, true
	}
	return constrained[0], true
}

// patternCache memoizes compiled patternProperties expressions (and compile failures) by source.
var patternCache sync.Map // string -> patternEntry

type patternEntry struct {
	re  *regexp.Regexp
	err error
}

func compilePattern(p string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(p); ok {
		e := v.(patternEntry)
		return e.re, e.err
	}
	re, err := regexp.Compile(p)
	patternCache.Store(p, patternEntry{re: re, err: err})
	return re, err
}

// lookupDescent looks up rest in sub, the subschema governing one key or index of the current
// instance. A false subschema there forbids the key (it must be absent) rather than making the
// enclosing value impossible, so Impossible is reported as Missing.
func lookupDescent(d *Document, sub any, rest []string, depth int) LookupResult {
	res := lookupSchema(d, sub, rest, depth+1)
	if res.Impossible {
		return LookupResult{Missing: true}
	}
	return res
}

// resolveLocalRef returns the schema (map[string]any or bool) that node's local "#/..." $ref
// points at, and whether node has such a $ref that resolves. JSON Pointer tokens index objects
// and arrays.
func resolveLocalRef(d *Document, node map[string]any) (any, bool) {
	if d == nil || node == nil {
		return nil, false
	}
	ref, ok := node["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	cur := d.Raw
	for _, p := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			if cur, ok = c[p]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	switch cur.(type) {
	case bool, map[string]any:
		return cur, true
	default:
		return nil, false
	}
}

func extractTypes(node map[string]any) TypeSet {
	if node == nil {
		return nil
	}
	t, ok := node["type"]
	if !ok {
		return nil
	}
	out := TypeSet{}
	switch v := t.(type) {
	case string:
		if isJSONType(v) {
			out[JSONType(v)] = struct{}{}
		}
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok || !isJSONType(s) {
				continue
			}
			out[JSONType(s)] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func isJSONIndex(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}
