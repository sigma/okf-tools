package publish

import "strings"

// This file is the home of the symbolic-id scheme: the "node:<path>" /
// "anchor:<name>" encoding that names every generation-time handle the pipeline
// threads. The scheme lives here and only here — construct a SymbolicID with
// NodeRef/AnchorRef, read one back with Rel/AnchorName. No other package mints
// or strips a "node:"/"anchor:" prefix.

// --- Symbolic identity (the generation-time handles of #162) ---------------

// SymbolicID is a generation-time handle keyed by stable identity, never a
// backend id: "node:docs/adr/0002.md", "anchor:glossary/root-kek". Ops that
// produce something mint one; ops that consume embed it as a Ref. The transport
// resolves it against a scan-seeded resolution table.
type SymbolicID string

// AnchorName is a declared named anchor — a bold-lead term hosted inside a node
// (today's anchor-ref target). The backend reports, per AtomicUnit, which
// anchors that unit hosts so the anchor map can be built.
type AnchorName string

const (
	nodeScheme   = "node:"
	anchorScheme = "anchor:"
	// unclaimedScheme names a backend object the pipeline owns but that corresponds
	// to no source node — see UnclaimedRef.
	unclaimedScheme = "unclaimed:"
)

// NodeRef mints the symbolic id of a node reference from its bundle-relative
// path.
func NodeRef(rel string) SymbolicID { return SymbolicID(nodeScheme + rel) }

// AnchorRef mints the symbolic id of an anchor reference for a declared anchor.
func AnchorRef(name AnchorName) SymbolicID {
	return SymbolicID(anchorScheme + string(name))
}

// Rel recovers the bundle-relative path from a node symbolic id. It panics if
// id is not a node ref: every caller reaches Rel on an id it produced as a node
// (a scanned node, an op target, a group key), so a wrong kind is a programmer
// error, not a data condition — surfacing it loudly beats silently returning an
// anchor id's raw string.
func (id SymbolicID) Rel() string {
	rel, ok := strings.CutPrefix(string(id), nodeScheme)
	if !ok {
		panic("publish: Rel on non-node symbolic id: " + string(id))
	}
	return rel
}

// AnchorName reports whether id is an anchor reference ("anchor:<name>") and, if
// so, the AnchorName it targets. It is the reader counterpart to AnchorRef, and
// the one dispatch the scheme needs: ok=false means "this is a node ref." Every
// stage matches an anchor Ref to a producer's Anchors through this method the
// same way (Stage 1 wiring op-DAG edges, Stage 2 wiring transaction-DAG edges).
func (id SymbolicID) AnchorName() (AnchorName, bool) {
	if rest, ok := strings.CutPrefix(string(id), anchorScheme); ok {
		return AnchorName(rest), true
	}
	return "", false
}

// UnclaimedRef mints the symbolic id of an UNCLAIMED backend object: something the
// pipeline created but that names no source node, keyed by its backend id because
// that is the only identity it has (sigma/okf-tools#135).
//
// A row acquires this state exactly one way: a run created it and died before
// recording which node it is. Since a scan keys a row to its node by the path the
// row carries, such a row is unmatchable — the next run neither recognises it as
// its node's row (creating the node a second time) nor as a leak. Naming it in the
// symbolic scheme is what lets the ordinary vanished-node reconciliation see it:
// no source doc can ever mint this id, so it is always "scanned but sourceless",
// which is precisely the condition that archives a node.
//
// It is deliberately NOT a node ref: it has no path, and Rel panics on it rather
// than inventing one.
func UnclaimedRef(id BackendID) SymbolicID { return SymbolicID(unclaimedScheme + string(id)) }

// Unclaimed reports whether id names an unclaimed backend object and, if so, the
// backend id it stands for. It is the reader counterpart to UnclaimedRef, and the
// guard every stage that would otherwise read a node's path must consult first.
func (id SymbolicID) Unclaimed() (BackendID, bool) {
	rest, ok := strings.CutPrefix(string(id), unclaimedScheme)
	return BackendID(rest), ok
}
