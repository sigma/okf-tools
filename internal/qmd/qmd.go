// Package qmd integrates the optional qmd semantic-search tool for the
// qmd-backed worklist rules (OKF203 near-duplicate, OKF204 staleness). It shells
// out to the `qmd` binary; a missing binary or unready index degrades
// gracefully into a Result the rules surface as a single info finding, so the
// rest of okftool lint stays dependency-free.
package qmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sigma/okf-tools/internal/config"
)

// Runner executes a qmd subcommand in dir and returns its stdout. Overridable in
// tests so they can feed recorded output instead of a live qmd index.
type Runner func(dir string, args ...string) ([]byte, error)

// execRunnerFor runs the qmd binary at bin (a name resolved on PATH, or an
// absolute path from qmd.path).
func execRunnerFor(bin string) Runner {
	return func(dir string, args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return out.Bytes(), fmt.Errorf("%s %s: %v: %s", bin, strings.Join(args, " "), err, lastLine(errb.String()))
		}
		return out.Bytes(), nil
	}
}

// Concept is the minimal per-page input Analyze needs from the bundle.
type Concept struct {
	Rel  string // bundle-relative path
	Abs  string // absolute on-disk path
	Text string // body used as the similarity query
}

// Pair is a near-duplicate concept pair (A < B).
type Pair struct {
	A, B  string
	Score float64
}

// Result is what the qmd rules consume. Unavailable is non-empty when qmd or its
// index could not be used.
type Result struct {
	Unavailable string
	NearDup     []Pair // OKF203
	StaleReason string // OKF204 ("" = fresh)
}

// resolveRunner returns run if non-nil, else a runner for the configured qmd
// binary (erroring when it is not found on PATH).
func resolveRunner(cfg *config.QMD, run Runner) (Runner, error) {
	if run != nil {
		return run, nil
	}
	bin := cfg.Path
	if bin == "" {
		bin = "qmd"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("qmd binary %q not found (set qmd.path, add it to PATH, or qmd.enabled=false)", bin)
	}
	return execRunnerFor(bin), nil
}

// Analyze runs the qmd-backed checks over concepts, rooted at root. Pass a nil
// runner to use the real qmd binary.
func Analyze(root string, concepts []Concept, cfg *config.QMD, run Runner) *Result {
	run, err := resolveRunner(cfg, run)
	if err != nil {
		return &Result{Unavailable: err.Error()}
	}

	statusOut, err := run(root, "status")
	if err != nil {
		return &Result{Unavailable: "qmd status failed: " + err.Error()}
	}
	indexed, embedded, ok := parseStatusCounts(statusOut)
	if !ok || indexed == 0 {
		return &Result{Unavailable: "qmd index is empty or unavailable; run 'qmd collection add . && qmd update && qmd embed'"}
	}

	return &Result{
		StaleReason: staleReason(indexed, embedded),
		NearDup:     nearDuplicates(root, concepts, cfg, run),
	}
}

func staleReason(indexed, embedded int) string {
	if embedded < indexed {
		return fmt.Sprintf("qmd embeddings are stale: %d/%d documents embedded; run 'qmd update && qmd embed'", embedded, indexed)
	}
	return ""
}

func nearDuplicates(root string, concepts []Concept, cfg *config.QMD, run Runner) []Pair {
	threshold := cfg.NearDuplicateThreshold
	if threshold <= 0 {
		threshold = 0.85
	}
	byAbs := absIndex(concepts)

	seen := map[string]bool{}
	var pairs []Pair
	for _, c := range concepts {
		ns, err := queryPages(root, boundedQuery(c.Text), byAbs, threshold, run)
		if err != nil {
			continue
		}
		for _, n := range ns {
			if n.Rel == c.Rel {
				continue
			}
			a, b := c.Rel, n.Rel
			if a > b {
				a, b = b, a
			}
			key := a + "\x00" + b
			if seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, Pair{A: a, B: b, Score: n.Score})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].A != pairs[j].A {
			return pairs[i].A < pairs[j].A
		}
		return pairs[i].B < pairs[j].B
	})
	return pairs
}

// Neighbor is a bundle page and its similarity to a query.
type Neighbor struct {
	Rel   string  `json:"page"`
	Score float64 `json:"sim"`
}

// Neighbors runs a single qmd vector search for query and returns the bundle
// pages most similar to it — page-level (max over chunks), filtered to >= minSim,
// sorted by score desc. run==nil resolves the qmd binary from cfg.Path.
func Neighbors(root, query string, concepts []Concept, minSim float64, cfg *config.QMD, run Runner) ([]Neighbor, error) {
	run, err := resolveRunner(cfg, run)
	if err != nil {
		return nil, err
	}
	return queryPages(root, boundedQuery(query), absIndex(concepts), minSim, run)
}

// queryPages runs one vsearch and aggregates chunk hits to page-level neighbors.
func queryPages(root, query string, byAbs map[string]string, minSim float64, run Runner) ([]Neighbor, error) {
	if minSim < 0 {
		minSim = 0
	}
	out, err := run(root, "vsearch", query, "--format", "json", "--full-path", "--all",
		"--min-score", strconv.FormatFloat(minSim, 'f', 2, 64))
	if err != nil {
		return nil, err
	}
	best := map[string]float64{}
	for _, h := range parseHits(out) {
		if rel, ok := byAbs[resolveAbs(root, h.File)]; ok && h.Score > best[rel] {
			best[rel] = h.Score
		}
	}
	var ns []Neighbor
	for rel, s := range best {
		if s >= minSim {
			ns = append(ns, Neighbor{Rel: rel, Score: s})
		}
	}
	sort.Slice(ns, func(i, j int) bool {
		if ns[i].Score != ns[j].Score {
			return ns[i].Score > ns[j].Score
		}
		return ns[i].Rel < ns[j].Rel
	})
	return ns, nil
}

func absIndex(concepts []Concept) map[string]string {
	m := make(map[string]string, len(concepts))
	for _, c := range concepts {
		m[filepath.Clean(c.Abs)] = c.Rel
	}
	return m
}

// boundedQuery keeps the query argument to a sane length for the exec call.
func boundedQuery(q string) string {
	if len(q) > 4000 {
		return q[:4000]
	}
	return q
}

// hit mirrors one entry of `qmd vsearch --format json`.
type hit struct {
	Score float64 `json:"score"`
	File  string  `json:"file"`
	Title string  `json:"title"`
}

// parseHits decodes the JSON array from qmd stdout, tolerating any leading
// progress lines qmd may print before the array.
func parseHits(out []byte) []hit {
	i := bytes.IndexByte(out, '[')
	if i < 0 {
		return nil
	}
	var hits []hit
	if err := json.NewDecoder(bytes.NewReader(out[i:])).Decode(&hits); err != nil {
		return nil
	}
	return hits
}

// resolveAbs turns a qmd result path (which may be "./x.md" relative to the
// index root, or absolute) into a cleaned absolute path.
func resolveAbs(root, file string) string {
	if filepath.IsAbs(file) {
		return filepath.Clean(file)
	}
	return filepath.Clean(filepath.Join(root, file))
}

var (
	totalRe   = regexp.MustCompile(`Total:\s+(\d+)\s+files indexed`)
	vectorsRe = regexp.MustCompile(`Vectors:\s+(\d+)\s+embedded`)
)

func parseStatusCounts(out []byte) (indexed, embedded int, ok bool) {
	s := string(out)
	tm := totalRe.FindStringSubmatch(s)
	vm := vectorsRe.FindStringSubmatch(s)
	if tm == nil || vm == nil {
		return 0, 0, false
	}
	indexed, _ = strconv.Atoi(tm[1])
	embedded, _ = strconv.Atoi(vm[1])
	return indexed, embedded, true
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
