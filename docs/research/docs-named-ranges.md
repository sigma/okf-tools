# Are Google Docs named ranges a durable per-tab identity marker?

Researched 2026-09-02 for [sigma/okf-tools#158](https://github.com/sigma/okf-tools/issues/158).

**Short answer: the API never documents that a named range survives deletion of
the content it covers — and Google's own sample code assumes it does not, by
re-creating the range after every replace.** Treat "IDENTITY marker created once,
never updated" as unsupported; the marker must be re-asserted in the same
`batchUpdate` that rewrites the tab.

## Sources

- REST reference: [`documents`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents),
  [`documents/request`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request),
  [`documents.get`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/get).
- Discovery document [`https://docs.googleapis.com/$discovery/rest?version=v1`](https://docs.googleapis.com/$discovery/rest?version=v1),
  revision **20260826**. All schema quotes below are verbatim from it and match
  the reference pages.
- Guide: [Work with named ranges](https://developers.google.com/workspace/docs/api/how-tos/named-ranges).
- Guide: [Structure of a Google Docs document](https://developers.google.com/workspace/docs/api/concepts/structure).
- Reference: [Usage limits](https://developers.google.com/workspace/docs/api/limits).
- Drive: [Add custom file properties](https://developers.google.com/workspace/drive/api/guides/properties),
  [Drive `File` resource](https://developers.google.com/workspace/drive/api/reference/rest/v3/files).

## 1. Survival under `deleteContentRange`

**Not documented — in either direction.** This is the central finding.

What *is* documented, in [`NamedRange`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#NamedRange):

> A named range is created with a single Range, and content inserted inside a
> named range generally expands that range. However, certain document changes can
> cause the range to be split into multiple ranges.

And in the guide:

> The indexes of the named range are automatically updated as content is added to
> and removed from the document.

Both sentences describe *adjustment*, not survival. Neither the reference nor the
guide states what happens when the entire covered span is deleted: whether the
range becomes zero-length, is garbage-collected, or persists. There is no
sentence anywhere in the reference or the guides saying a named range survives
deletion of all its content, and none saying it is removed.

[`DeleteContentRangeRequest.range`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#DeleteContentRangeRequest)
enumerates the collateral effects of a delete:

> Deleting text that crosses a paragraph boundary may result in changes to
> paragraph styles, lists, positioned objects and bookmarks as the two paragraphs
> are merged.

Named ranges are conspicuously **absent** from that list — which is evidence of
nothing either way, since the sentence is about paragraph merges, not about
whole-span deletion.

Two documented facts push against relying on survival:

1. The guide's own sample raises `"The named range is no longer present in the
   document."` when a lookup by name returns nothing — the sample treats
   disappearance as a reachable state.
2. The same sample **re-creates** the named range with `createNamedRange` after
   every delete+insert cycle. Google's canonical answer to "keep this label alive
   across a rewrite" is to re-create it, not to trust it.

Deleting a tab's *first character* specifically is not addressed anywhere. Note
also that a body's last newline cannot be deleted at all
([`DeleteContentRangeRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#DeleteContentRangeRequest)
lists "Deleting the last newline character of a Body" as an invalid request), so
a tab is never truly empty — but that surviving newline is at the *end*, not
where a first-character marker lives.

**Action for the design: assume no survival. Verify empirically against a live
document before shipping, and in either case re-assert the marker.**

## 2. Technique — keeping the marker alive across a full-body rewrite

**Documented and idiomatic: re-create the named range in the same `batchUpdate`.**
The [named-ranges guide](https://developers.google.com/workspace/docs/api/how-tos/named-ranges)
sample (Java and Python) builds one request list containing, per range:
`deleteContentRange` → `insertText` → `createNamedRange` over the newly inserted
span, then submits it as a single `batchUpdate` with
`writeControl.requiredRevisionId` set to the revision the document was read at.

Two consequences worth carrying into the design:

- Because names are not unique (§3), re-creating without deleting first will
  **accumulate** duplicate named ranges under the same name. Pair the re-create
  with [`DeleteNamedRangeRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#DeleteNamedRangeRequest)
  — "The name of the range(s) to delete. All named ranges with the given name
  will be deleted." — scoped by `tabsCriteria`, or delete by `namedRangeId`.
- `deleteNamedRange`'s `tabsCriteria` is documented as: "When omitted, the range
  deletion is applied to **all tabs**." Same default trap as
  `replaceNamedRangeContent`.

`replaceNamedRangeContent` as an alternative:
[`ReplaceNamedRangeContentRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#ReplaceNamedRangeContentRequest)

> Replaces the contents of the specified NamedRange or NamedRanges with the given
> replacement content. Note that an individual NamedRange may consist of multiple
> discontinuous ranges. In this case, only the content in the first range will be
> replaced. The other ranges and their content will be deleted.

Its `tabsCriteria` is documented: "When omitted, the replacement applies to **all
tabs**." Confirmed — the multi-tab default is dangerous.

It is also **unsuitable here for a stronger reason**: its `text` field is plain
text only. It cannot produce styled content, headings, tables or images, so it
cannot be the mechanism by which a page's body is republished. It is only viable
for a marker whose *content* is a short literal string.

Confirmed structural caveat from the guide:

> Note that named ranges specify a range of document content, but are not part of
> that content. If you extract content that includes a named range, then insert
> it at another location, the named range only points to the original content and
> not the duplicated section.

## 3. Limits

| Question | Answer | Source |
| --- | --- | --- |
| Max named ranges per document or per tab | **Not documented.** [Usage limits](https://developers.google.com/workspace/docs/api/limits) covers only request-rate quotas (3000 read/min/project, 600 write/min/project, etc.). No object-count limits appear anywhere in the reference or discovery doc. | absence |
| Max name length | **Documented: 1–256 UTF-16 code units.** `CreateNamedRangeRequest.name`: "Names do not need to be unique. Names must be at least 1 character and no more than 256 characters, measured in UTF-16 code units." | [`CreateNamedRangeRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#CreateNamedRangeRequest) |
| Are names unique | **Documented: no.** "A document can contain multiple named ranges with the same name, but every named range has a unique ID." `NamedRanges` is "A collection of all the NamedRanges in the document that share a given name." | [`NamedRange`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#NamedRange) |

So `okf:<path>` fits in 256 UTF-16 units for any realistic path, but a name is a
*grouping key*, not a primary key. The stable identifier is `namedRangeId`,
returned by [`CreateNamedRangeResponse`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/response#CreateNamedRangeResponse)
("The ID of the created named range"). Reconciliation logic must tolerate
`namedRanges[name].namedRanges` having length > 1.

## 4. Visibility

- **Per-tab on `documents.get`: yes, documented.**
  [`DocumentTab.namedRanges`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#DocumentTab)
  — "The named ranges in the document tab, keyed by name." The
  [tabs guide](https://developers.google.com/workspace/docs/api/how-tos/tabs)
  lists `document.namedRanges` among the legacy root-level fields, directing
  callers to `document.tabs[i].documentTab.namedRanges`. With
  `includeTabsContent=true`, `Document.tabs` is populated instead of the root
  content fields ([`documents.get`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/get)),
  so each tab's named ranges arrive per tab. The guide's own sample reads exactly
  `document.tabs[0].documentTab.namedRanges`.
- **Invisible to a human reader in the Docs UI: not found in the docs.** The only
  visibility statement is about *API* visibility, and it says the opposite of
  private: "Named ranges are not private. All applications and collaborators that
  have access to the document can see its named ranges."
  ([`NamedRange`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#NamedRange),
  repeated in the guide.) There is no rendered UI affordance for named ranges
  documented anywhere, but the docs never assert they are hidden.
- **Dropped by export (PDF, text, Markdown): not found in the docs.** Neither the
  Docs API reference nor the Drive `files.export` documentation says anything
  about named ranges in exported output. Since a named range is metadata over a
  span and "not part of that content" (guide), a marker leaking into an export
  would only be a concern if the marker were realised as *text*.

## 5. Alternatives for a durable per-tab identity marker

- **Tab ID (`TabProperties.tabId`).** Documented as "The immutable ID of the
  tab". It is the strongest durable handle in the model, but it is *assigned by
  Docs*, not chosen — it identifies the tab, not the source page, so it still
  needs an external or in-document map to recover the path.
- **Tab title (`TabProperties.title`).** "The user-visible name of the tab",
  writable via
  [`UpdateDocumentTabPropertiesRequest`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents/request#UpdateDocumentTabPropertiesRequest).
  Survives body rewrites trivially, but is user-visible and user-editable, so it
  is a lossy identity: a human renaming a tab breaks the match.
- **Bookmarks.** **Documented as unavailable.** The v1 data model exposes only
  [`BookmarkLink`](https://developers.google.com/workspace/docs/api/reference/rest/v1/documents#BookmarkLink)
  (a *reference* to a bookmark, for link targets). There is no `Bookmark`
  schema, no `createBookmark` request, and no bookmark field on
  `ParagraphElement` or `Paragraph` in discovery rev 20260826. Bookmarks cannot
  be created or enumerated through the API.
- **Zero-width marker in the text.** Fully under our control and readable via
  `documents.get`, but it *is* content: it lives inside the span
  `deleteContentRange` wipes, and it would appear in exports and in copy-paste.
  Trades one fragility for another.
- **Drive-level metadata (`appProperties`).** Private to the requesting app and
  invisible in the Docs UI, but per-**file**, not per-tab, and hard-limited:
  "Maximum of 124 bytes per property string (including both key and value)",
  "Maximum of 30 private properties per file from any one application"
  ([Add custom file properties](https://developers.google.com/workspace/drive/api/guides/properties)).
  Cannot hold a tab→path map for a document of more than a handful of tabs.

## Bottom line for the self-heal design

The design's premise — "created once over the tab's first character, never
updated" — has **no documented guarantee** behind it and is contradicted in
spirit by Google's own reference implementation. The cheap fix is to keep the
marker but re-assert it: in the same `batchUpdate` as the rewrite, issue
`deleteNamedRange` (scoped with `tabsCriteria`) + `deleteContentRange` +
`insertText`/content + `createNamedRange`, under
`writeControl.requiredRevisionId`. That is idempotent, avoids duplicate-name
accumulation, and does not depend on undocumented survival semantics. Before
committing, run a live experiment: create a range over a tab's first character,
delete the whole body, and read the tab back with `includeTabsContent=true`.
