package publish

import "testing"

func TestNodeRefRoundTrip(t *testing.T) {
	for _, rel := range []string{
		"a.md",
		"docs/adr/0002.md",
		"a:b.md",            // a rel containing a colon must survive intact
		"node:not-a-scheme", // a rel that looks like a scheme is still just a rel
		"",
	} {
		id := NodeRef(rel)
		if got := id.Rel(); got != rel {
			t.Errorf("NodeRef(%q).Rel() = %q, want %q", rel, got, rel)
		}
		if name, ok := id.AnchorName(); ok {
			t.Errorf("NodeRef(%q).AnchorName() = (%q, true), want ok=false", rel, name)
		}
	}
}

func TestAnchorRefRoundTrip(t *testing.T) {
	for _, name := range []AnchorName{
		"glossary/root-kek",
		"glossary/term",
		"anchor:nested", // a name that looks like a scheme is still just a name
		"",
	} {
		id := AnchorRef(name)
		got, ok := id.AnchorName()
		if !ok || got != name {
			t.Errorf("AnchorRef(%q).AnchorName() = (%q, %v), want (%q, true)", name, got, ok, name)
		}
	}
}

func TestRelPanicsOnAnchor(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Rel() on an anchor ref did not panic")
		}
	}()
	_ = AnchorRef("glossary/term").Rel()
}

func TestRelPanicsOnUnschemed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Rel() on a schemeless id did not panic")
		}
	}()
	_ = SymbolicID("bare-string").Rel()
}
