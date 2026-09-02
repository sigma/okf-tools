# okf-tools in CI

A guide for a repository that **holds an OKF bundle** and wants CI to check it, publish
it, or both. It covers installing the CLIs, the lint gate, publishing to each backend
(including the Google auth setup, which is the fiddly part), the operational patterns
worth copying, and the errors whose text points nowhere near their cause.

If you only want the one-screen version, the README's
[`okftool`](../README.md#in-ci-without-nix) and [`okfpub`](../README.md#in-ci-without-nix-1)
sections have it.

## Installing the CLIs

Two composite actions download a released binary onto `PATH`, verify its SHA-256, and
need no Nix:

```yaml
- uses: sigma/okf-tools/actions/setup-okftool@v0   # linting
- uses: sigma/okf-tools/actions/setup-okfpub@v0    # publishing
```

Both take an optional `install-dir` and expose the installed `version` as an output.
Runners: `ubuntu-*` and `macos-*` (linux/darwin × amd64/arm64). Windows is not supported.

> **Pin a released tag, never a branch.** The version each action installs is baked into
> its `action.yml` and rewritten by the release workflow. On `main` that marker is a
> `0.0.0` placeholder with nothing to download, so `@main` fails. `@v0` floats to the
> latest `v0.x`; `@v0.4.1` pins exactly.

Both CLIs ship lockstep from one release tag, so use the same tag for both.

## The lint gate

### Using the `okf-lint` action

The uniform half of a lint job — install, lint, annotate — is packaged as a composite
action:

```yaml
name: okf
on: [push, pull_request]

jobs:
  lint:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      security-events: write     # only needed when sarif is left on
    steps:
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/okf-lint@v0
        with:
          bundle: docs           # bundle root; default "."
          fail-on: error         # error | warning; default error
          sarif: true            # annotate the PR; default true
```

Inputs: `bundle`, `fail-on`, `sarif`, and `category` (the code-scanning category, worth
setting when one repository lints several bundles so their findings stay apart).

It installs `okftool` **only when it is not already on `PATH`**, so a job that ran
`setup-okftool` itself — to pin an exact version, or to lint several bundles without
downloading twice — is not made to download it again:

```yaml
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/setup-okftool@v0.4.1   # exact version
      - uses: sigma/okf-tools/actions/okf-lint@v0
        with: { bundle: docs, category: docs }
      - uses: sigma/okf-tools/actions/okf-lint@v0
        with: { bundle: handbook, category: handbook }
```

**Which `okftool` it installs.** The one released at the same tag: `okf-lint@v0.4.1`
installs `okftool` 0.4.1, and `okf-lint@v0` tracks the latest `v0.x` binary — the same
lockstep the setup actions promise. Pinning the action therefore pins the linter, so a
rule added in a later release cannot start failing your build without you moving the tag.
To decouple them, install `setup-okftool` at the version you want first, as above; the
action will use whatever is already on `PATH`.

> **`permissions` belongs on the calling job.** An action cannot grant itself more than
> the job has, so omitting `security-events: write` fails at the *upload* step rather than
> at the lint step — a confusing place to fail.

> **On a private repository, code scanning needs GitHub Advanced Security.** Without it
> the upload cannot succeed, so the action treats it as non-fatal: a bundle that lints
> clean will not go red because its findings could not be uploaded. Set `sarif: false` to
> skip the attempt entirely.

It is a composite action rather than a reusable workflow on purpose. A bundle repository
is checking out anyway, so a reusable workflow would spawn a second job with a second
checkout, and could not be combined with the other steps a consumer wants in the same job
— building a site, publishing after linting. Everything this repo ships for CI is
therefore an action under `actions/`, referenced one way.

### Writing it yourself

```yaml
- uses: actions/checkout@v4
- uses: sigma/okf-tools/actions/setup-okftool@v0
- run: okftool lint --bundle docs --fail-on error
```

What to know about the exit code: `lint` exits non-zero at **`--fail-on`** severity or
above, which defaults to `error`. A bundle with warnings and no errors therefore **passes**
by default — set `--fail-on warning` to gate on those too, or `--exit-zero` to report
without ever failing (useful when you want the SARIF annotations but not the gate).

`--format sarif` emits SARIF 2.1.0 for `github/codeql-action/upload-sarif`, so findings
appear inline on the pull request rather than only in the job log. `--format json` is the
machine-readable form for anything else.

## Publishing

Both backends share the same shape: check out, install `okfpub`, set the backend's
environment, run. What differs is authentication and what a run produces.

Two things worth deciding before either:

- **Trigger.** Publishing on every push to the default branch is the common case.
  Publishing from pull requests is usually wrong — a PR branch's content is not what the
  mirror should show.
- **Concurrency.** Two publishes racing each other against one destination will interleave
  writes. Guard it:

  ```yaml
  concurrency:
    group: okf-publish-${{ github.ref }}
    cancel-in-progress: false     # let a publish finish rather than half-writing
  ```

  `cancel-in-progress: false` matters: a cancelled publish leaves the destination
  partially written, and the next run has to reconcile it.

### Publishing to Notion

```yaml
jobs:
  publish:
    runs-on: ubuntu-latest
    concurrency:
      group: okf-publish-notion
      cancel-in-progress: false
    steps:
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/setup-okfpub@v0
      - run: okfpub run --bundle docs
        env:
          NOTION_TOKEN: ${{ secrets.NOTION_TOKEN }}
          NOTION_DB_ID: ${{ secrets.NOTION_DB_ID }}
```

**`NOTION_DB_ID` is a data-source id, not a database id.** Under the 2025-09-03 API a
database can host several data sources, and the id in a Notion URL is the wrong one.
Resolve it once:

```bash
curl -s https://api.notion.com/v1/databases/<ID-FROM-URL> \
  -H "Authorization: Bearer $NOTION_TOKEN" \
  -H "Notion-Version: 2025-09-03" | jq -r '.data_sources[].id'
```

Optional environment, all for the generated-page banner's deep-link back to source:

```
OKF_SOURCE_URL      repo web base (default: GITHUB_SERVER_URL/GITHUB_REPOSITORY)
OKF_SOURCE_REF      branch the /edit/ link targets (default: the git branch)
OKF_SOURCE_PREFIX   bundle root's path within the repo, e.g. docs
```

`OKF_SOURCE_PREFIX` is worth setting explicitly when the bundle is not at the repo root:
without it the deep-link omits that path segment and 404s. On Actions it is usually
inferred correctly from git, but a shallow or unusual checkout can defeat that.

`--interval` paces writes (default 350ms). Raise it if a large bundle is being throttled;
`--interval 0` disables pacing entirely.

### Publishing to Google Docs

There is **no secret to store**. Google organizations enforce
`constraints/iam.managed.disableServiceAccountKeyCreation` by default, so no
service-account key exists to put in one. CI mints a short-lived credential from GitHub's
OIDC token instead.

```yaml
jobs:
  publish:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write            # required to mint the OIDC token
    concurrency:
      group: okf-publish-gdocs
      cancel-in-progress: false
    steps:
      - uses: actions/checkout@v4
      - uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: projects/<NUM>/locations/global/workloadIdentityPools/<POOL>/providers/<PROVIDER>
          service_account: okfpub-gdocs@<PROJECT>.iam.gserviceaccount.com
      - uses: sigma/okf-tools/actions/setup-okfpub@v0
      - run: okfpub run --backend gdocs --bundle docs
        env:
          GDRIVE_FOLDER_ID: ${{ vars.GDRIVE_FOLDER_ID }}
          GDOCS_IMPERSONATE_SA: okfpub-gdocs@<PROJECT>.iam.gserviceaccount.com
```

Forgetting `id-token: write` is the most common failure, and its error message talks about
a missing token rather than a missing permission.

#### One-time Google setup

The destination — project, service account, shared drive — is provisioned by
[`scripts/provision-gdocs.sh`](../scripts/provision-gdocs.sh), which also verifies that the
identity can really write. Then bind **this repository** to that service account:

```bash
PROJECT=<your-project>; REPO=<owner>/<repo>
SA="okfpub-gdocs@${PROJECT}.iam.gserviceaccount.com"

gcloud iam workload-identity-pools create github --project "$PROJECT" --location global

gcloud iam workload-identity-pools providers create-oidc github --project "$PROJECT" \
  --location global --workload-identity-pool github \
  --issuer-uri https://token.actions.githubusercontent.com \
  --attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --attribute-condition "assertion.repository == '${REPO}'"

NUM=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
gcloud iam service-accounts add-iam-policy-binding "$SA" --project "$PROJECT" \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/${NUM}/locations/global/workloadIdentityPools/github/attributes/repository/${REPO}"
```

> **The `--attribute-condition` is load-bearing.** Without it, *any* GitHub repository —
> not just yours — can mint a token for this provider and publish into your drive. This
> fails open, silently, and nothing in a working pipeline will reveal it.

The target **must be a shared drive**. A service account has no storage quota and cannot
own files, so a folder in someone's My Drive fails at write time.

#### What a run produces

By default one Google Doc per area declared in `areas.json`, each a tab per page.
`--select <area|path>` narrows it, and is repeatable. Documents are found again by a hidden
Drive property rather than by title, so renaming one in the UI is safe and permanent.

A fan-out is **best-effort**: a failing document does not stop its siblings, the run prints
a per-selection summary, and it exits non-zero if any failed. So a red job may still mean
four of five documents updated — read the summary rather than assuming nothing happened.

## Patterns worth copying

### Dry-run on the pull request, publish on merge

`--dry-run` publishes nothing on either backend. Against Notion it exports the rendered
tree to disk; against Google Docs it dumps the API writes it would issue and does not even
create the destination. Either way a PR can show what a merge would do:

```yaml
jobs:
  preview:
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/setup-okfpub@v0
      - run: okfpub run --backend gdocs --dry-run --bundle docs
        env:
          GDRIVE_FOLDER_ID: ${{ vars.GDRIVE_FOLDER_ID }}
```

Note the Google dry run still *reads* the destination when it exists, so it diffs against
what is really there — which means it needs the same auth as a real publish. Drop the auth
step and it will report a first-publish plan instead of a diff.

### A scheduled drift sweep

A steady-state publish trusts stored state and is cheap. `--recompute` rebuilds each page's
fingerprint from the live destination, so it detects edits made *in* Notion and heals them.
It costs a read per node, so run it on a schedule rather than on every push:

```yaml
on:
  schedule:
    - cron: '17 4 * * 1'      # Mondays, off the hour
jobs:
  sweep:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: sigma/okf-tools/actions/setup-okfpub@v0
      - run: okfpub run --bundle docs --recompute
        env:
          NOTION_TOKEN: ${{ secrets.NOTION_TOKEN }}
          NOTION_DB_ID: ${{ secrets.NOTION_DB_ID }}
```

`--recompute` is a Notion concern. For Google Docs the two scan modes are the same
operation — one read returns every tab — so the flag changes nothing there.

### Reading the run summary

Every publish prints what it did, and the traffic line is what makes a slow run
diagnosable:

```
okfpub: published 12 node(s), 4 anchor(s) in 9 transaction(s) via gdocs backend
okfpub: 41 request(s), 0 retried after 429, 0 after 5xx
```

A request count far above the transaction count means per-request work nobody planned; a
high retry count means the service is throttling you, which `--interval` addresses on
Notion. An unchanged re-run should report **0 transactions** — if it does not, something is
re-publishing every time, and that is worth investigating rather than tolerating.

## Troubleshooting

Errors whose message points away from the cause.

| Symptom | Cause |
|---|---|
| Notion: `Could not find data source` or an empty publish | `NOTION_DB_ID` holds a **database** id copied from a URL, not a **data-source** id. See above. |
| gdocs: `403 storageQuotaExceeded` | The target is not a shared drive. A service account has no storage and cannot own files, so a My Drive folder can never work — the quota is not "full", it does not exist. |
| gdocs: `404` on a document or drive | The service account is not a member of the shared drive. Add it as **Content manager** (a `Contributor` can create and trash but not delete, which fails later in confusing ways). |
| gdocs: `ACCESS_TOKEN_SCOPE_INSUFFICIENT` | The credential carries `cloud-platform` but not Drive/Docs scope. Note `gcloud auth print-access-token --impersonate-service-account` **ignores `--scopes`**. |
| gdocs: cannot mint a token for the service account | The CI principal lacks `roles/iam.workloadIdentityUser` on it, or `iamcredentials.googleapis.com` is not enabled on the project. |
| gdocs: `Callers must accept Terms of Service` | The Google account has never opened the Cloud console. It is a one-time browser action; there is no CLI for it. |
| gdocs: key creation refused | Expected. `iam.managed.disableServiceAccountKeyCreation` is enforced by default — use impersonation and WIF rather than seeking an exception. Note the *legacy* `iam.disableServiceAccountKeyCreation` policy reads as unenforced while the managed one does the blocking. |
| Actions: `Unable to get ACTIONS_ID_TOKEN_REQUEST_URL` | The job is missing `permissions: id-token: write`. |
| The setup action downloads nothing | It is pinned to a branch. Use a released tag. |
| `okf-lint` fails at the SARIF upload | The calling job is missing `permissions: security-events: write`. On a private repo without GitHub Advanced Security, code scanning is unavailable — set `sarif: false`. |
| Nix consumers: `inconsistent vendoring` after a dependency change | A stale `vendorHash`. It fails this way rather than as a hash mismatch, because the fixed-output vendor derivation returns its old cached contents. |

## Reference

- [README](../README.md) — what the CLIs are and how to run them locally.
- [`docs/RULES.md`](RULES.md) — the lint rule catalog: ids, severities, what is autofixable.
- [`docs/okf.example.toml`](okf.example.toml) — annotated per-bundle configuration.
- [`scripts/provision-gdocs.sh`](../scripts/provision-gdocs.sh) — one-time Google setup.
