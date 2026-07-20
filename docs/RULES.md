# okftool lint — rule catalog

Canonical reference for the rules enforced by `okftool lint`. Each rule has a
stable ID, a category, a default severity, and a note on whether it is
autofixable. This document is the source of truth; the implementation must not
add or renumber rules without updating it.

## Design premises

1. **Mechanical only.** Every rule here is decidable by parsing the bundle. Rules
   that require judgment over *meaning* (contradictions between pages, claims
   made stale by a newer source) are deliberately **out of scope** — see
   [Out of scope](#out-of-scope). The linter's job is to be perfectly
   reproducible and to hand the semantic layer (an agent) a stable worklist.

2. **Categories and postures.**
   - **Conformance (`OKF0xx`)** — straight from the
     [OKF spec](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
     §9. Always on. Severity fixed at **error**. These define whether a bundle
     *is* OKF; they are not configurable.
   - **Policy (`OKF1xx`)** — bundle-local conventions and SPEC "SHOULD"s.
     Configurable via `okf.toml`. Defaults are **spec-aligned**, not tied to any
     one bundle — a bundle opts into stricter policy (see
     [okf.example.toml](okf.example.toml)).
   - **Worklist (`OKF2xx`)** — advisory. Default severity **info**. These are
     candidates the agent resolves.
   - **Extensions (`OKFEXT-<EXT>-<NN>`)** — built-in but **not part of the OKF
     spec**, one bucket per extension. Off unless the bundle opts in, configurable
     like Policy. The `OKF0xx`/`OKF1xx`/`OKF2xx` space is **reserved for the spec**
     — extensions never use it. See [Extensions](#extensions-okfext-ext-nn).

   **Defaults are spec-aligned and advisory, but not a ceiling.** Worklist rules
   default to `info` and a bundle that leaves them alone never fails a build on
   them — but a bundle **may escalate any rule** (worklist included) to
   `warning`/`error` via the `[rules]` map, consciously accepting the deviation
   from SPEC §5.3. Only conformance rules are immovable (fixed at `error`).

3. **Broken links are not errors — by default.** SPEC §5.3/§9 explicitly bless
   links to not-yet-written concepts, so `OKF202` **defaults to `info`**. This is
   the single most important place a generic markdown linter gets OKF wrong.
   A bundle that wants a hard CI gate on dead links may nonetheless promote it
   (`[rules]."OKF202" = "error"`, or `links.check_broken = "error"`) — an
   owner-ratified deviation from the spec default, not the default itself.

4. **Link classification.** The linter distinguishes three kinds of links, and
   most rules scope to the first:
   - **Concept cross-links** — body links whose target resolves to a `.md` file
     *inside the bundle*. Subject to `OKF102`, `OKF202`, orphan analysis.
   - **Citations** — links under a `# Citations` heading. May point outside the
     bundle (e.g. `../import/foo.md`) or to external URLs. Subject to `OKF105`,
     optionally `OKF206`.
   - **External references** — absolute URLs anywhere. Not resolved on disk.

## Severity & exit semantics

| Severity | Meaning | Affects exit code |
|----------|---------|-------------------|
| `error`  | Bundle is non-conformant, or a policy rule set to error | yes (non-zero) |
| `warning`| Policy violation | yes if `--fail-on warning` (default: error only) |
| `info`   | Worklist item for the agent/human | never |

`okftool lint` exits non-zero when any finding is at or above the `--fail-on`
threshold (default `error`). `--exit-zero` reports without failing (useful when
piping JSON to an agent). Rules can be turned off or re-leveled per bundle in
`okf.toml`, **except** conformance rules, whose severity is fixed.

---

## Category A — Conformance (`OKF0xx`)

Always on. Severity fixed at **error**. Source: SPEC §9.

### `OKF001` frontmatter-parseable
Every non-reserved `.md` file contains a YAML frontmatter block delimited by
`---` on its own line at the top and a closing `---`, and it parses as YAML.
*SPEC §4, §9.1. Autofix: no (structural).*

### `OKF002` type-required
The frontmatter contains a `type` key with a non-empty string value.
*SPEC §4.1, §9.2. Autofix: no (needs a human/agent to choose the type).*

### `OKF003` index-structure
`index.md` files carry **no frontmatter**, except the bundle-root `index.md`,
which MAY carry exactly one key: `okf_version`. The body is one or more heading-
grouped bullet lists of links.
*SPEC §6, §9.3, §11. Autofix: partial (via `okftool index --write`).*

### `OKF004` log-structure
`log.md` files use `## YYYY-MM-DD` ISO date headings, ordered newest-first, with
prose bullet entries.
*SPEC §7, §9.3. Autofix: no.*

---

## Category B — Policy (`OKF1xx`)

Configurable. Defaults below are **spec-aligned**; a bundle tightens them in
`okf.toml`. The "Strict example" column shows values a stricter bundle might set.

| Rule | Default | Strict example | Autofix |
|------|---------|----------------|---------|
| `OKF101` no-wikilinks | warning | warning | partial |
| `OKF102` link-style | **off** (SPEC recommends absolute) | relative → on | yes |
| `OKF103` filename-case | info (kebab) | warning (kebab) | no |
| `OKF104` timestamp-format | warning (if present) | warning, datetime | partial |
| `OKF105` citations-format | warning | warning | partial |
| `OKF106` index-sync | warning | warning | yes |
| `OKF107` description-present | info | warning | no |

### `OKF101` no-wikilinks
No Obsidian `[[wiki-link]]` syntax. It is not a standard markdown link and will
not resolve for non-Obsidian OKF consumers. Autofix can rewrite `[[foo]]` →
`[foo](foo.md)` only when the target is unambiguous; otherwise it flags for
manual repair. *Config: `links.allow_wikilinks`.*

### `OKF102` link-style
Enforce the configured concept-cross-link style. **Off by default** because SPEC
§5.1 *recommends* bundle-absolute (`/path.md`) links. A bundle whose consumer
resolves `/` against a different root than the bundle root (for example an editor
that resolves against its own vault root) sets `links.style = "relative"`, which
then flags any `/`-absolute cross-link and any relative link that escapes the
bundle. Autofix rewrites between styles by recomputing the path against the
bundle root. *Config: `links.style` = `relative` | `absolute` | `any`.*

### `OKF103` filename-case
Concept filenames match the configured case convention (default `kebab`:
lowercase, hyphen-separated). Guards portability across case-insensitive
filesystems and keeps concept IDs predictable. *Config: `filenames.case`.*

### `OKF104` timestamp-format
If `timestamp` is present it must match the configured format. Default
`rfc3339` (full ISO 8601 datetime, per SPEC §4.1 "ISO 8601 datetime"); a bundle
may relax to `date` to allow `YYYY-MM-DD`. Presence itself is governed by
`frontmatter.require_timestamp` (default off). Autofix normalizes a parseable
value to the canonical format. *Config: `frontmatter.timestamp_format`,
`frontmatter.require_timestamp`. A bundle whose pages use date-only stamps sets
`timestamp_format = "date"` (see [Decisions](DESIGN.md#decisions)).*

### `OKF105` citations-format
When the body makes sourced claims, sources appear under a `# Citations`
heading, numbered `[n] [label](target)`. SPEC §8 SHOULD. Setting
`citations.style = "footnote"` switches the expected form to the markdown
footnote `[^n]: [label](target)` (with `[^n]` inline markers), which renders as
real footnotes in Obsidian/GitHub; the default `numbered` matches SPEC §8.
Autofix renumbers a malformed sequence but will not invent missing citations.
*Config: `citations.heading`, `citations.style`, `citations.require_when_cited`.*

### `OKF106` index-sync
Each `index.md` enumerates every concept in its scope, contains no entries for
files that don't exist, and (when `index.descriptions_from_frontmatter`) each
entry's description matches the target's frontmatter `description`. Fully
autofixable — `okftool index --write` regenerates it. This rule turns a recurring,
easily-forgotten manual step into a checkable invariant. *Config:
`index.check_sync`, `index.descriptions_from_frontmatter`.*

### `OKF107` description-present
Concept frontmatter has a non-empty one-line `description`. SPEC §4.1
recommended; used by index generators, search snippets, previews. *Config:
`frontmatter.require_description`.*

---

## Category C — Worklist (`OKF2xx`)

Advisory. Default severity **info** — a bundle that leaves them alone is never
failed on them, though it may escalate any of them via `[rules]`. These are the
mechanical half of the semantic checks: the tool finds candidates; the agent
decides.

### `OKF201` orphan-pages
A concept with no inbound cross-links from any other concept (index/log
excluded). Not necessarily wrong — may be a new or intentionally standalone
page — but worth a look for graph connectivity.

### `OKF202` broken-links
A concept cross-link whose target does not resolve inside the bundle. **Defaults
to `info`** (SPEC §5.3: may be not-yet-written knowledge), but promotable — a
bundle may set `links.check_broken` or `[rules]."OKF202"` up to `error` to make
dead links a hard CI gate. *Config: `links.check_broken` = `off` | `info` |
`warning` | `error`.*

### `OKF206` citation-target-exists *(optional, off by default)*
A `# Citations` link with an on-disk path (e.g. `../import/x.md`) points to a
file that exists. Catches typo'd source filenames. Distinct from `OKF202`
because citations legitimately point *outside* the bundle. *Config:
`citations.check_targets`.*

---

## Extensions (`OKFEXT-<EXT>-<NN>`)

**Built-in but non-spec.** The `OKF0xx`/`OKF1xx`/`OKF2xx` namespaces are reserved
for the official OKF spec — conformance, policy, and worklist checks straight
from SPEC §9. Anything **not in the spec** — a *built-in extension* — lives under
`OKFEXT-<EXT>-<NN>`, **one bucket per extension**. The `OKFEXT` prefix advertises
"not official spec" in every surface (lint output, `[rules]`, `--select`), and
hyphens are valid TOML bare keys so overrides read `[rules].OKFEXT-GLOSSARY-02 =
"error"` unquoted.

Extensions are **off unless the bundle opts in**, configurable like Policy
(own `Default`, promotable via `[rules]`), and each is scoped to what it declares.

### `OKFEXT-QMD-*` — qmd-backed *(opt-in)*

Advisory semantic checks that need a fresh
[`qmd`](https://github.com/firefly-engineering/toolbox) index — the one place
`okftool` reaches for an external tool. **Off unless the bundle sets
`qmd.enabled = true`**; the rest of `okftool lint` stays dependency-free. Default
severity **info**.

#### `OKFEXT-QMD-01` near-duplicate
Pairs of concepts with high semantic overlap, from a `qmd` similarity query above
a threshold. Detection is reproducible *relative to a given qmd index snapshot*;
the merge/keep decision is the agent's. Requires a fresh index (see
`OKFEXT-QMD-02`). *Config: `qmd.near_duplicates`, `qmd.near_duplicate_threshold`.*

#### `OKFEXT-QMD-02` qmd-staleness
The `qmd` index is out of date: some concept's current content hash is absent
from the index. Signals that `qmd update && qmd embed` is needed before trusting
`OKFEXT-QMD-01` or any semantic recall. *Config: `qmd.staleness`.*

### `OKFEXT-GLOSSARY-*` — single-file glossaries *(opt-in)*

Treats one (or a few) declared Markdown files as an anchor-addressable
**single-file glossary**, and lints cross-references into it. This lets a bundle
keep all its vocabulary in one file that loads in a single read while still
getting mechanical link/anchor checking — the thing OKF's file-per-concept model
gives you for free but a single-file glossary does not. Framing: **a glossary is
to terms what `index.md` is to pages** — a structured, single-file aggregation,
so declared glossary files are exempt from the concept conformance rules
(`OKF001`/`OKF002`/`OKF103`) just as `index.md`/`log.md` are.

These rules implement the domain-modeling
[`CONTEXT-FORMAT`](https://github.com/mattpocock/skills/blob/main/skills/engineering/domain-modeling/CONTEXT-FORMAT.md),
**not the OKF SPEC**. All are gated on `glossary.enabled` and scoped to the
`glossary.files` globs; a bundle that doesn't opt in sees zero new diagnostics.

**Term & slug.** A *term* is a `CONTEXT-FORMAT` entry — a paragraph or list item
leading with bold text and a colon (`**Side-door credential**: An undeclared …`).
Its **anchor** is the GitHub-style slug of the bold term text
(`**Root KEK**` → `root-kek`, `**Foreign-rooted leaf**` → `foreign-rooted-leaf`).
The slug algorithm is **fixed** (lowercase; drop all but `[a-z0-9]`, space and
hyphen; spaces → `-`; collapse repeated hyphens; trim) — deliberately *not*
configurable, so it can't drift from a consumer that resolves the same anchors
(e.g. a Notion sync).

| Rule | Default | What it checks |
|------|---------|----------------|
| `OKFEXT-GLOSSARY-01` glossary-structure | warning | declared glossary files are term-structured (`CONTEXT-FORMAT`) |
| `OKFEXT-GLOSSARY-02` glossary-anchor-resolves | warning | a concept `#fragment` into a glossary resolves to a term/heading slug |
| `OKFEXT-GLOSSARY-03` glossary-term-unique | warning | term slugs are unique and don't collide with heading slugs |
| `OKFEXT-GLOSSARY-04` glossary-orphan-term | info | a defined term no concept references *(stretch)* |

#### `OKFEXT-GLOSSARY-01` glossary-structure
The glossary analogue of `OKF003`/`OKF004`: a declared glossary file's body is
bold-lead term entries and optional grouping headings. Prose intros are tolerated,
but every list item must parse as a well-formed `**Term**: definition`, and a
glossary that extracts zero terms is flagged.

#### `OKFEXT-GLOSSARY-02` glossary-anchor-resolves
**The load-bearing rule.** Every concept link into a declared glossary file that
carries a `#fragment` must resolve to a defined anchor (term slug or heading slug)
in that file — "a reference to an undefined concept," caught at lint time. In-page
`#fragments` are also checked when the source is itself a glossary (a
self-reference). General bundle-wide heading-anchor resolution is **out of scope**:
this extension only resolves anchors *into declared glossary files*. Promotable to
`error` for a strict bundle (`[rules].OKFEXT-GLOSSARY-02 = "error"`).

#### `OKFEXT-GLOSSARY-03` glossary-term-unique
Within a glossary file, term slugs are unique and do not collide with heading
slugs — otherwise a `#anchor` is ambiguous and unstable across renders
(GitHub/Notion). Flags any slug produced by more than one term, or by a term and a
heading, at the later occurrence's line.

#### `OKFEXT-GLOSSARY-04` glossary-orphan-term *(stretch)*
A defined glossary term that no concept references by anchor — the
term-granularity analogue of `OKF201` orphan-pages. Advisory (`info`): a
freshly-authored term may simply not be linked yet.

---

## Out of scope

The linter deliberately does **not** attempt these — they need semantic
judgment, not parsing. They remain the agent's job, fed by the worklist above:

- **Contradictions between pages** — two concepts asserting incompatible facts.
- **Claims outdated by a newer source** — requires understanding source recency
  and claim equivalence.
- **"Concept mentioned but not written"** beyond trivial proper-noun heuristics —
  deciding a phrase *deserves* its own page is editorial.
- **"Each change has a log entry"** — needs a VCS diff, not a snapshot; a future
  `okftool lint --since <rev>` mode could cover it where the bundle is in git/jj.

Keeping these out is a feature: it is precisely the line between what should be
reproducible tooling and what should stay agent judgment.
