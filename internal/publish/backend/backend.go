// Package backend defines the generic, backend-neutral publishing interface.
//
// It is expressed as narrow role interfaces — Tokenizer, ConstraintModel/Bin,
// Executor, and Scanner — each depended on only by the stage that needs it,
// with a Backend umbrella embedding all four for wiring. Concrete backends
// (Notion first, plus an in-memory fake and a filesystem/export target) live in
// subpackages.
//
// Scaffold stub — see sigma/ideas#172 (ratified #163).
package backend
