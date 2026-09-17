package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A captured self-contained schema using a same-document "#/$defs/..." ref must compile and validate
// regardless of the ref label — including the project's convention (./schemas/x.json). The old
// implementation embedded the label in the base URL, so this ref normalized to a URL the compiler
// could not find and fell through to the file loader (compile error).
func TestValidateContent_sameDocumentDefsRefWithPathLabel(t *testing.T) {
	captured := []byte(`{"$defs":{"o":{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}},"$ref":"#/$defs/o"}`)
	if err := ValidateContent("./schemas/input.json", captured, []byte(`{"x":"ok"}`)); err != nil {
		t.Fatalf("self-contained #/$defs schema must compile and accept valid input: %v", err)
	}
	if err := ValidateContent("./schemas/input.json", captured, []byte(`{"y":1}`)); err == nil {
		t.Fatal("captured #/$defs schema must reject input missing the required field")
	}
}

// A captured schema with an external file:// $ref must NOT follow the live file. Reading the current
// file is exactly the drift capture exists to prevent, so an external ref is a loud compile error.
func TestValidateContent_externalFileRefNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "marker.json")
	// The current file on disk would ACCEPT anything...
	if err := os.WriteFile(target, []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	captured := []byte(`{"$ref":"file://` + filepath.ToSlash(target) + `"}`)
	err := ValidateContent("./schemas/x.json", captured, []byte(`{"anything":true}`))
	if err == nil {
		t.Fatal("external file:// $ref must not be followed (would re-read live disk); expected a compile error")
	}
	if !strings.Contains(err.Error(), "not permitted") && !strings.Contains(strings.ToLower(err.Error()), "compile") {
		t.Fatalf("error should reflect a refused external reference, got %v", err)
	}
}

func TestValidateContent_booleanSchemas(t *testing.T) {
	if err := ValidateContent("any", []byte("true"), []byte(`{"x":1}`)); err != nil {
		t.Fatalf("captured true schema must accept any instance: %v", err)
	}
	err := ValidateContent("never", []byte("false"), []byte(`{}`))
	if err == nil {
		t.Fatal("captured false schema must reject every instance")
	}
}

func TestValidateContent_plainSchemaStillWorks(t *testing.T) {
	s := []byte(`{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`)
	if err := ValidateContent("x", s, []byte(`{"x":"ok"}`)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := ValidateContent("x", s, []byte(`{"y":1}`)); err == nil {
		t.Fatal("expected invalid")
	}
}
