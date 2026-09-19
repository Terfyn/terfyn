package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
// Path is the absolute file used to compile; Raw is the object form used for static type lookup.
type Document struct {
	Path string
	Raw  map[string]any
}

// LookupResult is the static type of a JSON Schema path.
type LookupResult struct {
	Types TypeSet
	// Known is true when the schema names at least one instance type at this path.
	Known bool
	// Missing is true when the path is forbidden (undeclared property with
	// additionalProperties: false, or a descent through a non-object/array).
	Missing bool
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
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, &CompileError{Path: abs, Err: fmt.Errorf("schema must be a JSON object: %w", err)}
	}
	if _, err := defaultReg.getOrCompile(abs); err != nil {
		return nil, &CompileError{Path: abs, Err: err}
	}
	return &Document{Path: abs, Raw: raw}, nil
}

// Lookup returns the schema constraint at a dotted property path from the document root.
// An empty path is the root schema (typically the whole output/input object).
func (d *Document) Lookup(path []string) LookupResult {
	if d == nil || d.Raw == nil {
		return LookupResult{}
	}
	return lookupNode(d, d.Raw, path, 0)
}

// Compatible reports whether a producing type set can flow into a consuming type set.
// Untyped (empty) sides are compatible (gradual typing). integer may flow into number.
func Compatible(producer, consumer TypeSet) bool {
	if len(producer) == 0 || len(consumer) == 0 {
		return true
	}
	for t := range producer {
		if consumer.Has(t) {
			return true
		}
		if t == TypeInteger && consumer.Has(TypeNumber) {
			return true
		}
	}
	return false
}

func lookupNode(d *Document, node map[string]any, path []string, depth int) LookupResult {
	if node == nil || depth > maxSchemaDepth {
		return LookupResult{}
	}
	node = resolveLocalRef(d, node, depth)
	if node == nil {
		return LookupResult{}
	}
	if len(path) == 0 {
		ts := extractTypes(node)
		return LookupResult{Types: ts, Known: len(ts) > 0}
	}
	key := path[0]
	types := extractTypes(node)

	if props, ok := asObject(node["properties"]); ok {
		if sub, ok := props[key]; ok {
			if sm := asSchemaMap(sub); sm != nil {
				return lookupNode(d, sm, path[1:], depth+1)
			}
		}
	}
	if (len(types) == 0 || types.Has(TypeArray)) && isJSONIndex(key) {
		if items := asSchemaMap(node["items"]); items != nil {
			return lookupNode(d, items, path[1:], depth+1)
		}
	}
	if len(types) == 0 || types.Has(TypeObject) {
		return additionalPropertiesLookup(d, node, path, depth)
	}
	return LookupResult{Missing: true}
}

func additionalPropertiesLookup(d *Document, node map[string]any, path []string, depth int) LookupResult {
	ap, ok := node["additionalProperties"]
	if !ok {
		return LookupResult{}
	}
	switch t := ap.(type) {
	case bool:
		if !t {
			return LookupResult{Missing: true}
		}
		return LookupResult{}
	default:
		if sm := asSchemaMap(t); sm != nil {
			return lookupNode(d, sm, path[1:], depth+1)
		}
		return LookupResult{}
	}
}

func resolveLocalRef(d *Document, node map[string]any, depth int) map[string]any {
	if d == nil || node == nil {
		return node
	}
	ref, ok := node["$ref"].(string)
	if !ok || ref == "" {
		return node
	}
	if !strings.HasPrefix(ref, "#/") {
		return node
	}
	if depth > maxSchemaDepth {
		return node
	}
	cur := any(d.Raw)
	for _, p := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return node
		}
		cur, ok = m[p]
		if !ok {
			return node
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return node
	}
	return m
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

func asSchemaMap(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

func isJSONIndex(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}
