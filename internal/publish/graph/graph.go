// Package graph is Stage 1 of the okfpub pipeline: concurrent generation of the
// backend-neutral operation dependency graph (op-DAG).
//
// Each source page is diffed independently against a scan of current state and
// emits semantic operations (CreateNode, SetProperties, SetContent, DeleteNode)
// with late-bound symbolic ids. Edges mean "must complete before"; the three
// edge causes are structural parent-before-child, content-refs-node, and
// content-refs-anchor.
//
// The entry point is Generate; op.go defines the op-DAG vocabulary (Op, Edge,
// Graph) and document.go the backend-neutral document tree SetContent carries.
// See sigma/ideas#172 (ratified #162).
package graph
