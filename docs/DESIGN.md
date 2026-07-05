# okf-tools — design

Status: **implemented.** `okf` (`lint`/`index`/`fmt`/`new`/`graph`) is built to
this spec — conformance `OKF001`–`OKF004`, policy `OKF101`–`OKF107`, and worklist
`OKF201`/`OKF202`/`OKF206`, with autofix, `okf.toml` config, JSON+human output,
golden-fixture tests, and flake `package`/`app`/`devShell`/`checks`. Still open:
the qmd-coupled `OKF203`/`OKF204` and the Claude Code hook wiring (M5). This
document and [RULES.md](RULES.md) remain the canonical spec.

## What this is

`okf-tools` today is a Nix flake that re-exports a dev shell bundling `qmd`
(from `firefly-engineering/toolbox` → `llm-toolchain`). This document plans its
evolution into the home of **`okf`**, a small, deterministic CLI for authoring
and maintaining [Open Knowledge Format](../../wiki/SPEC.md) bundles.

The motivating problem: the OKF wiki's `CLAUDE.md` currently describes "lint" as
a checklist an agent interprets *dynamically*. That is neither reproducible nor a
good use of the agent — most of the checklist is mechanical. `okf` moves the
mechanical rules into a tool with a stable, testable contract, and leaves the
agent only the genuinely semantic work, handed to it as a structured worklist.

## Principles

- **Deterministic core, semantic edge.** Anything decidable by parsing lives in
  the tool. Judgment stays with the agent. The boundary is drawn explicitly in
  [RULES.md](RULES.md); "Out of scope" is a deliberate list, not an omission.
- **Spec-neutral defaults, bundle-local policy.** Conformance rules come from the
  OKF spec and are universal. Everything opinionated is configurable per bundle
  via `okf.toml`, with spec-aligned defaults, so the same binary is useful on
  this wiki *and* on foreign bundles (e.g. the `knowledge-catalog` samples).
- **Machine-readable first.** Every command speaks `--format json` so it can gate
  CI, feed an agent, or drive an editor. Human output is a rendering of the same
  data.
- **Autofix where safe.** Mechanical problems with a unique correct repair are
  fixable in place; ambiguous ones are reported, never guessed.
- **Fast enough to run on every edit.** Sub-100ms on a bundle this size, so it
  can sit inside an agent's edit loop as a verification hook.

## CLI surface

One binary, `okf`, with subcommands. Global flags: `--bundle <dir>` (else
auto-discover), `--config <path>` (else `okf.toml` at bundle root),
`--format human|json`.

| Command | Purpose |
|---------|---------|
| `okf lint [paths…]` | Run the [rule catalog](RULES.md). `--fix` applies autofixable rules; `--fail-on error\|warning`; `--select/--ignore OKFxxx`; `--exit-zero`. |
| `okf index [--check\|--write]` | Verify or regenerate `index.md` from the filesystem + frontmatter descriptions (rule `OKF106`). |
| `okf fmt [--check\|--write]` | Normalize frontmatter key order, timestamp format, citation numbering, link style. The autofix companion to `lint`. |
| `okf new <path> --type <T> [--title …]` | Scaffold a conformant concept page (frontmatter + `# Citations` stub). Prevents drift at creation. |
| `okf graph [--format json\|dot]` | Emit the concept link graph. Powers orphan/backlink analysis internally; can feed a visualizer. |

`lint` is the anchor. `index` and `fmt` are the same parser pointed at repair.
`new` and `graph` are cheap wins that fall out of having the model in memory.

## Bundle model & link resolution

The trickiest correctness detail, and the reason a generic linter won't do:

- The **bundle root** is the directory the bundle is rooted at — for this wiki,
  the inner `wiki/`, *not* the Obsidian vault root that holds `.obsidian/`. The
  tool discovers it from `--bundle`, or by walking up to the nearest `okf.toml`,
  or (fallback) the nearest `index.md` declaring `okf_version`.
- **Bundle-absolute** links (`/foo.md`) resolve against the bundle root.
  **Relative** links resolve against the linked file's directory.
- A **concept cross-link** must resolve to a `.md` inside the bundle. A
  **citation** may point outside it (this wiki cites `../import/*.md`, which live
  *outside* `wiki/`) or to a URL. The linter classifies links by section and
  target shape (see RULES.md §"Link classification") and applies the right rules
  to each — e.g. `OKF202` broken-link analysis runs only on concept cross-links,
  never on citations.

This is exactly the mismatch the wiki's `CLAUDE.md` warns about (Obsidian
resolves `/` against the vault root; OKF resolves it against the bundle root),
encoded once, correctly, instead of re-explained to an agent each session.

## Output format (json)

A stable envelope so consumers don't scrape human text:

```json
{
  "bundle": "wiki",
  "okf_version": "0.1",
  "summary": { "error": 0, "warning": 2, "info": 5 },
  "findings": [
    {
      "rule": "OKF102",
      "severity": "warning",
      "path": "wiki/graphrag.md",
      "line": 24,
      "message": "bundle-absolute link '/neo4j.md'; this bundle requires relative links",
      "fixable": true
    }
  ]
}
```

SARIF output (for GitHub code scanning) is a later, additive option; JSON is the
pragmatic default for agent/editor/CI use.

## Integration with the workflow

- **Flake outputs.** Add `packages.<system>.okf` (a `buildGoModule` derivation,
  reusing the toolbox's nixpkgs so it stays a binary-cache hit) and
  `apps.<system>.okf`, and add `okf` to the existing `devShell.packages`
  alongside `llm-toolchain`. Result: `okf` on `PATH` in the wiki's `.envrc`
  shell, and `nix run github:sigma/okf-tools#okf -- lint` anywhere.
- **Claude Code hooks = in-loop verification.** A `PostToolUse` hook matching
  `Write|Edit` on `wiki/**/*.md` runs `okf lint --format json --exit-zero` and
  surfaces findings to the agent immediately after it edits; a `Stop` hook runs
  `okf lint --fail-on error && okf index --check` and refuses to finish a
  non-conformant bundle. This is deterministic, external verification — the
  agent stops self-adjudicating the mechanical rules.
- **`CLAUDE.md` shrinks.** The entire "Lint" section collapses to: *run
  `okf lint`; for `info` findings (near-duplicates, orphans, staleness) and for
  contradictions/outdated claims, apply judgment.* The rules stop living in prose
  that drifts.
- **The git wrinkle.** This wiki lives on Google Drive, not git, so
  Actions-on-commit doesn't apply *here* — the gate is the hook plus manual /
  `nix flake check` runs. But `okf-tools` itself is a jj/git repo (its own tests
  run in CI), and OKF bundles that *are* in git (knowledge-catalog) can gate PRs
  on `okf lint`.

## Implementation

- **Language: Go.** Single static binary, trivial `buildGoModule` packaging,
  fast startup for per-edit hook use, and a good fit for the surrounding stack.
- **Shape:** a `parser` package (frontmatter + markdown link extraction), a
  `bundle` package (discovery, concept model, link graph), a `rules` package
  (one file per rule, registered against IDs from RULES.md), and thin `cmd`
  wrappers. Rules are pure functions `(Bundle) -> []Finding` so they're trivially
  unit-testable against fixture bundles.
- **Testing:** golden fixture bundles under `testdata/` (one per rule, plus a
  conformant "happy" bundle), asserted via `okf lint --format json`. Wire the
  happy bundle into `nix flake check`.

## Roadmap

1. **M1 — conformance.** `okf lint` with `OKF001`–`OKF004`, JSON + human output,
   bundle discovery, exit semantics. Flake `package`/`app`. Enough to replace the
   hard conformance checks.
2. **M2 — policy + config.** `okf.toml`, `OKF101`–`OKF107`, `--select/--ignore`,
   `--fix` for the safe ones.
3. **M3 — index.** `okf index --check/--write` (`OKF106`).
4. **M4 — fmt + new.** Authoring ergonomics.
5. **M5 — worklist + graph + hooks.** `OKF201`–`OKF206`, `okf graph`, the
   Claude Code hook wiring, and the `CLAUDE.md` slimming.

## Open questions

Decisions to settle before/while implementing — flagged rather than silently
chosen:

1. **Timestamp granularity.** The wiki's pages use date-only stamps
   (`2026-07-04`); its `CLAUDE.md` example and SPEC §4.1 ("ISO 8601 datetime")
   imply full datetimes. Does `OKF104` default `timestamp_format` to `date` or
   `rfc3339` for this bundle? (Whichever we pick, `okf fmt` should normalize the
   existing pages to match.)
2. **Filename-case default severity.** `info` or `warning` out of the box?
3. **Worth the qmd coupling?** `OKF203`/`OKF204` make `okf` depend on a fresh
   `qmd` index. Keep them in `okf`, or split semantic-worklist rules into a
   separate `okf audit` subcommand that may call `qmd`, keeping `okf lint`
   dependency-free?
4. **Scope of `okf` vs. `qmd`.** Both come from `okf-tools`. Is `okf` a sibling
   binary, or should some of this live as `qmd` subcommands? (Leaning sibling:
   different concerns — authoring/conformance vs. search.)
5. **Config discovery.** Is `okf.toml` at the bundle root the right home, or a
   `[tool.okf]` block in a repo-level config? (Bundle root keeps it portable with
   the bundle.)
