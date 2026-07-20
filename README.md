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
okftool gaps <concept>                      # concepts near <concept> but unlinked (needs qmd)
okftool skill                               # print the bundled agent SKILL.md
```

`okftool skill` emits a Claude Code skill teaching an agent how and when to use
the tool — install it with `okftool skill > .claude/skills/okftool/SKILL.md`, so
the guidance versions with the binary. The Nix package also installs the same
file at `share/okftool/SKILL.md` for consumers that prefer to reference it from
the store.

Every command takes `--bundle <dir>` (else auto-discover), `--config <path>`
(else `okf.toml` at the bundle root), and `--format human|json` (`lint` also
`sarif`). Run it via the flake — `nix run github:sigma/okf-tools#okftool -- lint`,
or on `PATH` inside the dev shell.

### In CI (without Nix)

Downstream GitHub Actions workflows can install `okftool` without Nix using the
`setup-okftool` action from this repo. It downloads the matching released binary
(verifying its SHA-256 checksum) and puts `okftool` on `PATH`; later steps just
run `okftool`:

```yaml
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/setup-okftool@v0
      - run: okftool lint --bundle path/to/bundle
```

Pin `@v0` for the latest v0.x release, or an exact `@vX.Y.Z` for a fixed version —
the action tag resolves to the `okftool` binary released at that same tag. The
action installs on `ubuntu-*` and `macos-*` runners (linux/darwin × amd64/arm64)
and exposes the installed version as the `version` output; it takes no `version:`
input. Windows runners are not yet supported.

Implemented: conformance rules `OKF001`–`OKF004`, policy `OKF101`–`OKF107`, and
worklist `OKF201`/`OKF202`/`OKF206`, with autofix for the safe ones. Optional and
opt-in **extensions** (the `OKFEXT-*` namespace, off by default; `OKF0xx/1xx/2xx`
stays reserved for the spec): `OKFEXT-QMD-01`/`OKFEXT-QMD-02` (qmd-backed semantic
near-duplicate detection and index staleness, needs `qmd` on `PATH`) and
`OKFEXT-GLOSSARY-*` (anchor-checked single-file glossaries). Not yet built: the
Claude Code hook wiring (a consuming-bundle artifact).

Reference:

- [`docs/DESIGN.md`](docs/DESIGN.md) — architecture, CLI surface, bundle/link
  model, workflow integration, roadmap, open questions.
- [`docs/RULES.md`](docs/RULES.md) — the canonical rule catalog (IDs, categories,
  severities, autofix).
- [`docs/okf.example.toml`](docs/okf.example.toml) — annotated per-bundle config
  schema.
