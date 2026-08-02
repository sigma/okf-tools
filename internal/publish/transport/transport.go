// Package transport is Stage 3 of the okfpub pipeline: it drains the
// transaction-DAG wavefront-by-wavefront.
//
// Transport owns the resolution table (symbolic id → real backend id), gates
// transaction readiness on Refs, and handles rate-limiting, throttle, retry,
// and physical id substitution at execution time behind Execute.
//
// Scaffold stub — see sigma/ideas#172 (ratified #162, #163).
package transport
