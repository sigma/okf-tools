package rules

import "testing"

// TestHeadingAnchorResolves covers OKF203: a #heading anchor — cross-file
// (other.md#frag) or same-file (#frag) — into an ordinary non-glossary page
// must name a real heading. Good anchors and missing-file links (OKF202's job)
// are silent; the two dangling anchors fire at their lines; the rule disables
// via links.check_anchors and promotes to a build-failing error via [rules].
func TestHeadingAnchorResolves(t *testing.T) {
	b := loadFixture(t, "okf203")
	fs := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	if got := countByRule(fs)["OKF203"]; got != 2 {
		for _, f := range fs {
			t.Logf("%s %s:%d %s", f.Rule, f.Path, f.Line, f.Message)
		}
		t.Fatalf("OKF203 = %d, want 2 (#missing-section same-file, #nonexistent cross-file)", got)
	}
	lines := map[int]bool{}
	for _, f := range fs {
		if f.Rule == "OKF203" {
			lines[f.Line] = true
		}
	}
	// Line 11 is the same-file dangling anchor; line 15 the cross-file one. The
	// missing-file link on line 17 is OKF202's concern, never OKF203's.
	if !lines[11] || !lines[15] {
		t.Errorf("OKF203 findings at %v, want lines 11 (same-file) and 15 (cross-file)", lines)
	}

	// Disabled via the typed knob ⇒ silent.
	b.Config.Links.CheckAnchors = "off"
	if got := countByRule(Run(&Context{Bundle: b, Config: b.Config}, nil, nil))["OKF203"]; got != 0 {
		t.Errorf("check_anchors=off: OKF203 = %d, want 0", got)
	}

	// Promotable to a hard error via [rules], failing the build at --fail-on=error.
	b.Config.Links.CheckAnchors = "info"
	b.Config.Rules["OKF203"] = "error"
	promoted := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	got := 0
	for _, f := range promoted {
		if f.Rule == "OKF203" {
			got++
			if f.Severity != Error {
				t.Errorf("promoted OKF203 severity = %v, want error", f.Severity)
			}
		}
	}
	if got != 2 {
		t.Errorf("promoted OKF203 = %d, want 2", got)
	}
}

// TestHeadingAnchorSkipsGlossary confirms OKF203 does not double-report anchors
// that OKFEXT-GLOSSARY-02 already owns: cross-file anchors into a glossary file
// and same-file anchors inside a glossary source are skipped. On the glossary-02
// fixture only the one cross-file anchor into a plain concept page (b.md#whatever)
// is OKF203's — everything else there points at or lives in the glossary.
func TestHeadingAnchorSkipsGlossary(t *testing.T) {
	b := loadFixture(t, "glossary-02")
	fs := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)
	if got := countByRule(fs)["OKF203"]; got != 1 {
		for _, f := range fs {
			t.Logf("%s %s:%d %s", f.Rule, f.Path, f.Line, f.Message)
		}
		t.Fatalf("OKF203 on glossary-02 = %d, want 1 (only b.md#whatever)", got)
	}
}
