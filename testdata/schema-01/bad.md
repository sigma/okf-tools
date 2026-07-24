---
title: A Dirty Page
type: idea
status: seed
author: someone
created: not-a-date
tags: [gamma]
description: A page with three schema violations.
---

Three violations: `status: seed` is not one of the schema options, `author` is
an undeclared key, and `created` is not a date. `type` carries a derived-column
value and is ignored regardless.
