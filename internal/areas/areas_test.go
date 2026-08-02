package areas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAreas writes body to a temp areas.json and returns its path.
func writeAreas(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "areas.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write areas.json: %v", err)
	}
	return p
}

// The marker names the previously-hardwired area: `context` -> CONTEXT.md with
// role: glossary. Resolution must yield exactly that file, so okftool's glossary
// + anchor behaviour is unchanged when the marker names the old anchor host.
func TestGlossaryMarkerOnContext(t *testing.T) {
	p := writeAreas(t, `{
	  "adr":     { "directory": "docs/adr", "type": "adr" },
	  "context": { "file": "CONTEXT.md", "type": "context", "role": "glossary" }
	}`)
	r, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !r.HasGlossary() {
		t.Fatal("HasGlossary = false, want true")
	}
	name, a, err := r.Glossary()
	if err != nil {
		t.Fatalf("Glossary: %v", err)
	}
	if name != "context" || a.File != "CONTEXT.md" {
		t.Fatalf("Glossary = (%q, %q), want (context, CONTEXT.md)", name, a.File)
	}
	if got, ok := r.GlossaryFile(); !ok || got != "CONTEXT.md" {
		t.Fatalf("GlossaryFile = (%q, %v), want (CONTEXT.md, true)", got, ok)
	}
}

// The marker is not hardwired to CONTEXT.md: a bundle may host anchors in any
// file-backed area. Proves resolution comes from the marker, not a filename.
func TestGlossaryMarkerOnNonContextArea(t *testing.T) {
	p := writeAreas(t, `{
	  "handbook": { "directory": "handbook", "type": "guide" },
	  "terms":    { "file": "GLOSSARY.md", "type": "glossary", "role": "glossary" }
	}`)
	r, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	name, a, err := r.Glossary()
	if err != nil {
		t.Fatalf("Glossary: %v", err)
	}
	if name != "terms" || a.File != "GLOSSARY.md" {
		t.Fatalf("Glossary = (%q, %q), want (terms, GLOSSARY.md)", name, a.File)
	}
	if got, ok := r.GlossaryFile(); !ok || got != "GLOSSARY.md" {
		t.Fatalf("GlossaryFile = (%q, %v), want (GLOSSARY.md, true)", got, ok)
	}
}

// Absent marker: a registry with no glossary role is valid (Load succeeds), but
// resolving the anchor host is a clear error and HasGlossary is false, so a
// caller that requires one gets a legible failure.
func TestGlossaryMarkerAbsent(t *testing.T) {
	p := writeAreas(t, `{
	  "adr":   { "directory": "docs/adr", "type": "adr" },
	  "ideas": { "directory": "ideas",    "type": "idea" }
	}`)
	r, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.HasGlossary() {
		t.Fatal("HasGlossary = true, want false")
	}
	if _, ok := r.GlossaryFile(); ok {
		t.Fatal("GlossaryFile ok = true, want false")
	}
	if _, _, err := r.Glossary(); err == nil {
		t.Fatal("Glossary err = nil, want a clear absent-role error")
	} else if !strings.Contains(err.Error(), "no area declares role") {
		t.Fatalf("Glossary err = %q, want it to explain the absent role", err)
	}
}

// Duplicate marker: two areas both claiming role: glossary is a hard Load error
// naming both offenders, because the anchor host must be unambiguous.
func TestGlossaryMarkerDuplicate(t *testing.T) {
	p := writeAreas(t, `{
	  "context": { "file": "CONTEXT.md",  "type": "context",  "role": "glossary" },
	  "terms":   { "file": "GLOSSARY.md", "type": "glossary", "role": "glossary" }
	}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("Load err = nil, want a duplicate-role error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "multiple") || !strings.Contains(msg, "context") || !strings.Contains(msg, "terms") {
		t.Fatalf("Load err = %q, want it to name both duplicate areas", msg)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "both directory and file",
			body: `{ "x": { "directory": "d", "file": "f.md", "type": "t" } }`,
			want: "both a directory and a file",
		},
		{
			name: "neither directory nor file",
			body: `{ "x": { "type": "t" } }`,
			want: "neither a directory nor a file",
		},
		{
			name: "unknown role",
			body: `{ "x": { "file": "f.md", "type": "t", "role": "index" } }`,
			want: "not one of",
		},
		{
			name: "directory-backed glossary",
			body: `{ "x": { "directory": "d", "type": "t", "role": "glossary" } }`,
			want: "single file",
		},
		{
			name: "not an object",
			body: `["adr", "ideas"]`,
			want: "cannot unmarshal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeAreas(t, tc.body))
			if err == nil {
				t.Fatalf("Load err = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// A missing areas.json is surfaced as an ordinary os error for the caller to
// treat as "no registry"; it is never a validation failure.
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "areas.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("Load err = %v, want a not-exist error", err)
	}
}

// A nil registry resolves no glossary — lets the bundle keep a nil Areas field
// without special-casing it at every call site.
func TestNilRegistryGlossaryFile(t *testing.T) {
	var r *Registry
	if _, ok := r.GlossaryFile(); ok {
		t.Fatal("nil Registry GlossaryFile ok = true, want false")
	}
}
