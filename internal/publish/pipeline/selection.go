package pipeline

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/areas"
	"github.com/sigma/okf-tools/internal/bundle"
)

// Selection is one published document's worth of the bundle: a name that
// identifies it across runs, a title, and the predicate saying which pages belong
// to it.
//
// A selection is a property of a RUN, not of the bundle — the same bundle
// publishes as several documents in a fan-out — so it is resolved here and handed
// to Generation as an option rather than narrowing the bundle itself
// (sigma/okf-tools#161).
type Selection struct {
	// Name is the stable identity stamped into the document's Drive appProperties,
	// so a re-run finds the same document. It is the area key or the path, never a
	// title, which a human may rename freely.
	Name string
	// Title is the document's display name at creation.
	Title string
	// Contains reports whether a bundle-relative page belongs to this selection.
	Contains func(rel string) bool
}

// SelectionPlan is the resolved outcome of a run's --select flags: the documents
// to publish, plus what the resolution had to say about the bundle.
type SelectionPlan struct {
	// Selections are the documents to publish, in a deterministic order.
	Selections []Selection
	// Warnings are non-fatal notes the caller should print — an area key that also
	// names a path, say.
	Warnings []string
	// Unclaimed is how many publishable pages no area claimed. An incomplete
	// registry is visible rather than fatal: turning a publish into a linting gate
	// would duplicate what okftool already owns.
	Unclaimed int
}

// ResolveSelections turns the requested --select values into a publish plan.
//
// With none requested the default is a FAN-OUT: one document per declared area,
// excluding the glossary area, because the glossary's content already reaches
// every document through the always-append rule and a standalone copy would be a
// duplicate with no reader. A bundle with no areas.json has no vocabulary to fan
// out over, so it publishes as one whole-bundle document.
func ResolveSelections(b *bundle.Bundle, requested []string) (*SelectionPlan, error) {
	plan := &SelectionPlan{}
	glossary, hasGlossary := glossaryHost(b)

	if len(requested) > 0 {
		for _, req := range requested {
			sel, warn, err := resolveOne(b, req)
			if err != nil {
				return nil, err
			}
			if warn != "" {
				plan.Warnings = append(plan.Warnings, warn)
			}
			plan.Selections = append(plan.Selections, withGlossary(sel, glossary, hasGlossary))
		}
		return plan, nil
	}

	if b.Areas == nil {
		plan.Selections = []Selection{{
			Name:     ".",
			Title:    path.Base(b.Root),
			Contains: func(string) bool { return true },
		}}
		return plan, nil
	}

	names := make([]string, 0, len(b.Areas.Areas))
	for name := range b.Areas.Areas {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		area := b.Areas.Areas[name]
		// The glossary area is excluded from the DEFAULT fan-out only; selecting it
		// explicitly still produces its own document.
		if area.Role == areas.RoleGlossary {
			continue
		}
		sel := Selection{Name: name, Title: name, Contains: areaContains(area)}
		plan.Selections = append(plan.Selections, withGlossary(sel, glossary, hasGlossary))
	}

	// Count pages no area claimed, so an incomplete registry is visible.
	//
	// This walks the LOADED docs, not PublishDocs: the bundle's own export scope
	// already drops an unclaimed page, so counting the narrowed set would always
	// report zero and the signal would be silently lost.
	for _, doc := range b.Docs {
		claimed := false
		for _, name := range names {
			if areaContains(b.Areas.Areas[name])(doc.Rel) {
				claimed = true
				break
			}
		}
		if !claimed && !(hasGlossary && doc.Rel == glossary) {
			plan.Unclaimed++
		}
	}
	return plan, nil
}

// resolveOne resolves a single --select value. An area key wins over a path of
// the same name, and the collision is reported rather than silently preferred; a
// disambiguating prefix syntax was rejected as a tax on every invocation for a
// rare clash (#151).
func resolveOne(b *bundle.Bundle, req string) (Selection, string, error) {
	req = strings.Trim(req, "/")
	if req == "" || req == "." {
		return Selection{
			Name:     ".",
			Title:    path.Base(b.Root),
			Contains: func(string) bool { return true },
		}, "", nil
	}

	var warn string
	if b.Areas != nil {
		if area, ok := b.Areas.Areas[req]; ok {
			if pathMatches(b, req) {
				warn = fmt.Sprintf("selection %q names both an area and a path; the area wins", req)
			}
			return Selection{Name: req, Title: req, Contains: areaContains(area)}, warn, nil
		}
	}

	if !pathMatches(b, req) {
		return Selection{}, "", fmt.Errorf(
			"selection %q matches no area and no page; nothing to publish", req)
	}
	return Selection{
		Name:     req,
		Title:    strings.TrimSuffix(path.Base(req), ".md"),
		Contains: pathContains(req),
	}, "", nil
}

// withGlossary appends the glossary host to a selection that does not already
// contain it. A document that cites terms it does not carry sends every citation
// back to the repo, which defeats the self-contained-document premise; and the
// rule must not duplicate the glossary when the glossary IS the selection.
func withGlossary(sel Selection, glossary string, has bool) Selection {
	if !has || sel.Contains(glossary) {
		return sel
	}
	inner := sel.Contains
	sel.Contains = func(rel string) bool { return rel == glossary || inner(rel) }
	return sel
}

// glossaryHost reports the bundle's anchor host, resolved from the areas.json
// role marker rather than a filename convention.
func glossaryHost(b *bundle.Bundle) (string, bool) {
	if b.Areas == nil {
		return "", false
	}
	host, ok := b.Areas.GlossaryFile()
	if !ok {
		return "", false
	}
	return strings.Trim(host, "/"), true
}

func areaContains(a areas.Area) func(string) bool {
	if a.File != "" {
		file := strings.Trim(a.File, "/")
		return func(rel string) bool { return rel == file }
	}
	return pathContains(a.Directory)
}

func pathContains(p string) func(string) bool {
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return func(string) bool { return true }
	}
	if strings.HasSuffix(p, ".md") {
		return func(rel string) bool { return rel == p }
	}
	return func(rel string) bool { return rel == p || strings.HasPrefix(rel, p+"/") }
}

// pathMatches reports whether any publishable page lives at or under p.
func pathMatches(b *bundle.Bundle, p string) bool {
	in := pathContains(p)
	for _, doc := range b.PublishDocs() {
		if in(doc.Rel) {
			return true
		}
	}
	return false
}
