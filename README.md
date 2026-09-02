# okf-tools

Tooling for authoring and maintaining [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
(OKF) bundles.

## What's here

Two Go CLIs and a Nix flake:

- [**`okftool`**](#okftool) — lints, formats, and scaffolds OKF bundles.
- [**`okfpub`**](#okfpub) — publishes a bundle to a backend (Notion, Google Docs,
  or the filesystem for dry runs).
- a **dev shell** bundling [`qmd`](https://github.com/firefly-engineering/toolbox)
  (local hybrid search over markdown), with both CLIs on `PATH`.

The flake exposes each CLI as its own package and app:

```
nix run github:sigma/okf-tools#okftool -- lint
nix run github:sigma/okf-tools#okfpub  -- run --dry-run
```

Downstream projects consume the dev shell with:

```
use flake github:sigma/okf-tools
```

Both CLIs ship lockstep from the same release tag, and each is also installable
in CI without Nix — see [`setup-okftool`](#in-ci-without-nix) and
[`setup-okfpub`](#in-ci-without-nix-1).

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
worklist `OKF201`/`OKF202`/`OKF203`/`OKF206`, with autofix for the safe ones. Optional and
opt-in **extensions** (the `OKFEXT-*` namespace, off by default; `OKF0xx/1xx/2xx`
stays reserved for the spec): `OKFEXT-QMD-01`/`OKFEXT-QMD-02` (qmd-backed semantic
near-duplicate detection and index staleness, needs `qmd` on `PATH`),
`OKFEXT-GLOSSARY-*` (anchor-checked single-file glossaries), and
`OKFEXT-SCHEMA-01` (frontmatter validated against a closed `schema.json`). Not yet
built: the Claude Code hook wiring (a consuming-bundle artifact).

## `okfpub`

Publishes an OKF bundle to a backend — Notion, or [Google Docs](#publishing-to-google-docs)
— mirroring each page's content, its
frontmatter-derived properties, and its cross-references — concept links become
real links between published pages, and glossary-term citations resolve to
in-page anchors. Change detection is hash-based, so a re-run touches only the
pages whose source actually changed.

```
okfpub run [flags]   # publish the bundle to a backend
okfpub version       # print the version
```

Run flags:

```
--backend  notion|gdocs|fake|fs   (default notion)
--bundle   bundle root      (default ".")
--config   okf.toml path    (default: discovered)
--areas    areas.json path  (default: <root>/areas.json if present)
--schema   schema.json path (default: <root>/schema.json if present)
--out      fs/export output dir   (default: okfpub-export)
--select   area or path to publish as one document; repeatable (gdocs)
--dry-run  publish nothing: dump the API writes (gdocs) or export to the
           filesystem (implies --backend fs otherwise)
--recompute                 full live-block scan (true drift + self-heal)
```

`--dry-run` renders the whole pipeline to a local directory tree instead of
calling the backend — the same generation, optimization, and transport code path
that publishes to Notion, so it is a faithful preview. `--recompute` rebuilds each
live page's fingerprint from the backend rather than trusting stored state,
which detects out-of-band edits and re-heals them.

The Notion backend reads its credentials from the environment:

```
NOTION_TOKEN    Notion integration token
NOTION_DB_ID    Notion data-source id
OKF_SOURCE_URL  repo web base for the generated-page banner deep-link
                (default: GITHUB_SERVER_URL/GITHUB_REPOSITORY, else local git)
OKF_SOURCE_REF  branch the banner's /edit/ link targets (default: git branch, else main)

OKF_SOURCE_PREFIX
                bundle root's path within the repo, when the bundle is not at the
                repo root (e.g. docs for `--bundle docs`). Without it the banner
                deep-link omits that segment and 404s.
                (default: `git rev-parse --show-prefix`, else none)
```

> **`NOTION_DB_ID` is a data-source id, not a database id.** Under the
> 2025-09-03 API a database can host several data sources, so the id in a Notion
> URL is the wrong one. Resolve it with `GET /v1/databases/{id}` and read
> `.data_sources[]`.

Published pages carry a disclaimer banner linking back to the source file — on by
default, configurable in `okf.toml` (see
[`docs/okf.example.toml`](docs/okf.example.toml)).

### Publishing to Google Docs

`--backend gdocs` publishes a bundle to a **Google Doc**: one tab per page, in one
document per selection, inside a Google **shared drive**.

```
okfpub run --backend gdocs                          # one document per area
okfpub run --backend gdocs --select concepts        # just that area
okfpub run --backend gdocs --select concepts --select adr
okfpub run --backend gdocs --dry-run                # dump the API writes, touch nothing
```

`--select` names an **area key** from `areas.json` or a bundle-relative path, and is
repeatable. Omitting it fans out to one document per declared area; a bundle with no
`areas.json` publishes as a single whole-bundle document. The glossary area is appended
to every document as a trailing tab — so citations resolve inside the document — rather
than being published as a document of its own. Pages that no area claims are reported
and skipped. A failed document does not stop its siblings: the run publishes what it
can, prints a per-selection summary, and exits non-zero.

Each document is found again by a hidden Drive property, not by its title, so renaming
it in the UI is safe. Tabs are flat, titled from frontmatter, and an area's `README.md`
opens its document.

```
GDRIVE_FOLDER_ID       the SHARED DRIVE to publish into
GDOCS_IMPERSONATE_SA   the service account to impersonate
```

> **It must be a shared drive, not a folder in My Drive.** A service account has no
> storage quota and cannot own files, so publishing into a My Drive folder fails at
> write time with a misleading `403 storageQuotaExceeded`.

**There is no key file.** Google organizations enforce
`constraints/iam.managed.disableServiceAccountKeyCreation` by default, so no
service-account JSON key can be created. `okfpub` instead impersonates the service
account using whatever Application Default Credentials the environment already has — a
developer's `gcloud auth application-default login`, or a Workload Identity Federation
credential in CI. Both are short-lived, and neither is a secret to store.

[`scripts/provision-gdocs.sh`](scripts/provision-gdocs.sh) walks the one-time setup:
the Cloud project, the Docs/Drive/IAM-Credentials APIs, the service account and its
token-creator grant, the shared drive, and a verification write.

### In CI (without Nix)

`setup-okfpub` mirrors `setup-okftool`, with the same tag-pinning rules:

```yaml
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/setup-okfpub@v0
      - run: okfpub run --bundle path/to/bundle
        env:
          NOTION_TOKEN: ${{ secrets.NOTION_TOKEN }}
          NOTION_DB_ID: ${{ secrets.NOTION_DB_ID }}
```

For the Google Docs backend there is **no secret to store**: CI authenticates with
GitHub's OIDC token, exchanges it for short-lived Google credentials, and `okfpub`
impersonates the service account from there.

```yaml
jobs:
  publish:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write          # required to mint the OIDC token
    steps:
      - uses: actions/checkout@v4
      - uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: projects/<NUM>/locations/global/workloadIdentityPools/<POOL>/providers/<PROVIDER>
          service_account: okfpub-gdocs@<PROJECT>.iam.gserviceaccount.com
      - uses: sigma/okf-tools/actions/setup-okfpub@v0
      - run: okfpub run --backend gdocs --bundle path/to/bundle
        env:
          GDRIVE_FOLDER_ID: ${{ vars.GDRIVE_FOLDER_ID }}
          GDOCS_IMPERSONATE_SA: okfpub-gdocs@<PROJECT>.iam.gserviceaccount.com
```

The provider and its binding are created once, per consuming repo:

```bash
PROJECT=<your-project>; REPO=<owner>/<repo>
SA="okfpub-gdocs@${PROJECT}.iam.gserviceaccount.com"

gcloud iam workload-identity-pools create github --project "$PROJECT" --location global

gcloud iam workload-identity-pools providers create-oidc github --project "$PROJECT" \
  --location global --workload-identity-pool github \
  --issuer-uri https://token.actions.githubusercontent.com \
  --attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --attribute-condition "assertion.repository == '${REPO}'"   # scope it to ONE repo

NUM=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
gcloud iam service-accounts add-iam-policy-binding "$SA" --project "$PROJECT" \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/${NUM}/locations/global/workloadIdentityPools/github/attributes/repository/${REPO}"
```

The `--attribute-condition` is load-bearing: without it, **any** GitHub repository can
mint a token for this provider.

## Reference

- [`docs/DESIGN.md`](docs/DESIGN.md) — architecture, CLI surface, bundle/link
  model, workflow integration, roadmap, open questions.
- [`docs/RULES.md`](docs/RULES.md) — the canonical rule catalog (IDs, categories,
  severities, autofix).
- [`docs/okf.example.toml`](docs/okf.example.toml) — annotated per-bundle config
  schema.
