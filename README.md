# okf-tools

Tooling for authoring and maintaining [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
(OKF) bundles.

## Today

A Nix flake exposing a dev shell that bundles [`qmd`](https://github.com/firefly-engineering/toolbox)
(local hybrid search over markdown), consumed by downstream projects via:

```
use flake github:sigma/okf-tools
```

## `okftool`

A small, deterministic Go CLI for OKF bundles — the mechanical half of what an
agent currently does "by interpretation." It moves reproducible checks
(frontmatter/type conformance, link style, index sync, citation shape, orphans,
broken-link reporting) into a testable tool, and hands the genuinely semantic
work (contradictions, near-duplicates, staleness) back to the agent as a
structured worklist.

```
okftool lint [paths…]   # run the rule catalog; --fix, --fail-on, --select/--ignore, --exit-zero
okftool index --check   # verify index.md is in sync   (--write regenerates it)
okftool fmt   --check   # normalize frontmatter/timestamps/citations/link-style (--write applies)
okftool new <path> --type <T> [--title …]   # scaffold a conformant concept page
okftool graph --format json|dot             # emit the concept link graph
okftool skill                               # print the bundled agent SKILL.md
```

`okftool skill` emits a Claude Code skill teaching an agent how and when to use
the tool — install it with `okftool skill > .claude/skills/okftool/SKILL.md`, so
the guidance versions with the binary.

Every command takes `--bundle <dir>` (else auto-discover), `--config <path>`
(else `okf.toml` at the bundle root), and `--format human|json`. Run it via the
flake — `nix run github:sigma/okf-tools#okftool -- lint`, or on `PATH` inside the
dev shell.

Implemented: conformance rules `OKF001`–`OKF004`, policy `OKF101`–`OKF107`, and
worklist `OKF201`/`OKF202`/`OKF206`, with autofix for the safe ones. Optional and
opt-in: the qmd-backed rules `OKF203`/`OKF204` (semantic near-duplicate
detection), off unless a bundle sets `qmd.enabled`. Not yet built: the qmd rules
themselves and the Claude Code hook wiring.

Reference:

- [`docs/DESIGN.md`](docs/DESIGN.md) — architecture, CLI surface, bundle/link
  model, workflow integration, roadmap, open questions.
- [`docs/RULES.md`](docs/RULES.md) — the canonical rule catalog (IDs, categories,
  severities, autofix).
- [`docs/okf.example.toml`](docs/okf.example.toml) — annotated per-bundle config
  schema.
