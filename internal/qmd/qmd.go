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

func execRunner(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("qmd", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("qmd %s: %v: %s", strings.Join(args, " "), err, lastLine(errb.String()))
	}
	return out.Bytes(), nil
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

// Analyze runs the qmd-backed checks over concepts, rooted at root. Pass a nil
// runner to use the real qmd binary.
func Analyze(root string, concepts []Concept, cfg *config.QMD, run Runner) *Result {
	if run == nil {
		if _, err := exec.LookPath("qmd"); err != nil {
			return &Result{Unavailable: "qmd not found on PATH (install qmd or set qmd.enabled=false)"}
		}
		run = execRunner
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
	byAbs := make(map[string]string, len(concepts))
	for _, c := range concepts {
		byAbs[filepath.Clean(c.Abs)] = c.Rel
	}

	seen := map[string]bool{}
	var pairs []Pair
	for _, c := range concepts {
		q := c.Text
		if len(q) > 4000 { // keep the query arg bounded
			q = q[:4000]
		}
		out, err := run(root, "vsearch", q, "--format", "json", "--full-path", "--all",
			"--min-score", strconv.FormatFloat(threshold, 'f', 2, 64))
		if err != nil {
			continue
		}
		best := map[string]float64{}
		for _, h := range parseHits(out) {
			rel, ok := byAbs[resolveAbs(root, h.File)]
			if !ok || rel == c.Rel {
				continue
			}
			if h.Score > best[rel] {
				best[rel] = h.Score
			}
		}
		for rel, score := range best {
			if score < threshold {
				continue
			}
			a, b := c.Rel, rel
			if a > b {
				a, b = b, a
			}
			key := a + "\x00" + b
			if seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, Pair{A: a, B: b, Score: score})
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
