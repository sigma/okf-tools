# nested-bundle

A bundle that is **not** at the repo root: the bundle lives at `docs/`, and this
file sits above it as a repo-root artifact that is no part of the bundle.

Every other fixture here is its own bundle root, so the two coordinates — "repo
work tree" and "bundle root" — coincide and any code that conflates them still
passes. This fixture separates them, which is what makes it useful: a node's
bundle-relative path (`adr/0001-nested-bundles.md`) differs from its repo-relative
path (`docs/adr/0001-nested-bundles.md`).

Load it as `--bundle testdata/nested-bundle/docs`.

The banner is left at its default (enabled), so a publish run emits a
generated-page disclaimer whose `/edit/` deep-link must resolve against the
repo-relative path, not the bundle-relative one — see #124.

Tests drive that resolution with `OKF_SOURCE_PREFIX=docs`, which needs no
checkout: the local-git tier (`git rev-parse --show-prefix`) is covered by the
resolver's own tests through an injected git.
