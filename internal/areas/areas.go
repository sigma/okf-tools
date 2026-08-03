// Package areas loads a bundle's /areas.json — the repo-root registry that maps
// each content area to a directory or single file, its unified-database row
// type, and an optional role marker. It is the config surface that designates
// the glossary / concept-anchor host page: an area entry carrying
// `role: glossary` is the anchor host, replacing any hardwired CONTEXT.md /
// area-name literal. Two consumers share this one definition — okftool's
// glossary/anchor rules and the okfpub publisher both resolve the anchor host
// from the marker, never from a filename convention.
//
// The file is a JSON object keyed by area name, so the key *is* the name and
// name uniqueness is enforced by JSON itself. A missing areas.json is not an
// error — a bundle configured entirely through okf.toml has no registry.
package areas

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// RoleGlossary marks the single area that hosts concept anchors — the glossary
// page. It is the only role the registry currently understands.
const RoleGlossary = "glossary"

// knownRoles is the closed vocabulary the Role field accepts (besides empty).
var knownRoles = []string{RoleGlossary}

// Area is one entry in areas.json: a content area mapped to either a directory
// or a single file, plus its row type and an optional role. The name lives in
// the map key, not here.
type Area struct {
	// Directory is the bundle-relative directory this area covers (forward
	// slashes). Exactly one of Directory / File is set.
	Directory string `json:"directory,omitempty"`
	// File is the bundle-relative single file this area covers (forward slashes).
	// Exactly one of Directory / File is set. The glossary-role area is always
	// file-backed — the anchor host is a single page (CONTEXT-FORMAT).
	File string `json:"file,omitempty"`
	// Type is the unified-database row type the area's pages carry.
	Type string `json:"type"`
	// Role is an optional marker; the empty string means "no role". The only
	// understood value is "glossary" (see RoleGlossary).
	Role string `json:"role,omitempty"`
}

// Registry is a parsed areas.json: area name -> entry, keyed verbatim by the
// JSON object key.
type Registry struct {
	// Areas maps each declared area's name to its body, verbatim from the file.
	Areas map[string]Area
	// Path is the file the registry was loaded from, for diagnostics.
	Path string
}

// Load reads, parses, and validates the areas.json at path. The file must be a
// JSON object keyed by area name; anything malformed — an entry that names both
// or neither a directory and a file, an unknown role, a directory-backed
// glossary, or more than one glossary role — is a hard error, because the
// registry is authoritative and the anchor host must be unambiguous.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]Area
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r := &Registry{Areas: m, Path: path}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// validate enforces the structural and role invariants. Area names are visited
// in sorted order so every diagnostic is deterministic regardless of JSON map
// iteration order.
func (r *Registry) validate() error {
	var glossary []string
	for _, name := range r.sortedNames() {
		a := r.Areas[name]
		hasDir, hasFile := a.Directory != "", a.File != ""
		switch {
		case hasDir && hasFile:
			return fmt.Errorf("%s: area %q names both a directory and a file; give exactly one", r.Path, name)
		case !hasDir && !hasFile:
			return fmt.Errorf("%s: area %q names neither a directory nor a file; give exactly one", r.Path, name)
		}
		if a.Role != "" && !oneOf(a.Role, knownRoles) {
			return fmt.Errorf("%s: area %q has role %q, not one of %s", r.Path, name, a.Role, strings.Join(knownRoles, "|"))
		}
		if a.Role == RoleGlossary {
			if hasDir {
				return fmt.Errorf("%s: area %q has role %q but names a directory; the anchor host is a single file", r.Path, name, RoleGlossary)
			}
			glossary = append(glossary, name)
		}
	}
	if len(glossary) > 1 {
		return fmt.Errorf("%s: role %q is declared on multiple areas (%s); it may mark exactly one", r.Path, RoleGlossary, strings.Join(quote(glossary), ", "))
	}
	return nil
}

// HasGlossary reports whether some area carries role: glossary. A bundle with no
// glossary area is valid — anchor resolution simply has no host to resolve
// against — so callers gate optional glossary behaviour on this.
func (r *Registry) HasGlossary() bool {
	for _, a := range r.Areas {
		if a.Role == RoleGlossary {
			return true
		}
	}
	return false
}

// Glossary resolves the single area marked role: glossary, returning its name
// and entry. It is a clear error when no area declares the role; duplicate roles
// are already rejected at Load, so this never sees more than one.
func (r *Registry) Glossary() (name string, a Area, err error) {
	for _, n := range r.sortedNames() {
		if r.Areas[n].Role == RoleGlossary {
			return n, r.Areas[n], nil
		}
	}
	return "", Area{}, fmt.Errorf("%s: no area declares role %q (the glossary/anchor host)", r.Path, RoleGlossary)
}

// GlossaryFile returns the bundle-relative file (forward slashes) of the
// glossary/anchor-host area, or "" and false when no area is marked. It is the
// single point that turns the role marker into a concrete anchor-host path.
func (r *Registry) GlossaryFile() (string, bool) {
	if r == nil {
		return "", false
	}
	_, a, err := r.Glossary()
	if err != nil {
		return "", false
	}
	return a.File, true
}

// TypeFor returns the declared type of the area that owns the given
// bundle-relative path (forward slashes), or "" when no area claims it — or the
// registry is nil (a bundle configured entirely through okf.toml). It is the
// source of the type fallback for pages that carry no frontmatter `type`: the
// area *is* the type when unauthored. A file-backed area matches only its exact
// file; a directory-backed area matches any path under it, and nested areas
// (e.g. docs/adr inside docs) are resolved by the longest matching directory
// prefix so the most specific area wins.
func (r *Registry) TypeFor(rel string) string {
	if r == nil {
		return ""
	}
	best := ""
	bestLen := -1
	for _, name := range r.sortedNames() {
		a := r.Areas[name]
		switch {
		case a.File != "":
			// An exact file is the most specific possible match; it outranks any
			// directory prefix and returns immediately.
			if a.File == rel {
				return a.Type
			}
		case a.Directory != "":
			d := strings.TrimSuffix(a.Directory, "/")
			if rel == d || strings.HasPrefix(rel, d+"/") {
				if len(d) > bestLen {
					best, bestLen = a.Type, len(d)
				}
			}
		}
	}
	return best
}

func (r *Registry) sortedNames() []string {
	names := make([]string, 0, len(r.Areas))
	for n := range r.Areas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func quote(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
