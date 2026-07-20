# okf-tools — design

Status: **implemented.** `okftool`
(`lint`/`index`/`fmt`/`new`/`graph`/`gaps`/`skill`) is built to this spec —
conformance `OKF001`–`OKF004`, policy `OKF101`–`OKF107`, worklist
`OKF201`/`OKF202`/`OKF206`, the opt-in extensions `OKFEXT-QMD-*` (qmd-backed) and
`OKFEXT-GLOSSARY-*` (single-file glossaries), and
seed-driven content-gap analysis (`gaps`) — with autofix, `okf.toml` config,
human/JSON/SARIF output, a per-package test suite, CI, and flake
`package`/`app`/`devShell`/`checks`. Still open: Claude Code hook wiring (M5). This document and [RULES.md](RULES.md) remain
the canonical spec.

## What this is

`okf-tools` provides **`okftool`**, a small, deterministic CLI for authoring and
maintaining [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
(OKF) bundles, alongside a Nix dev shell that bundles `qmd` (from
`firefly-engineering/toolbox` → `llm-toolchain`).

The motivating problem: OKF bundle maintenance ("lint") is often handed to an
agent as a prose checklist it interprets *dynamically*. That is neither
reproducible nor a good use of the agent — most of the checklist is mechanical.
`okftool` moves the mechanical rules into a tool with a stable, testable
contract, and leaves the agent only the genuinely semantic work, handed to it as
a structured worklist.

## Principles

- **Deterministic core, semantic edge.** Anything decidable by parsing lives in
  the tool. Judgment stays with the agent. The boundary is drawn explicitly in
  [RULES.md](RULES.md); "Out of scope" is a deliberate list, not an omission.
- **Spec-neutral defaults, bundle-local policy.** Conformance rules come from the
  OKF spec and are universal. Everything opinionated is configurable per bundle
  via `okf.toml`, with spec-aligned defaults, so the same binary is useful on any
  OKF bundle (e.g. the `knowledge-catalog` samples).
- **Machine-readable first.** Every command speaks `--format json` so it can gate
  CI, feed an agent, or drive an editor. Human output is a rendering of the same
  data.
- **Autofix where safe.** Mechanical problems with a unique correct repair are
  fixable in place; ambiguous ones are reported, never guessed.
- **Fast enough to run on every edit.** Sub-100ms on a typical bundle, so it can
  sit inside an agent's edit loop as a verification hook.
- **No mandatory external tools.** Core linting is self-contained. Anything that
  needs a heavier dependency (e.g. `qmd` for semantic checks) is opt-in per
  bundle, never required.

## CLI surface

One binary, `okftool`, with subcommands. Global flags (GNU-style, via `pflag`):
`--bundle <dir>` (else auto-discover), `--config <path>` (else `okf.toml` at
bundle root), `--format human|json`.

| Command | Purpose |
|---------|---------|
| `okftool lint [paths…]` | Run the [rule catalog](RULES.md). `--fix` applies autofixable rules; `--fail-on error\|warning`; `--select/--ignore OKFxxx`; `--exit-zero`. |
| `okftool index [--check\|--write]` | Verify or regenerate `index.md` from the filesystem + frontmatter descriptions (rule `OKF106`). |
| `okftool fmt [--check\|--write]` | Normalize frontmatter key order, timestamp format, citation numbering, link style. The autofix companion to `lint`. |
| `okftool new <path> --type <T> [--title …]` | Scaffold a conformant concept page (frontmatter + `# Citations` stub). Prevents drift at creation. |
| `okftool graph [--format json\|dot]` | Emit the concept link graph. Powers orphan/backlink analysis internally; can feed a visualizer. |
| `okftool skill` | Print the bundled agent skill (a Claude Code `SKILL.md`) to stdout, e.g. `okftool skill > .claude/skills/okftool/SKILL.md`. |
| `okftool gaps <concept>` | Seed-driven content-gap analysis: concepts semantically near `<concept>` but not linked to it (`--depth`, `--top`, `--min-sim`, `--exclude-types`). Needs qmd. Spec: [gaps.md](gaps.md). |

`lint` is the anchor. `index` and `fmt` are the same parser pointed at repair.
`new` and `graph` are cheap wins that fall out of having the model in memory.
`skill` ships the tool's own usage guidance so an agent can install it.

## Bundle model & link resolution

The trickiest correctness detail, and the reason a generic linter won't do:

- The **bundle root** is the directory the bundle is rooted at, which may be
  nested inside a larger vault or repository. The tool discovers it from
  `--bundle`, or by walking up to the nearest `okf.toml`, or (fallback) the
  nearest `index.md` declaring `okf_version`.
- **Bundle-absolute** links (`/foo.md`) resolve against the bundle root.
  **Relative** links resolve against the linked file's directory.
- A **concept cross-link** must resolve to a `.md` inside the bundle. A
  **citation** may point outside it (e.g. `../import/foo.md`, living outside the
  bundle) or to a URL. The linter classifies links by section and target shape
  (see RULES.md §"Link classification") and applies the right rules to each —
  e.g. `OKF202` broken-link analysis runs only on concept cross-links, never on
  citations.

A subtle consequence: a consumer that resolves `/` against a *different* root
than the bundle root (for example an editor that resolves against its own vault
root) will mis-resolve bundle-absolute links. Such a bundle sets
`links.style = "relative"` so its links resolve correctly everywhere. The tool
encodes the correct OKF resolution once, instead of re-explaining it to an agent
each session.

## Output format (json)

A stable envelope so consumers don't scrape human text:

```json
{
  "bundle": "docs",
  "okf_version": "0.1",
  "summary": { "error": 0, "warning": 2, "info": 5 },
  "findings": [
    {
      "rule": "OKF102",
      "severity": "warning",
      "path": "graphrag.md",
      "line": 24,
      "message": "bundle-absolute link '/neo4j.md'; this bundle requires relative links",
      "fixable": true
    }
  ]
}
```

`--format sarif` emits SARIF 2.1.0 so CI can upload findings to GitHub code
scanning; JSON is the pragmatic default for agent/editor/CI use.

## Integration with the workflow

- **Flake outputs.** `packages.<system>.okftool` (a `buildGoModule` derivation,
  reusing the toolbox's nixpkgs so it stays a binary-cache hit),
  `apps.<system>.okftool`, and `okftool` added to `devShell.packages` alongside
  `llm-toolchain`. Result: `okftool` on `PATH` in a consuming project's shell,
  and `nix run github:sigma/okf-tools#okftool -- lint` anywhere.
- **Claude Code hooks = in-loop verification (M5).** A `PostToolUse` hook
  matching `Write|Edit` on a bundle's `*.md` runs
  `okftool lint --format json --exit-zero` and surfaces findings to the agent
  immediately after it edits; a `Stop` hook runs
  `okftool lint --fail-on error && okftool index --check` and refuses to finish a
  non-conformant bundle. This is deterministic, external verification — the agent
  stops self-adjudicating the mechanical rules.
- **Agent instructions shrink.** A consuming bundle's "how to lint" guidance
  collapses to: *run `okftool lint`; for `info` findings (near-duplicates,
  orphans, staleness) and for contradictions/outdated claims, apply judgment.*
  The rules stop living in prose that drifts. `okftool` ships that guidance as a
  bundled skill — `okftool skill` prints a `SKILL.md` a project can install, so
  the usage instructions version with the binary instead of being copied by hand.
- **CI.** `okf-tools` runs `nix flake check` (build + full test suite) and a
  gofmt/vet/test gate in GitHub Actions; OKF bundles kept in git can gate PRs on
  `okftool lint` (uploading `--format sarif` to code scanning if they like).

## Implementation

- **Language: Go.** Single static binary, trivial `buildGoModule` packaging,
  fast startup for per-edit hook use, and a good fit for the surrounding stack.
- **Shape:** a `parser` package (frontmatter split + markdown parsed via the
  goldmark AST), a `bundle` package (discovery, concept model, link
  classification/resolution, graph, index model), a `config` package (`okf.toml`),
  a `rules` package (grouped by category, each rule registered against an ID from
  RULES.md), and thin `command` wrappers under `cmd/okftool`. Rules are pure
  functions `(Context) -> []Finding` so they're trivially unit-testable against
  fixture bundles. CLI argument parsing uses `spf13/pflag`.
- **Testing:** golden fixture bundles under `testdata/` (one per rule, plus a
  conformant "happy" bundle), asserted by running the rules over each bundle. The
  suite is wired into `nix flake check` via the package's `doCheck`.

## Roadmap

1. **M1 — conformance.** ✅ `okftool lint` with `OKF001`–`OKF004`, JSON + human
   output, bundle discovery, exit semantics. Flake `package`/`app`.
2. **M2 — policy + config.** ✅ `okf.toml`, `OKF101`–`OKF107`,
   `--select/--ignore`, `--fix` for the safe ones.
3. **M3 — index.** ✅ `okftool index --check/--write` (`OKF106`).
4. **M4 — fmt + new.** ✅ Authoring ergonomics.
5. **M5 — worklist + graph + hooks.** ✅ `okftool graph`, worklist
   `OKF201`/`OKF202`/`OKF206`, and the opt-in `OKFEXT-QMD-*` qmd-backed extension.
   Still open: the Claude Code hook wiring (a consuming-bundle artifact).
6. **M6 — content-gap analysis.** ✅ `okftool gaps <concept>`: seed-driven
   detection of near-but-unlinked concepts (direct gaps + neighborhood holes),
   reusing the qmd integration and link graph. Spec in [gaps.md](gaps.md).

## Decisions

Settled during design/implementation — recorded rather than left implicit:

1. **Timestamp granularity.** `OKF104` defaults `timestamp_format` to `rfc3339`
   (full ISO 8601 datetime, per SPEC §4.1); a bundle relaxes it to `date` for
   `YYYY-MM-DD`. `okftool fmt` normalizes existing pages to the configured form.
2. **Filename-case default severity.** `info` out of the box; a bundle raises it
   to `warning` via config.
3. **qmd coupling.** `OKFEXT-QMD-01`/`OKFEXT-QMD-02` are a separate, **optional**
   qmd-backed extension, **off** unless a bundle sets `qmd.enabled = true`. `lint`
   runs the qmd analysis (via the `internal/qmd` package, which shells out to
   `qmd`) only when enabled and hands the result to the pure rules through
   `Context.QMD`; the core stays dependency-free. Needs the `qmd` binary (on
   `PATH`, or set `qmd.path`) and a fresh index.
4. **Scope of `okftool` vs. `qmd`.** Sibling binaries from `okf-tools` with
   different concerns — authoring/conformance vs. search — not subcommands of one
   another.
5. **Config discovery.** `okf.toml` at the bundle root, so config travels with
   the bundle and stays portable.
6. **Extension namespace (`OKFEXT-<EXT>-<NN>`).** Checks that are *not* in the OKF
   spec live in their own ID namespace, **one bucket per extension**
   (`OKFEXT-QMD-*`, `OKFEXT-GLOSSARY-*`), leaving `OKF0xx`/`OKF1xx`/`OKF2xx`
   reserved for the spec. The prefix advertises "non-spec" in every surface, and
   an extension is off by default, configurable like Policy, and scoped to what it
   declares. Any rule (spec or extension) is promotable to a hard failure via
   `[rules]`; only conformance stays fixed at `error`.

## Single-file glossaries (`OKFEXT-GLOSSARY-*`)

OKF models a concept as *its own file* with `type` frontmatter; a glossary breaks
that — it is one file defining ~40 terms, referenced by anchor
(`[root-kek](/CONTEXT.md#root-kek)`). Some consumers require *all* vocabulary in a
single file that loads in one read (e.g. a GitHub→Notion doc sync), which the
file-per-concept model can't give them. The glossary extension adds exactly the
two things OKF otherwise lacks here: it **preserves a link's `#fragment`** (base
resolution discards it) and introduces an **in-page, anchor-addressable term**.

Framing: **a glossary is to terms what `index.md` is to pages** — a structured,
single-file aggregation. It slots in beside `OKF003`/`OKF004` as a third
structured page kind, so a declared glossary file carries no frontmatter and is
exempt from the concept conformance rules; identity comes from `[glossary] files`
config alone, keeping the file byte-for-byte `CONTEXT-FORMAT` compliant.

A term's anchor is the **fixed** GitHub-style slug of its bold text — not
configurable, so it can't drift from a consumer resolving the same anchors. The
**scope boundary** is deliberate: anchors are resolved only *into declared
glossary files*; general bundle-wide heading-anchor resolution stays out of scope,
which keeps the surface minimal and the opt-in boundary crisp.
