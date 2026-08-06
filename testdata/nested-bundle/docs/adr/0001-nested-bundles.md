---
type: adr
title: Nested bundles
description: Why the bundle root is not the repo root.
timestamp: 2026-08-06
---

A repo that is primarily code keeps its knowledge bundle in `docs/` rather than
at the top level, so the bundle root and the repo work tree root are distinct.

Anything that maps a node back to its source file — a deep-link, a
self-describing path column — needs both coordinates, not just the
bundle-relative one. See [Publishing](../publishing.md).
