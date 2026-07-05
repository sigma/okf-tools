# okftool gaps — content-gap analysis (spec)

Status: **implemented** — `okftool gaps <concept>`. Prototype validated against a
real bundle (see [Evidence](#evidence)); shipped as the seed-driven command
described below.

`okftool gaps <concept>` answers a question you have whenever you are working on
a page: **which concepts is this one semantically near but *not* linked to?** It
surfaces candidate cross-links / bridges; it does not write them — detection is
deterministic, bridging is the agent's (or human's) job, the same
*tool-finds-candidates-you-decide* contract as the worklist rules in
[RULES.md](RULES.md).

## The idea: two graphs, and their discrepancy

A bundle carries two overlapping graphs:

- the **authored graph** — the markdown cross-links (`okftool graph`);
- the **semantic graph** — proximity in embedding space, observable through the
  bundle's qmd index (the same signal `OKF203` near-duplicate uses).

A **content gap** is a *discrepancy* between them: a pair of concepts that are
semantically adjacent yet unlinked. That discrepancy is where a missing
cross-reference — or a genuinely new connection — lives. (This is the graph-native
"structural hole" idea, rebuilt on the bundle's own link graph instead of an
inferred word co-occurrence network.)

## Why seed-driven, not a global scan

A whole-bundle scan is the wrong shape:

- **Cost** — it needs one qmd query per page to build the similarity graph:
  **O(N)**, linear in bundle size.
- **Signal** — most page pairs are *legitimately* far apart and unlinked
  (unrelated topics). Reporting those is noise, not gaps.

Seeding from a concept fixes both. You only ever examine that concept's genuine
neighborhood, and cost stops depending on N:

| Mode | qmd queries | notes |
|------|-------------|-------|
| global scan (rejected) | ~N | linear; mostly non-gaps |
| **`gaps C` — direct** | **1** | C's near-but-unlinked neighbors |
| `gaps C` — neighborhood | 1 + k | + open triangles among C's k neighbors |

## Algorithm

Inputs: a seed concept `C`; the bundle's authored link graph; a qmd `concepts`
collection.

1. **Neighbors of C** — one qmd vector search using C's `title` + `description`
   as the query. qmd returns chunk-level hits; aggregate to page level (score =
   **max** over a page's chunks). Keep the top `k` above a similarity floor `τ`.
2. **Direct gaps** — `neighbors(C) \ links(C)`, i.e. semantically near but not
   already cross-linked. Ranked by similarity. This is the primary output.
3. **Neighborhood holes** *(optional, `--depth neighborhood`)* — among C's
   neighbors, pairs `(A,B)` that are mutually near but unlinked: open triangles /
   local structural holes. Costs `+k` qmd queries (one per neighbor), still
   independent of N.

**Filters (required — see [Evidence](#evidence)):**

- **Exclude reserved files** (`index.md`, `log.md`). The generated `index.md`
  contains every page's description, so it is semantically near *everything* and
  is not a concept node — it pollutes results.
- **Optionally down-weight hubs** — high-degree or `Person`-type nodes are near
  much of the bundle by nature; the graph JSON carries `type`, so
  `--include-types`/`--exclude-types` can scope this. Direct gaps are clean
  without this; the neighborhood pass benefits from it.

## CLI surface

```
okftool gaps <concept> [flags]
  --depth direct|neighborhood   default: direct
  --top <k>                     neighbors to consider (default 10)
  --min-sim <τ>                 similarity floor (default ~0.4; see calibration)
  --exclude-types <T,...>       skip node types (e.g. Person) in the hole pass
  --format human|json           default: human
```

Global flags (`--bundle`, `--config`) as elsewhere. Like `OKF203`/`OKF204`, this
depends on **qmd** (a fresh index + the `qmd` binary on `PATH`, honoring
`qmd.path`); with qmd absent or the master `qmd.enabled` off, `gaps` errors
clearly rather than silently returning nothing.

## Output (json)

```json
{
  "seed": "content-gap-analysis.md",
  "neighbors": [
    {"page": "graphrag.md", "sim": 0.53, "linked": false},
    {"page": "infranodus.md", "sim": 0.61, "linked": true}
  ],
  "direct": [{"page": "graphrag.md", "sim": 0.53}],
  "holes":  [{"a": "fix-llm-wiki-with-knowledge-graph.md", "b": "graphrag.md", "sim": 0.5}]
}
```

Human output lists the seed's existing links, its ranked neighbors marked
`linked`/`GAP`, then the direct gaps (and holes, if requested).

## Evidence

A public-interface prototype (`okftool graph --format json` + `qmd vsearch --json`
per seed) run against the OKF wiki bundle:

- **Real find** — `gaps content-gap-analysis.md` surfaced **`graphrag.md`
  (sim 0.53)** as a near-but-unlinked concept: both are about knowledge graphs,
  for opposite purposes (retrieval vs. finding absences). A sensible bridge that
  did not exist.
- **Low false positives** — `gaps jujutsu.md` came back essentially clean: every
  strong neighbor was already linked (the jj cluster is dense). The tool does not
  cry wolf on well-connected clusters.
- **Why the filters exist** — `index.md` appeared as a "gap" for every seed
  (~0.51–0.53), motivating the reserved-file filter; a `Person` hub
  (`andrej-karpathy.md`) inflated the neighborhood-hole pass, motivating hub
  handling.
- **Calibration** — similarity bands were informative: *intra-cluster* ~0.60–0.77,
  *cross-topic adjacency* (the interesting gaps) ~0.45–0.55. A `τ` around 0.4 with
  the reserved/hub filters worked better than tuning `τ` alone.
- **Cost** — full run (direct + holes) on one seed = 1 + ~10 qmd queries; direct
  only = 1. Confirms the seed-driven complexity.

## Relationship to OKF203

`gaps` is the mirror image of the near-duplicate rule — same qmd similarity
primitive, opposite filter plus a link-graph join:

| | qmd similarity | link check | verdict |
|---|---|---|---|
| `OKF203` near-duplicate | very high | — | maybe **merge** |
| `gaps` | high-ish | **not linked** | maybe **connect** |

So it should reuse the existing qmd integration (`internal/qmd`, already used by
`OKF203`/`OKF204`) and the bundle link graph (already built for `okftool graph` /
`OKF201`).

## Implementation notes

- **Vector source: prefer the public qmd path.** Query qmd for a seed's neighbors
  rather than reading `.qmd/index.sqlite` directly — the index is a rebuildable
  cache whose schema is not a stable interface. (For reference, exact page vectors
  *can* be pulled from `documents(collection,path,hash)` → `content_vectors(hash,
  seq)` → `vectors_vec(hash_seq, embedding float[768], cosine)` and mean-pooled,
  if exact centroids ever matter — but that couples to qmd internals.)
- **Chunk→page aggregation** — `max` over a page's chunk hits (mean is an
  alternative).
- **Query expansion** — qmd's vector search may expand the query with a model;
  for a symmetric, LLM-free similarity, disable expansion if qmd exposes it, or
  accept the mild enrichment (cost is one call for the direct pass).

## Integration & amortization

Never a global batch. Two natural drivers:

- **After ingest** — run `gaps` on the pages just created/changed (they are the
  natural seeds; their neighborhoods are where new gaps appear). This sweeps the
  whole bundle over time at O(1) per interaction — the compounding-wiki spirit.
- **On demand** — an MCP tool / agent call ("gaps for X") while working on a
  concept.

## Non-goals

- **Not a lint rule.** No pass/fail; advisory only. (A whole-bundle `OKF2xx`
  variant could exist, but the seed-driven command is the primary, cheap shape.)
- **Not clustering the whole bundle.**
- **Not writing the bridge.** `gaps` reports candidate pairs; composing the
  cross-link or synthesis page is the agent's job.

## Open questions

1. Default `--depth`: `direct` (1 query, clean) vs `neighborhood` (richer, needs
   the hub filter to be worth it).
2. Ship a global `OKF2xx` variant too, or keep it command-only?
3. Default `τ` and `k`; whether to disable qmd expansion by default.
4. Hub handling: exclude `Person`/high-degree nodes, down-weight, or leave to
   `--exclude-types`.
