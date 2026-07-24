package schema

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSchema(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	return p
}

func TestLoadAndLookup(t *testing.T) {
	p := writeSchema(t, `{
		"Name":   { "kind": "title",  "source": "derived" },
		"type":   { "kind": "select", "source": "derived", "options": ["adr", "idea"] },
		"status": { "kind": "select", "source": "frontmatter", "options": ["draft", "accepted"] },
		"tags":   { "kind": "list",   "source": "frontmatter" }
	}`)
	s, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Columns) != 4 {
		t.Fatalf("columns = %d, want 4", len(s.Columns))
	}

	// Lookup is case-insensitive (frontmatter keys are case-folded).
	if c, ok := s.Lookup("STATUS"); !ok || !c.IsFrontmatter() || len(c.Options) != 2 {
		t.Errorf("Lookup(STATUS) = %+v,%v", c, ok)
	}
	if c, ok := s.Lookup("type"); !ok || !c.IsDerived() {
		t.Errorf("Lookup(type) = %+v,%v; want derived", c, ok)
	}
	if _, ok := s.Lookup("nope"); ok {
		t.Error("Lookup(nope) should miss")
	}
	if !s.HasTitleColumn() {
		t.Error("HasTitleColumn = false, want true (Name is title-kind)")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("missing file should error")
	}
	if _, err := Load(writeSchema(t, `[]`)); err == nil {
		t.Error("a non-object schema should error")
	}
}
