package bundle

import (
	"strings"
	"testing"
)

// The areas.json role marker designates the anchor host: marking the
// previously-hardwired CONTEXT.md area role: glossary yields exactly the glossary
// treatment okf.toml's [glossary] files gave it — same designation, same anchors
// — so okftool's behaviour is unchanged when the marker names the old host.
func TestAreasMarkerDesignatesContextGlossary(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\n",
		"areas.json": `{
		  "context": { "file": "CONTEXT.md", "type": "context", "role": "glossary" }
		}`,
		"index.md":   "- [c](CONTEXT.md)\n",
		"CONTEXT.md": "# Keys\n\n**Root KEK**: the top of the hierarchy.\n\n- **Foreign-rooted leaf**: a leaf.\n",
	})
	g := b.byRel["CONTEXT.md"]
	if g == nil || !g.Glossary {
		t.Fatalf("CONTEXT.md should be a glossary doc via the areas marker, got %+v", g)
	}
	for _, want := range []string{"root-kek", "foreign-rooted-leaf", "keys"} {
		if !g.HasAnchor(want) {
			t.Errorf("glossary missing anchor %q (have %+v)", want, g.Anchors)
		}
	}
	if b.Areas == nil {
		t.Fatal("b.Areas should be populated from areas.json")
	}
	if name, _, err := b.Areas.Glossary(); err != nil || name != "context" {
		t.Fatalf("b.Areas.Glossary() = (%q, %v), want (context, nil)", name, err)
	}
}

// The marker is not hardwired to CONTEXT.md: a bundle may host anchors in any
// file. Marking GLOSSARY.md resolves the glossary there and nowhere else.
func TestAreasMarkerNonContextArea(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\n",
		"areas.json": `{
		  "context": { "file": "CONTEXT.md",  "type": "context",  "role": "" },
		  "terms":   { "file": "GLOSSARY.md", "type": "glossary", "role": "glossary" }
		}`,
		"index.md":    "- [g](GLOSSARY.md)\n",
		"GLOSSARY.md": "**Root KEK**: the top.\n",
		"CONTEXT.md":  "---\ntype: Concept\ndescription: x\n---\nBody.\n",
	})
	g := b.byRel["GLOSSARY.md"]
	if g == nil || !g.Glossary {
		t.Fatalf("GLOSSARY.md should be the glossary via the marker, got %+v", g)
	}
	if !g.HasAnchor("root-kek") {
		t.Errorf("GLOSSARY.md missing anchor root-kek (have %+v)", g.Anchors)
	}
	if c := b.byRel["CONTEXT.md"]; c != nil && c.Glossary {
		t.Error("CONTEXT.md should NOT be a glossary — the marker names GLOSSARY.md")
	}
}

// The marker honours [glossary].enabled as the master opt-in: with the extension
// off, a marked file is not silently reclassified as a glossary (which would
// exempt it from concept rules). Behaviour is unchanged for a bundle that has
// not enabled the extension.
func TestAreasMarkerGatedOnEnabled(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = false\n",
		"areas.json": `{
		  "terms": { "file": "GLOSSARY.md", "type": "glossary", "role": "glossary" }
		}`,
		"index.md":    "---\nokf_version: \"0.1\"\n---\n- [g](GLOSSARY.md)\n",
		"GLOSSARY.md": "---\ntype: Concept\ndescription: x\n---\nBody.\n",
	})
	if g := b.byRel["GLOSSARY.md"]; g != nil && g.Glossary {
		t.Error("GLOSSARY.md should not be a glossary while [glossary] is disabled")
	}
	// The registry is still parsed and exposed for consumers (e.g. okfpub).
	if b.Areas == nil || !b.Areas.HasGlossary() {
		t.Error("b.Areas should still expose the parsed marker even when the extension is off")
	}
}

// okf.toml [glossary] files and the areas marker union: a bundle may name its
// glossary the old way and add the marker without either disturbing the other.
func TestAreasMarkerUnionsWithOkfTomlFiles(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{
		  "context": { "file": "CONTEXT.md",  "type": "context",  "role": "glossary" }
		}`,
		"index.md":   "- [c](CONTEXT.md)\n",
		"CONTEXT.md": "**Root KEK**: def\n",
	})
	if g := b.byRel["CONTEXT.md"]; g == nil || !g.Glossary {
		t.Fatalf("CONTEXT.md should be a glossary (okf.toml + marker agree), got %+v", g)
	}
}

// A malformed/ambiguous areas.json fails the whole load loudly — the anchor host
// must be unambiguous. Here two areas both claim role: glossary.
func TestAreasDuplicateRoleFailsLoad(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\n",
		"areas.json": `{
		  "context": { "file": "CONTEXT.md",  "type": "context",  "role": "glossary" },
		  "terms":   { "file": "GLOSSARY.md", "type": "glossary", "role": "glossary" }
		}`,
		"index.md":   "- [c](CONTEXT.md)\n",
		"CONTEXT.md": "**Root KEK**: def\n",
	})
	root, cfgPath, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	_, err = Load(root, cfgPath)
	if err == nil {
		t.Fatal("Load err = nil, want a duplicate-role failure")
	}
	if !strings.Contains(err.Error(), "load areas") || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("Load err = %q, want a wrapped duplicate-role error", err)
	}
}

// A bundle with no areas.json is unaffected: no registry, and okf.toml's
// [glossary] files remain the sole glossary designation.
func TestNoAreasFileLeavesOkfTomlInCharge(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "- [c](CONTEXT.md)\n",
		"CONTEXT.md": "**Root KEK**: def\n",
	})
	if b.Areas != nil {
		t.Error("b.Areas should be nil with no areas.json")
	}
	if g := b.byRel["CONTEXT.md"]; g == nil || !g.Glossary {
		t.Fatalf("CONTEXT.md should still be a glossary via okf.toml, got %+v", g)
	}
}
