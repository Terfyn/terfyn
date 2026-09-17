package schema

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_fixtureValidAndInvalid(t *testing.T) {
	schema := filepath.Join("testdata", "person.schema.json")

	err := Validate(schema, []byte(`{"name":"alice"}`))
	if err != nil {
		t.Fatalf("valid instance: %v", err)
	}

	err = Validate(schema, []byte(`{}`))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want *ValidationError for missing required field, got %T: %v", err, err)
	}
	if filepath.Base(verr.Path) != "person.schema.json" {
		t.Fatalf("error should mention schema path, got Path=%q: %v", verr.Path, verr)
	}

	err = Validate(schema, []byte(`{"name":1}`))
	if !errors.As(err, &verr) {
		t.Fatalf("want *ValidationError for wrong type, got %T: %v", err, err)
	}
}

func TestValidate_booleanSchemas(t *testing.T) {
	dir := t.TempDir()
	truePath := filepath.Join(dir, "true.json")
	falsePath := filepath.Join(dir, "false.json")
	if err := os.WriteFile(truePath, []byte("true"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(falsePath, []byte("false"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(truePath, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("true schema must accept any instance: %v", err)
	}
	if err := Validate(truePath, []byte(`null`)); err != nil {
		t.Fatalf("true schema must accept null: %v", err)
	}
	err := Validate(falsePath, []byte(`{}`))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("false schema must reject every instance, got %T: %v", err, err)
	}
}

func TestLoadDocument_missingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.schema.json")
	_, err := LoadDocument(missing)
	var fe *FileError
	if !errors.As(err, &fe) {
		t.Fatalf("want *FileError, got %T: %v", err, err)
	}
}

func TestValidate_missingSchemaFile_clearPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.schema.json")

	err := Validate(missing, []byte(`{}`))
	var fe *FileError
	if !errors.As(err, &fe) {
		t.Fatalf("want *FileError, got %T: %v", err, err)
	}
	if fe.Path != missing {
		t.Fatalf("FileError.Path = %q, want %q", fe.Path, missing)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist in chain: %v", err)
	}
	if !strings.Contains(fe.Error(), missing) {
		t.Fatalf("error should contain path: %q", fe.Error())
	}
}

func TestResolveSchemaPath_relativeUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(root, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "schemas", "x.json")
	if err := os.WriteFile(want, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSchemaPath(root, "./schemas/x.json")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResolveSchemaPath_rejectsEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveSchemaPath(root, "../outside.json")
	if err == nil {
		t.Fatal("expected error for path outside root")
	}
}
