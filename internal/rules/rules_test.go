package rules

import (
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/qmd"
)

func loadFixture(t *testing.T, name string) *bundle.Bundle {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", name)
	root, cfg, err := bundle.Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover %s: %v", name, err)
	}
	b, err := bundle.Load(root, cfg)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return b
}

func TestParseSeverity(t *testing.T) {
	for s, want := range map[string]Severity{"off": Off, "info": Info, "warning": Warning, "error": Error} {
		if got, ok := ParseSeverity(s); !ok || got != want {
			t.Errorf("ParseSeverity(%q) = %v,%v", s, got, ok)
		}
	}
	if _, ok := ParseSeverity("nope"); ok {
		t.Error("ParseSeverity(nope) should fail")
	}
}

func TestEffective(t *testing.T) {
	if got := Effective(Get("OKF001"), config.Default()); got != Error {
		t.Errorf("conformance OKF001 = %v, want error", got)
	}

	capped := config.Default()
	capped.Rules = map[string]string{"OKF202": "error"}
	if got := Effective(Get("OKF202"), capped); got != Info {
		t.Errorf("OKF202 = %v, want info (hard-capped)", got)
	}

	fn := config.Default()
	fn.Filenames.Severity = "warning"
	if got := Effective(Get("OKF103"), fn); got != Warning {
		t.Errorf("OKF103 = %v, want warning (filenames.severity)", got)
	}

	ov := config.Default()
	ov.Rules = map[string]string{"OKF105": "error"}
	if got := Effective(Get("OKF105"), ov); got != Error {
		t.Errorf("OKF105 = %v, want error ([rules] override)", got)
	}

	if got := Effective(Get("OKF107"), config.Default()); got != Off {
		t.Errorf("OKF107 = %v, want off (require_description false)", got)
	}
}

func ruleSet(fs []Finding) map[string]bool {
	m := map[string]bool{}
	for _, f := range fs {
		m[f.Rule] = true
	}
	return m
}

func TestRunSelectIgnore(t *testing.T) {
	b := loadFixture(t, "okf001")
	ctx := &Context{Bundle: b, Config: b.Config}

	if !ruleSet(Run(ctx, nil, nil))["OKF001"] {
		t.Fatal("expected OKF001 in unfiltered run")
	}
	for _, f := range Run(ctx, map[string]bool{"OKF001": true}, nil) {
		if f.Rule != "OKF001" {
			t.Errorf("--select OKF001 leaked %s", f.Rule)
		}
	}
	if ruleSet(Run(ctx, nil, map[string]bool{"OKF001": true}))["OKF001"] {
		t.Error("--ignore OKF001 should drop it")
	}
}

func TestRegistryComplete(t *testing.T) {
	want := []string{
		"OKF001", "OKF002", "OKF003", "OKF004",
		"OKF101", "OKF102", "OKF103", "OKF104", "OKF105", "OKF106", "OKF107",
		"OKF201", "OKF202", "OKF206",
		"OKFEXT-QMD-01", "OKFEXT-QMD-02",
	}
	got := ruleSet(nil)
	for _, r := range All() {
		got[r.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("rule %s is not registered", id)
		}
	}
	if len(All()) != len(want) {
		t.Errorf("registry has %d rules, want %d", len(All()), len(want))
	}
}

func TestQMDRules(t *testing.T) {
	b := loadFixture(t, "happy")
	b.Config.QMD.Enabled = true
	ctx := &Context{
		Bundle: b,
		Config: b.Config,
		QMD: &qmd.Result{
			NearDup:     []qmd.Pair{{A: "graphrag.md", B: "neo4j.md", Score: 0.9}},
			StaleReason: "qmd embeddings are stale",
		},
	}
	got := ruleSet(Run(ctx, map[string]bool{"OKFEXT-QMD-01": true, "OKFEXT-QMD-02": true}, nil))
	if !got["OKFEXT-QMD-01"] {
		t.Error("expected OKFEXT-QMD-01 near-duplicate finding")
	}
	if !got["OKFEXT-QMD-02"] {
		t.Error("expected OKFEXT-QMD-02 staleness finding")
	}
}

func TestNeedsQMD(t *testing.T) {
	off := config.Default() // qmd disabled by default
	if NeedsQMD(off, nil, nil) {
		t.Error("qmd disabled: NeedsQMD should be false")
	}
	on := config.Default()
	on.QMD.Enabled = true
	if !NeedsQMD(on, nil, nil) {
		t.Error("qmd enabled: NeedsQMD should be true")
	}
	if NeedsQMD(on, nil, map[string]bool{"OKFEXT-QMD-01": true, "OKFEXT-QMD-02": true}) {
		t.Error("both qmd rules ignored: NeedsQMD should be false")
	}
	if NeedsQMD(on, map[string]bool{"OKF001": true}, nil) {
		t.Error("selecting only OKF001: NeedsQMD should be false")
	}
}
