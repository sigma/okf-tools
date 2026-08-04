package graph

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
)

// This file is the shared flatten: it projects a neutral block's content into the
// pipeline's flattened run currency (publish.Run) once, at the producer, so every
// backend consumes runs the same way instead of re-implementing the identical
// inlinesToRuns/reshape projection. The seam hands backends already-flattened runs;
// each backend keeps only its own tokenization policy (Cost, char-cap chunking) and
// its per-medium emit.

// RunsOf projects a neutral block's opaque Content into an ordered slice of inline
// runs, reporting the block kind, level, code-fence language, and whether the
// content exposed any first-class inline Refs (so a backend knows whether to fall
// back to the block's aggregate Refs). It understands the shared parser's
// BlockContent; a bare string or nil is tolerated as a degenerate single-run block.
func RunsOf(content any) (kind BlockKind, level int, language string, runs []publish.Run, hadInlineRefs bool) {
	switch c := content.(type) {
	case BlockContent:
		kind, level, language = c.Kind, c.Level, c.Language
		runs = inlinesToRuns(c.Inlines)
		for _, r := range runs {
			if r.Ref != "" {
				hadInlineRefs = true
				break
			}
		}
	case string:
		if c != "" {
			runs = append(runs, publish.Run{Text: c})
		}
	case nil:
		// empty block — still emits one unit
	default:
		runs = append(runs, publish.Run{Text: fmt.Sprint(c)})
	}
	return kind, level, language, runs, hadInlineRefs
}

// inlinesToRuns maps a neutral inline slice to flattened runs: a Ref inline becomes
// a late-bound Ref placeholder, a URL inline a hyperlinked span, and a plain text
// inline a literal span. Empty spans are dropped. Shared by RunsOf (block-level
// runs) and TableRunsOf (per-cell runs) so both project inlines identically.
func inlinesToRuns(inlines []Inline) []publish.Run {
	var runs []publish.Run
	for _, in := range inlines {
		switch {
		case in.Ref != nil:
			runs = append(runs, publish.Run{Ref: in.Ref.ID})
		case in.URL != "":
			runs = append(runs, publish.Run{Text: in.Text, Link: in.URL})
		case in.Text != "":
			runs = append(runs, publish.Run{Text: in.Text})
		}
	}
	return runs
}

// TableRunsOf reshapes a neutral Table block into its rows-of-cells-of-runs and the
// symbolic ids of every Ref cited in any cell, in row-major order, so the section's
// unit exposes them for the transport to gate and resolve. Each backend wraps the
// rows in its own table container; the cell projection is shared here.
func TableRunsOf(bc BlockContent) ([][][]publish.Run, []publish.SymbolicID) {
	var refs []publish.SymbolicID
	rows := make([][][]publish.Run, 0, len(bc.Rows))
	for _, r := range bc.Rows {
		cells := make([][]publish.Run, 0, len(r.Cells))
		for _, cell := range r.Cells {
			runs := inlinesToRuns(cell.Inlines)
			refs = append(refs, publish.RefsOf(runs)...)
			cells = append(cells, runs)
		}
		rows = append(rows, cells)
	}
	return rows, refs
}
