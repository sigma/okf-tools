package rules

import "testing"

// countByRule tallies findings per rule ID.
func countByRule(fs []Finding) map[string]int {
	m := map[string]int{}
	for _, f := range fs {
		m[f.Rule]++
	}
	return m
}

// TestGlossaryStructure covers OKFEXT-GLOSSARY-01: a clean glossary is silent,
// a malformed entry is flagged at its line, and disabling the extension silences
// it entirely (scoping to declared glossary files is inherent — the rule only
// walks b.Glossaries).
func TestGlossaryStructure(t *testing.T) {
	clean := loadFixture(t, "glossary-clean")
	if got := countByRule(Run(&Context{Bundle: clean, Config: clean.Config}, nil, nil))["OKFEXT-GLOSSARY-01"]; got != 0 {
		t.Errorf("clean glossary: OKFEXT-GLOSSARY-01 = %d, want 0", got)
	}

	bad := loadFixture(t, "glossary-01")
	fs := Run(&Context{Bundle: bad, Config: bad.Config}, nil, nil)
	if got := countByRule(fs)["OKFEXT-GLOSSARY-01"]; got != 1 {
		t.Fatalf("malformed glossary: OKFEXT-GLOSSARY-01 = %d, want 1", got)
	}
	for _, f := range fs {
		if f.Rule == "OKFEXT-GLOSSARY-01" && f.Line != 4 {
			t.Errorf("finding at line %d, want 4 (the bare bullet)", f.Line)
		}
	}

	// Disabled ⇒ no findings, even on the malformed fixture.
	bad.Config.Glossary.Enabled = false
	if got := countByRule(Run(&Context{Bundle: bad, Config: bad.Config}, nil, nil))["OKFEXT-GLOSSARY-01"]; got != 0 {
		t.Errorf("disabled: OKFEXT-GLOSSARY-01 = %d, want 0", got)
	}
}

// TestGlossaryAnchorResolves covers OKFEXT-GLOSSARY-02: cross-file and in-page
// anchors into a glossary must resolve to a term/heading slug. Defined term and
// heading anchors and no-fragment/non-glossary links are silent; undefined
// anchors fire; and the rule is promotable to a build-failing error.
func TestGlossaryAnchorResolves(t *testing.T) {
	b := loadFixture(t, "glossary-02")
	fs := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	if got := countByRule(fs)["OKFEXT-GLOSSARY-02"]; got != 2 {
		for _, f := range fs {
			t.Logf("%s %s:%d %s", f.Rule, f.Path, f.Line, f.Message)
		}
		t.Fatalf("OKFEXT-GLOSSARY-02 = %d, want 2 (#root-kekk cross-file, #ghost self)", got)
	}

	// Promotable to error: an explicit [rules] override stamps error severity,
	// which fails the build at the default --fail-on=error threshold.
	b.Config.Rules["OKFEXT-GLOSSARY-02"] = "error"
	promoted := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	var got int
	for _, f := range promoted {
		if f.Rule == "OKFEXT-GLOSSARY-02" {
			got++
			if f.Severity != Error {
				t.Errorf("promoted finding severity = %v, want error", f.Severity)
			}
		}
	}
	if got != 2 {
		t.Errorf("promoted OKFEXT-GLOSSARY-02 = %d, want 2", got)
	}
}

// TestGlossaryTermUnique covers OKFEXT-GLOSSARY-03: a term-term slug collision
// and a term-heading slug collision each fire (at the later line); an all-unique
// glossary is silent.
func TestGlossaryTermUnique(t *testing.T) {
	b := loadFixture(t, "glossary-03")
	fs := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	if got := countByRule(fs)["OKFEXT-GLOSSARY-03"]; got != 2 {
		for _, f := range fs {
			t.Logf("%s %s:%d %s", f.Rule, f.Path, f.Line, f.Message)
		}
		t.Fatalf("OKFEXT-GLOSSARY-03 = %d, want 2", got)
	}
	lines := map[int]bool{}
	for _, f := range fs {
		if f.Rule == "OKFEXT-GLOSSARY-03" {
			lines[f.Line] = true
		}
	}
	if !lines[4] || !lines[8] {
		t.Errorf("collisions reported at %v, want the later occurrences (lines 4 and 8)", lines)
	}

	clean := loadFixture(t, "glossary-clean")
	if got := countByRule(Run(&Context{Bundle: clean, Config: clean.Config}, nil, nil))["OKFEXT-GLOSSARY-03"]; got != 0 {
		t.Errorf("clean glossary: OKFEXT-GLOSSARY-03 = %d, want 0", got)
	}
}

// TestGlossaryOrphanTerm covers OKFEXT-GLOSSARY-04: a defined-but-unreferenced
// term fires one advisory info finding; a referenced term does not; disabling
// the extension silences it.
func TestGlossaryOrphanTerm(t *testing.T) {
	b := loadFixture(t, "glossary-04")
	fs := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	orphans := 0
	for _, f := range fs {
		if f.Rule == "OKFEXT-GLOSSARY-04" {
			orphans++
			if f.Severity != Info {
				t.Errorf("orphan-term severity = %v, want info", f.Severity)
			}
			if f.Line != 4 {
				t.Errorf("orphan-term at line %d, want 4 (the unreferenced term)", f.Line)
			}
		}
	}
	if orphans != 1 {
		t.Fatalf("OKFEXT-GLOSSARY-04 = %d, want 1 (only the unreferenced term)", orphans)
	}

	b.Config.Glossary.Enabled = false
	if got := countByRule(Run(&Context{Bundle: b, Config: b.Config}, nil, nil))["OKFEXT-GLOSSARY-04"]; got != 0 {
		t.Errorf("disabled: OKFEXT-GLOSSARY-04 = %d, want 0", got)
	}
}
