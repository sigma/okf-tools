---
name: okftool
description: Lint, format, and maintain Open Knowledge Format (OKF) bundles with the deterministic okftool CLI. Use when creating or editing .md concept pages, index.md, or log.md in an OKF bundle, or before finishing work on one — okftool owns the mechanical checks so you can focus on meaning.
---

# okftool — OKF bundle maintenance

`okftool` is a fast, deterministic CLI that enforces the mechanical rules of an
Open Knowledge Format (OKF) bundle: frontmatter/type conformance, link style,
index sync, citation shape, orphans, and broken-link reporting. It exists so you
don't re-derive these checks by hand — run the tool, then spend your judgment on
the genuinely semantic work it can't do.

## When to use it

- After you create or edit any `.md` in a bundle (concept pages, `index.md`,
  `log.md`), run `okftool lint` on the affected paths.
- Before you consider the work done, run `okftool lint --fail-on error` over the
  whole bundle and make sure `okftool index --check` passes.
- Whenever you're unsure whether a change is conformant — the tool is the
  authority, not your memory of the rules.

## The core loop

1. **Lint (machine-readable).**

   ```
   okftool lint --format json [paths...]
   ```

   Returns a stable envelope `{bundle, okf_version, summary, findings}`. Each
   finding has `rule` (e.g. `OKF102`), `severity` (`error` | `warning` | `info`),
   `path`, `line`, `message`, and `fixable`.

2. **Autofix the mechanical ones.** For findings with `"fixable": true`:

   ```
   okftool lint --fix        # apply autofixable rules in place
   okftool fmt --write       # normalize frontmatter order, timestamps,
                             # citation numbering, and link style
   okftool index --write     # regenerate index.md from the filesystem
   ```

   Re-run lint to confirm they're gone.

3. **Apply judgment to the rest.** `info` findings (`OKF201` orphans, `OKF202`
   broken links) and anything not marked `fixable` are candidates for *you* to
   resolve, not the tool. A broken link may be deliberate (not-yet-written
   knowledge); an orphan may be intentional. Decide, don't auto-apply.

## What the tool owns vs. what you own

- **The tool (deterministic):** is the frontmatter parseable, is `type` present,
  is `index.md` in sync, are citations numbered `[n] [label](target)`, are links
  in the configured style, are filenames kebab-case, are there orphans or broken
  links. Don't argue with it about these — fix them.
- **You (semantic):** contradictions between pages, claims made stale by a newer
  source, whether two near-duplicate pages should merge, whether a mentioned
  concept deserves its own page. The tool surfaces candidates; you decide.

## Rule categories

- `OKF0xx` **conformance** — always on, always `error`. A bundle is not OKF until
  these pass.
- `OKF1xx` **policy** — configurable per bundle via `okf.toml`; usually `warning`.
- `OKF2xx` **worklist** — advisory `info`; never fails a build. `OKF202`
  (broken links) is hard-capped at `info` on purpose.
- `OKF203`/`OKF204` **qmd-backed** — optional semantic checks (near-duplicate
  pages, stale index), off unless the bundle sets `qmd.enabled` and `qmd` is
  available (on `PATH`, or via `qmd.path`).

## Commands

```
okftool lint [paths...]   # run the rules; --fix, --fail-on error|warning,
                          # --select/--ignore OKFxxx, --exit-zero,
                          # --format human|json|sarif
okftool index --check     # verify index.md is in sync   (--write regenerates)
okftool fmt --check       # check formatting              (--write applies)
okftool new <path> --type <T> [--title ...]   # scaffold a conformant page
okftool graph --format json|dot               # concept link graph
```

Global flags: `--bundle <dir>` (else auto-discover), `--config <path>` (else
`okf.toml` at the bundle root), `--format human|json`.

## Tips

- Prefer `--format json` when you'll act on the output; it's the same data as the
  human view, but parseable.
- `--exit-zero` reports findings without failing the process — handy for reading
  findings mid-task without treating them as a hard stop.
- Scope a run to what you changed: `okftool lint path/to/page.md`.
