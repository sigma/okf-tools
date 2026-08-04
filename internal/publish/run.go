package publish

// Run is one inline span of block content, flattened into the pipeline's neutral
// currency: either a late-bound reference (Ref set, Text empty) that a backend
// resolves through the transport, or a literal Text span optionally hyperlinked to
// an external Link URL. Ref and Link are mutually exclusive. It is the shared
// output of the graph flatten (graph.RunsOf) that both backends serialize — fs to
// Markdown, notion to rich-text JSON — so the projection of graph.Inline into runs
// lives in exactly one place instead of once per backend.
type Run struct {
	Text string
	Ref  SymbolicID
	Link string
}

// RefsOf collects the symbolic ids of the Ref runs in a slice, in order. It is
// pure over Run (no graph types), so a backend that re-chunks runs — Notion's
// per-block char-cap split — can re-derive each chunk's refs from the runs alone.
func RefsOf(runs []Run) []SymbolicID {
	var refs []SymbolicID
	for _, r := range runs {
		if r.Ref != "" {
			refs = append(refs, r.Ref)
		}
	}
	return refs
}
