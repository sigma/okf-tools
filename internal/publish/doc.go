// Package publish is the umbrella for okfpub's publisher-only subtree.
//
// The publisher mirrors an OKF bundle into a pluggable publishing backend
// (Notion first) through three decoupled stages, each speaking a
// backend-neutral vocabulary behind a generic backend interface:
//
//   - graph     — Stage 1: concurrent generation of the operation DAG.
//   - optimize  — Stage 2: deterministic op-DAG → transaction-DAG transform.
//   - transport — Stage 3: wavefront drain, resolution table, rate-limiting.
//   - backend   — the generic backend role interfaces.
//   - backend/notion — the Notion backend implementation.
//
// This subtree is imported only by cmd/okfpub. The lean-lint-binary invariant
// (cmd/okftool must never pull in internal/publish/**, and internal/publish/**
// must never import the lint-only internal/{command,rules} packages) is
// mechanically enforced by the test in boundary_test.go.
//
// See sigma/ideas#172 for the ratified spec.
package publish
