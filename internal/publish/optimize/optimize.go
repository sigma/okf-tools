// Package optimize is Stage 2 of the okfpub pipeline: a pure, deterministic
// transform from the op-DAG into a transaction-DAG.
//
// It packs each group's atomic units into the minimal set of backend API calls
// ("transactions") against a swappable backend constraint model, so the result
// is golden-file testable and dry-runs are reproducible.
//
// Scaffold stub — see sigma/ideas#172 (ratified #164).
package optimize
