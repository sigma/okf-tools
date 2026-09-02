# Service-account Google Drive semantics

Research note for [sigma/okf-tools#149](https://github.com/sigma/okf-tools/issues/149).

Scope of the question: a service account, driven from CI, creates a Google Doc in a
configured Drive folder; a later run must find *that same* Doc again by a hidden
`appProperties` key and rewrite it in place.

All claims below cite Google's own Drive API / Workspace documentation. Where the
primary docs are silent, that is stated explicitly rather than filled in.

---

## 1. Storage quota: the target folder must live in a shared drive

This is the load-bearing finding for provisioning.

> "Service accounts don't have storage quota and can't own any files. Instead, they
> must upload files and folders into shared drives, or use OAuth 2.0 to upload items
> on behalf of a human user."
>
> — [Resolve errors — `storageQuotaExceeded`](https://developers.google.com/workspace/drive/api/guides/handle-errors)

The identical sentence is repeated in the shared-drives overview:
[Shared drives overview](https://developers.google.com/workspace/drive/api/guides/about-shareddrives).

Consequences:

- A folder in a **human's My Drive**, shared with the service account as writer, is
  **not** a viable target. Creating a file there makes the *creator* the owner
  ([Files and folders overview](https://developers.google.com/workspace/drive/api/guides/about-files):
  "The user is the primary owner of this folder"), and a service account cannot own
  files, so the create fails.
- A **shared drive** works because "Files in a shared drive belong to the
  organization, not an individual user. They count against the organization's pooled
  storage, not against an individual member's personal storage quota."
  ([Shared drives overview](https://developers.google.com/workspace/drive/api/guides/about-shareddrives))

### Exact error

HTTP `403`, error `reason: storageQuotaExceeded`, message *"The user's Drive storage
quota has been exceeded."*
([Resolve errors](https://developers.google.com/workspace/drive/api/guides/handle-errors))

Note the message is misleading in this case — it fires even though the service
account's Drive is empty, because the account has *no* quota at all rather than a
full one.

### Does domain-wide delegation change the answer?

Yes. With domain-wide delegation the service account impersonates a real user via
the `sub` claim in the JWT, and "The application then acts with the permissions of
that specific user"
([Using OAuth 2.0 for Server to Server Applications](https://developers.google.com/identity/protocols/oauth2/service-account)).
The file is then created and owned by the impersonated human, consuming *their*
quota — which is exactly the "use OAuth 2.0 to upload items on behalf of a human
user" escape hatch named in the errors guide.

Trade-off: DWD is a domain-admin-granted, tenant-wide capability, and it makes the
Doc owned by an individual (a leaver/offboarding hazard). A shared drive avoids both.

**Recommendation: provision a shared drive, add the service account as a member with
at least `writer` (`contentManager`), and configure its folder as the publish
target.** Shared-drive roles are `organizer`, `fileOrganizer`, `writer`, and
"The `owner` role is not permitted in shared drives"
([Shared drives overview](https://developers.google.com/workspace/drive/api/guides/about-shareddrives)).

### Ownership and transfer

- In a shared drive there is no per-file owner; the organization owns the content
  (above).
- Ownership transfer *to* a service account is impossible: "Service accounts don't
  have storage quota in Drive, so ownership transfers to service accounts will fail."
  ([Transfer file ownership](https://developers.google.com/workspace/drive/api/guides/transfer-file))
- Ownership transfer of shared-drive content is not a thing either: "ownership
  transfers are not supported for files and folders in shared drives." (same page)
- My Drive transfers only work "from one Google Workspace account to another account
  in the same organization"; the previous owner is downgraded to `writer`. (same page)

---

## 2. `appProperties` as the idempotency key

### What they are

> "To add properties visible to all applications, use the `properties` field of the
> `files` resource. To add properties restricted to your app, use the
> `appProperties` field of the `files` resource."
>
> — [Add custom file properties](https://developers.google.com/workspace/drive/api/guides/properties)

`appProperties` is the right choice for a hidden publish-marker: it is private to the
OAuth client that wrote it, and it does not appear in the Drive UI at all — "The
Google Drive UI doesn't include a built-in way for you to edit custom properties."
(same page)

Shape (v3):

```json
"appProperties": {
  "additionalID": "ID"
}
```

### Setting them

`appProperties` is a writable field on the `files` resource, so it can be supplied in
the `files.create` request body alongside `name`, `mimeType`, and `parents` — the
properties guide documents the update path
([`files.update`](https://developers.google.com/workspace/drive/api/guides/properties))
and the create path is the same field on the same resource
([`files.create`](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/create)).
Setting it at creation is one round-trip; setting it afterwards leaves a window where
a crashed run orphans an unmarked Doc. **Set it at creation.**

Deleting a property is `files.update` with the key set to `null`.

### Limits

From [Add custom file properties](https://developers.google.com/workspace/drive/api/guides/properties):

- Max **100** custom properties per file, from all sources.
- Max **30** public properties per file, from all sources.
- Max **30** private properties per file from any one application.
- Max **124 bytes per property string, key + value combined**, in UTF-8.

That 124-byte budget is the real constraint: a key like `okf.pageId` (10 bytes)
leaves 114 bytes for the value. A slug or a UUID fits; a full wiki path may not.

### The lookup query

Search terms ([Search query terms and operators](https://developers.google.com/workspace/drive/api/guides/ref-search-terms)):

- `appProperties` uses the `has` operator: `appProperties has {key='department' and value='sales'}`
- `parents` uses the `in` operator: `'folder_id' in parents`
- terms combine with `and` ([Search for files and folders](https://developers.google.com/workspace/drive/api/guides/search-files))

So the scoped lookup is:

```
q = appProperties has {key='okfPageId' and value='PAGE_ID'}
    and 'FOLDER_ID' in parents
    and trashed = false
```

`'FOLDER_ID' in parents` matches **direct children only**, so if the publisher ever
nests subfolders the parent clause must be dropped (or the search widened to the
shared drive via `driveId`) or previously-published Docs become invisible and get
duplicated.

Private properties can only be read "through an authenticated request that uses an
access token obtained with an OAuth 2.0 client ID. You cannot use an API key."
(properties guide) — fine for a service account, which is an OAuth client.

---

## 3. Scopes

Canonical descriptions
([Choose Google Drive API scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)):

- `.../auth/drive.file` (non-sensitive): "Create new Drive files, or modify existing
  files, that you open with an app or that the user shares with an app while using
  the Google Picker API or the app's file picker."
- `.../auth/drive` (restricted): "View and manage all your Drive files."

The user-facing consent string for `drive.file` is "See, edit, create, and delete
**only the specific Google Drive files you use with this app**"
([OAuth 2.0 Scopes for Google APIs](https://developers.google.com/identity/protocols/oauth2/scopes)).

Method requirements:

- [`files.create`](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/create):
  `drive`, `drive.appdata`, or `drive.file`.
- [`files.update`](https://developers.google.com/workspace/drive/api/reference/rest/v3/files): same family.
- [`files.list`](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/list):
  `drive`, `drive.file`, `drive.metadata.readonly`, `drive.readonly`, and others.

So `drive.file` nominally covers create + update + list.

### Is `drive.file` access durable across runs? (unresolved in the primary docs)

**The Drive documentation does not state a duration or expiry for per-file access.**
Both [Choose scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)
and [Authentication and authorization overview](https://developers.google.com/workspace/drive/api/guides/about-auth)
describe the grant purely structurally — access to files the app "opens or creates" —
with no statement that it lapses when the token does. The only lifetime statement on
those pages is about *tokens*: "Because access tokens are short-lived, you must use
refresh tokens for long-term access." That is a credential lifetime, not a per-file
grant lifetime, and a service account mints a fresh token every run anyway.

Reading the model as designed (per-file association keyed to the OAuth client, not to
a session), a Doc created by the service account in run N should still be visible to
the same service account in run N+1, and should therefore appear in the
`appProperties` `files.list` query. But this is inference, not a documented guarantee,
and it is the single riskiest assumption in the design.

Mitigations, in order of preference:

1. **Use the full `drive` scope.** A service account with domain-wide delegation or
   shared-drive membership is already a trusted first-party integration, not a public
   app facing OAuth verification, so the "restricted scope" cost that motivates
   `drive.file` largely does not apply. `drive` makes the re-find unconditional.
2. Persist the created file ID in the publish state file, and treat the
   `appProperties` search as a *recovery* path rather than the primary lookup. The
   repo already records publish state per node, so this is cheap.
3. If `drive.file` is kept, add an integration test that creates, drops the token,
   re-authenticates, and re-finds — proving durability against the live API rather
   than against the docs.

---

## 4. Sharing: how humans see the Doc

Permission inheritance does the work; no explicit `permissions.create` is needed when
publishing into a shared drive or a shared folder:

> "Permission lists for a folder propagate downward. All child files and folders
> inherit permissions from the parent." … "Inherited permissions cannot be removed or
> reduced on any item."
>
> — [Share files, folders, and drives](https://developers.google.com/workspace/drive/api/guides/manage-sharing)

So: grant humans access **once, on the shared drive (or the target folder)**, and
every Doc the service account creates under it is visible to them automatically. This
is materially better than per-file `permissions.create` calls, which cost an extra
write per publish and can hit sharing rate limits.

Use explicit `permissions.create` only for the exceptional case — e.g. exposing one
Doc to someone outside the drive's membership. It needs `type` (`user`, `group`,
`domain`, `anyone`) and `role`, plus `emailAddress` or `domain` (same page).

---

## 5. Shared-drive parameters on every call

> "The `supportsAllDrives=true` parameter informs Google Drive that your app is
> designed to handle files on shared drives."
>
> — [Implement shared drive support](https://developers.google.com/workspace/drive/api/guides/enable-shareddrives)

It must be set on `files.get`, `files.list`, `files.create`, `files.update`,
`files.copy`, `files.delete`, `changes.list`, `changes.getStartPageToken`, and all
`permissions` methods. Omitting it on `files.create` is the classic
"why did my shared-drive write fail" bug.

For the lookup call, the useful combinations are (same page):

| Parameters | Searches |
| --- | --- |
| `corpora=user` + `includeItemsFromAllDrives=true` | files the caller can access, both My Drive and shared drives |
| `corpora=drive` + `driveId=DRIVE_ID` + `includeItemsFromAllDrives=true` | everything in one named shared drive |
| `corpora=allDrives` + `includeItemsFromAllDrives=true` | caller's files plus all shared drives they belong to |

`corpora` values are `user`, `domain`, `drive`, `allDrives`; `driveId` is the "ID of
the shared drive to search"
([`files.list`](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/list)).

For a publish loop pinned to one drive, `corpora=drive` + `driveId` +
`includeItemsFromAllDrives=true` + `supportsAllDrives=true` is the narrowest correct
setting.

Other shared-drive constraints
([Shared drives overview](https://developers.google.com/workspace/drive/api/guides/about-shareddrives),
[Create and populate folders](https://developers.google.com/workspace/drive/api/guides/folder)):

- 500,000 items per shared drive, including folders, shortcuts, and trash.
- A file in a shared drive has exactly one parent; multi-parenting is unsupported
  (`parents` on create takes a single folder ID).
- A folder cannot be moved from My Drive into a shared drive via `addParents` —
  it returns `teamDrivesFolderMoveInNotSupported`. Relevant if the target folder is
  ever migrated: recreate, don't move.

---

## 6. Rate limits for a CI publish loop

From [Usage limits](https://developers.google.com/workspace/drive/api/guides/limits):

- 1,000,000 quota units per minute per project.
- 325,000 quota units per minute per user per project.
- Per-method costs: `files.get` 5, `files.list` **100**, `files.update` 50,
  download 200, other 5.

A publish loop is roughly `list (100) + create-or-update (50)` per page, so ~150
units per node — the per-user ceiling is thousands of pages a minute. Quota is not
the binding constraint; the write-side Docs/Drive per-user write throttling is.

Relevant errors ([Resolve errors](https://developers.google.com/workspace/drive/api/guides/handle-errors)):

- `403`/`429 rateLimitExceeded` — "use exponential backoff to retry the request".
- `403 userRateLimitExceeded` — the per-user limit; the guide explicitly suggests
  "using service accounts with domain-wide delegation via the `quotaUser` parameter"
  to spread load, plus exponential backoff.
- `403 dailyLimitExceeded` — a cap the project owner set; remove the "Queries per
  day" cap.

The existing read/write pacing in the publisher should be reused here; a single
service account is a single quota principal, so a wide CI fan-out serialises against
one `userRateLimitExceeded` bucket.

---

## Summary for provisioning

1. Create a **shared drive** (not a My Drive folder).
2. Add the service account as a member with `writer` or `contentManager`.
3. Grant the human audience access on the shared drive / target folder once —
   inheritance covers every published Doc.
4. Pass `supportsAllDrives=true` on every Drive call; use `corpora=drive` +
   `driveId` on the lookup.
5. Set `appProperties` in the `files.create` body; keep key+value under 124 bytes.
6. Prefer the `drive` scope, or persist file IDs, rather than betting the re-find on
   undocumented `drive.file` durability.
