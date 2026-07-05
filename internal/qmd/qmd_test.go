package qmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/config"
)

func TestAnalyzeConfiguredBinaryMissing(t *testing.T) {
	// run == nil forces the real PATH lookup of the configured binary.
	res := Analyze("/b", nil, &config.QMD{Path: "okf-nonexistent-qmd-xyz"}, nil)
	if res.Unavailable == "" || !strings.Contains(res.Unavailable, "okf-nonexistent-qmd-xyz") {
		t.Errorf("expected unavailable naming the configured path, got %q", res.Unavailable)
	}
}

var testConcepts = []Concept{
	{Rel: "neo4j.md", Abs: "/b/neo4j.md", Text: "graph database"},
	{Rel: "graphrag.md", Abs: "/b/graphrag.md", Text: "graph rag"},
	{Rel: "widget.md", Abs: "/b/widget.md", Text: "widgets"},
}

// fakeRunner returns recorded qmd output keyed by the query, so each concept's
// vsearch yields realistic (not identical) results.
func fakeRunner(status string) Runner {
	return func(dir string, args ...string) ([]byte, error) {
		if args[0] == "status" {
			return []byte(status), nil
		}
		switch args[1] { // the vsearch query text
		case "graph database":
			return []byte(`noise before json
[{"score":0.99,"file":"./neo4j.md"},{"score":0.91,"file":"./graphrag.md"},{"score":0.20,"file":"./widget.md"}]`), nil
		case "graph rag":
			return []byte(`[{"score":0.99,"file":"./graphrag.md"},{"score":0.90,"file":"./neo4j.md"},{"score":0.15,"file":"./widget.md"}]`), nil
		default:
			return []byte(`[{"score":0.99,"file":"./widget.md"},{"score":0.10,"file":"./neo4j.md"}]`), nil
		}
	}
}

func TestAnalyzeNearDuplicates(t *testing.T) {
	res := Analyze("/b", testConcepts, &config.QMD{NearDuplicateThreshold: 0.85},
		fakeRunner("  Total:    3 files indexed\n  Vectors:  3 embedded\n"))

	if res.Unavailable != "" {
		t.Fatalf("unexpected unavailable: %s", res.Unavailable)
	}
	if res.StaleReason != "" {
		t.Errorf("stale = %q, want fresh", res.StaleReason)
	}
	if len(res.NearDup) != 1 {
		t.Fatalf("got %d pairs, want 1: %+v", len(res.NearDup), res.NearDup)
	}
	p := res.NearDup[0]
	if p.A != "graphrag.md" || p.B != "neo4j.md" || p.Score < 0.85 {
		t.Errorf("pair = %+v, want graphrag.md/neo4j.md score>=0.85", p)
	}
}

func TestAnalyzeStale(t *testing.T) {
	res := Analyze("/b", testConcepts, &config.QMD{NearDuplicateThreshold: 0.85},
		fakeRunner("  Total:    3 files indexed\n  Vectors:  1 embedded\n"))
	if res.Unavailable != "" {
		t.Fatalf("unexpected unavailable: %s", res.Unavailable)
	}
	if res.StaleReason == "" {
		t.Error("expected a stale reason when embedded < indexed")
	}
}

func TestAnalyzeUnavailable(t *testing.T) {
	res := Analyze("/b", testConcepts, &config.QMD{},
		fakeRunner("  Total:    0 files indexed\n  Vectors:  0 embedded\n"))
	if res.Unavailable == "" {
		t.Error("expected unavailable when the index is empty")
	}
}

func TestAnalyzeStatusError(t *testing.T) {
	run := func(dir string, args ...string) ([]byte, error) { return nil, fmt.Errorf("boom") }
	res := Analyze("/b", testConcepts, &config.QMD{}, run)
	if res.Unavailable == "" {
		t.Error("expected unavailable when qmd status errors")
	}
}

func TestNeighbors(t *testing.T) {
	run := func(dir string, args ...string) ([]byte, error) {
		return []byte(`[{"score":0.90,"file":"./graphrag.md"},{"score":0.30,"file":"./widget.md"},{"score":0.99,"file":"./neo4j.md"}]`), nil
	}
	ns, err := Neighbors("/b", "graph database", testConcepts, 0.5, &config.QMD{}, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 { // widget (0.30) filtered by minSim
		t.Fatalf("got %d neighbors, want 2: %+v", len(ns), ns)
	}
	if ns[0].Rel != "neo4j.md" || ns[1].Rel != "graphrag.md" {
		t.Errorf("neighbors not sorted desc: %+v", ns)
	}
}

func TestParseStatusCounts(t *testing.T) {
	i, e, ok := parseStatusCounts([]byte("Documents\n  Total:    7 files indexed\n  Vectors:  5 embedded\n"))
	if !ok || i != 7 || e != 5 {
		t.Errorf("parseStatusCounts = %d,%d,%v want 7,5,true", i, e, ok)
	}
	if _, _, ok := parseStatusCounts([]byte("no counts here")); ok {
		t.Error("expected ok=false on unparseable status")
	}
}
